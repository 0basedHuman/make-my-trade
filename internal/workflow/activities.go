// internal/workflow/activities.go
//
// WHAT: Temporal activity implementations for the scheduled daily workflows.
//
// WHY:  Activities are the work units Temporal orchestrates. They use the same
//       pipeline logic as the HTTP handler but run on schedule (6:30 AM PT)
//       via Temporal cron instead of an HTTP POST.
//
// HOW:  RunDailyAnalysisActivity mirrors runPipeline() in api/handlers.go.
//       The Finnhub sentiment call is included here (with rate-limit spacing)
//       since Temporal handles retries and the 6:30 AM run is not user-facing.
//
// WHAT BREAKS: If ActivityDeps is nil (not injected in cmd/worker), all
//              activities return errors. The worker registers activities
//              via closures that capture the deps struct.
//
// VERIFY: After the first scheduled run (6:30 AM PT), check:
//   psql $DB_URL -c "SELECT trade_date, candidates_found, no_trade_today FROM daily_summaries ORDER BY trade_date DESC LIMIT 5;"

package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/makemytrade/config"
	claudeclient "github.com/yourname/makemytrade/internal/claude"
	"github.com/yourname/makemytrade/internal/execution"
	"github.com/yourname/makemytrade/internal/indicators"
	"github.com/yourname/makemytrade/internal/market"
	"github.com/yourname/makemytrade/internal/store"
	"github.com/yourname/makemytrade/internal/strategy"
)

// ActivityDeps holds shared dependencies for all activities.
// Injected at worker startup in cmd/worker/main.go.
type ActivityDeps struct {
	Pool    *pgxpool.Pool
	Cfg     *config.Config
	Alpaca  *market.AlpacaClient
	Finnhub *market.FinnhubClient
	FRED    *market.FREDClient
	Engine  *strategy.Engine
	Rules   *strategy.Rules
}

// RunDailyAnalysisActivity runs the full daily options analysis pipeline.
// This is the Temporal-managed version of POST /api/run-analysis.
func (d *ActivityDeps) RunDailyAnalysisActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	today := time.Now().In(loc)
	todayStr := today.Format("2006-01-02")
	scanTime := today.Format("15:04")

	log.Printf("schedule_daily_scan_started date=%s time=%s", todayStr, today.Format("15:04"))

	tickers, err := loadWatchlist(ctx, d.Pool)
	if err != nil {
		return "", fmt.Errorf("load watchlist: %w", err)
	}

	// Fetch price bars
	barsMap, _ := d.Alpaca.FetchDailyBars(tickers, today.AddDate(0, -12, 0), today, 300)
	spyBars := barsMap["SPY"]

	// VIX
	vixLevel, _, err := d.FRED.FetchLatestVIX()
	if err != nil {
		log.Printf("activity: VIX warning: %v", err)
		vixLevel = 20.0
	}

	// BTC ROC
	btcROC := 0.0
	btcBars, btcErr := d.Alpaca.FetchCryptoDailyBars("BTC/USD", today.AddDate(0, -2, 0), 60)
	if btcErr == nil && len(btcBars) >= 21 {
		btcCloses := indicators.Closes(btcBars)
		if roc, ok := indicators.ROCLast(btcCloses, 20); ok {
			btcROC = roc
		}
	}

	regime := strategy.Regime{VIX: vixLevel, BTCROC20: btcROC}
	regimeLabel := strategy.RegimeLabel(vixLevel, btcROC)
	earningsEvents, _ := d.Finnhub.FetchUpcomingEarnings(today, today.AddDate(0, 0, 7))

	// Prescreen symbols in parallel
	type symResult struct {
		ticker   string
		analysis strategy.SymbolAnalysis
		candID   string
	}

	var mu sync.Mutex
	var allResults []symResult
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // conservative — includes Finnhub rate-limit spacing

	for _, ticker := range tickers {
		bars, ok := barsMap[ticker]
		if !ok || len(bars) == 0 {
			continue
		}

		wg.Add(1)
		go func(t string, b []indicators.Bar) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			earningsRisk := market.HasEarningsWithin(earningsEvents, t, today, 5)

			// Fetch Finnhub sentiment (with rate-limit delay — 60 req/min limit)
			time.Sleep(time.Second)
			sentiment, _ := d.Finnhub.FetchSocialSentiment(t)

			a := d.Engine.Analyze(t, todayStr, b, regime, spyBars, earningsRisk, sentiment)

			dir := "bullish"
			if strings.Contains(strings.ToLower(a.SetupFamily), "bearish") {
				dir = "bearish"
			}
			var prevVol int64
			if len(b) > 0 {
				prevVol = int64(b[len(b)-1].Volume)
			}

			candID, upsertErr := store.UpsertCandidate(ctx, d.Pool, store.UpsertCandidateInput{
				TradeDate:       today.Truncate(24 * time.Hour),
				Ticker:          t,
				GateTrend:       a.GateTrend.Passed,
				GateMomentum:    a.GateMomentum.Passed,
				GateVolume:      a.GateVolume.Passed,
				GateVIX:         a.GateVIX.Passed,
				GateBTC:         a.GateBTC.Passed,
				GateRSI:         a.GateRSI.Passed,
				AllGates:        a.Eligible,
				ClosePrice:      a.ClosePrice,
				EMA20:           a.EMA20,
				EMA100:          a.EMA100,
				RSI14:           a.RSI14,
				MACDHist:        a.MACDHist,
				VolumeRatio:     a.VolumeRatio,
				VIXLevel:        vixLevel,
				BTCROC20:        btcROC,
				PatternName:     a.PatternName,
				PatternScore:    a.PatternScore,
				AntiPatterns:    a.AntiPatterns,
				RejectedByAnti:  a.RejectedByAnti,
				EntryLow:        a.EntryLow,
				EntryHigh:       a.EntryHigh,
				StopLoss:        a.StopLoss,
				Target1:         a.Target1,
				Target2:         a.Target2,
				RRRatio:         a.RRRatio,
				HoldDaysMin:     a.HoldDaysMin,
				HoldDaysBase:    a.HoldDaysBase,
				HoldDaysMax:     a.HoldDaysMax,
				RejectReason:    a.ScreenReason,
				CandidateStatus: a.CandidateStatus,
				SetupFamily:     a.SetupFamily,
				Direction:       dir,
				PrevDayVolume:   prevVol,
			})
			if upsertErr != nil {
				log.Printf("activity: upsert %s: %v", t, upsertErr)
			}

			mu.Lock()
			allResults = append(allResults, symResult{ticker: t, analysis: a, candID: candID})
			mu.Unlock()
		}(ticker, bars)
	}
	wg.Wait()

	// Fetch option chains for eligible symbols
	rules := d.Rules
	if rules == nil {
		rules = strategy.DefaultRules()
	}
	lf := rules.OptionsTranslation.LiquidityFilters

	chains := make(map[string][]market.OptionContract)
	for _, r := range allResults {
		if !r.analysis.Eligible {
			continue
		}
		contracts, chainErr := d.Alpaca.FetchOptionChain(r.ticker, r.analysis.ClosePrice, todayStr)
		if chainErr != nil {
			log.Printf("activity: option chain %s: %v", r.ticker, chainErr)
			contracts = []market.OptionContract{}
		}
		chains[r.ticker] = filterChainQuality(contracts, lf)
		time.Sleep(200 * time.Millisecond) // conservative rate limiting
	}

	// Build RuntimePayload
	var candidates []claudeclient.CandidateInput
	for _, r := range allResults {
		if !r.analysis.Eligible {
			continue
		}
		a := r.analysis
		contracts := chains[r.ticker]

		optionsStatus := "options_not_allowed"
		if len(contracts) > 0 {
			optionsStatus = "options_ready"
		}

		hwBase := a.HoldDaysBase
		if hwBase == 0 {
			hwBase = 10
		}

		candidates = append(candidates, claudeclient.CandidateInput{
			Ticker:         r.ticker,
			Price:          a.ClosePrice,
			Daily20EMA:     a.EMA20,
			Daily50EMA:     a.EMA50,
			RSI14:          a.RSI14,
			MACDHist:       a.MACDHist,
			RelativeVolume: a.VolumeRatio,
			PriorDayHigh:   a.PriorDayHigh,
			PriorDayLow:    a.PriorDayLow,
			TrendBias:      a.TrendBias,
			Sentiment:      a.Sentiment,
			EarningsRisk:   a.EarningsRisk,
			AntiPatterns:   a.AntiPatterns,
			Options:        contracts,
			// v2 fields
			SetupFamily:    a.SetupFamily,
			PatternScore:   a.PatternScoreInt,
			ReasonCodes:    a.ReasonCodes,
			HoldWindowBase: hwBase,
			BaseTarget:     a.BaseTarget,
			StretchTarget:  a.StretchTarget,
			OptionsStatus:  optionsStatus,
		})
	}

	// Call Claude
	systemPrompt := claudeclient.BuildSystemPrompt()
	claudeCli := claudeclient.NewClient(d.Cfg.AnthropicAPIKey, "claude-sonnet-4-6", d.Cfg.ClaudeMaxOutputTokens, systemPrompt)

	candidateCount := 0
	var watchTickers []string

	if len(candidates) > 0 {
		payload := claudeclient.RuntimePayload{
			ScanTimePT: scanTime,
			MarketContext: claudeclient.MarketContext{
				VIX:           vixLevel,
				BTCRoc20:      btcROC,
				MacroNewsBias: "neutral",
			},
			OpenPositions: []claudeclient.PositionInput{},
			Candidates:    candidates,
		}

		decision, claudeErr := claudeCli.DecideOptions(payload)
		if claudeErr != nil {
			log.Printf("activity: Claude error: %v", claudeErr)
		} else {
			// Build per-ticker decision map
			decisionByTicker := make(map[string]*claudeclient.CandidateDecision)
			for i := range decision.Candidates {
				cd := &decision.Candidates[i]
				decisionByTicker[cd.Ticker] = cd
			}

			// Persist decisions
			for _, r := range allResults {
				if !r.analysis.Eligible || r.candID == "" {
					continue
				}
				cd, ok := decisionByTicker[r.ticker]
				if !ok {
					continue
				}
				decisionJSON, _ := json.Marshal(cd)
				if persistErr := store.UpdateCandidateClaudeReview(ctx, d.Pool, r.candID,
					cd.FinalDecision, float64(cd.Score)/100.0, string(decisionJSON)); persistErr != nil {
					log.Printf("activity: persist decision %s: %v", r.ticker, persistErr)
				}
				switch cd.Status {
				case "entry_ready", "confirmed":
					candidateCount++
				case "structural_candidate":
					watchTickers = append(watchTickers, r.ticker)
				}
			}
		}
	}

	noTrade := candidateCount == 0
	noTradeReason := ""
	if noTrade {
		noTradeReason = "No symbols reached entry_ready or confirmed status"
	}

	store.UpsertDailySummary(ctx, d.Pool, store.DailySummary{
		TradeDate:         today.Truncate(24 * time.Hour),
		VIXLevel:          vixLevel,
		BTCROC20:          btcROC,
		RegimeLabel:       regimeLabel,
		SymbolsScanned:    len(allResults),
		CandidatesFound:   candidateCount,
		NoTradeToday:      noTrade,
		NoTradeReason:     noTradeReason,
		RegimeSummary:     fmt.Sprintf("VIX %.1f | BTC ROC %.1f%% | %s", vixLevel, btcROC, regimeLabel),
		WatchTickers:      watchTickers,
		AnalysisCompleted: true,
	})

	return fmt.Sprintf("scanned=%d candidates=%d watchlist=%d no_trade=%v",
		len(allResults), candidateCount, len(watchTickers), noTrade), nil
}

// RunOpeningConfirmationActivity evaluates the first 10 minutes of trading for
// every entry_ready candidate. v7: Claude is the final authority at opening time.
//
// WHAT:  Runs after 6:40 AM PT on the trade date, once opening bars are available.
// WHY:   Separates structural setup quality (overnight analysis) from live open
//
//	behavior. Deterministic signals provide evidence; Claude makes the call.
//
// HOW:
//  1. Load entry_ready candidates for today.
//  2. Fetch 1-min bars (6:30–6:40 AM PT) for all tickers + SPY in one batch.
//  3. Shortlist top N by score (strategy.TradeFrequency config).
//  4. For each shortlisted candidate:
//     a. Run deterministic confirmation as evidence (not authority).
//     b. If hard block fired → mark watch_only directly (Claude cannot override).
//     c. Fetch option chain, filter for quality, select best contract.
//     d. If no qualifying contract → watch_only (nothing to confirm).
//     e. Populate ConfirmationCandidate with actual contract details.
//  5. Fetch market context (VIX, BTC ROC20, SPY trend) for Claude payload.
//  6. Call Claude.ConfirmEntry with full payload (candidates + market context).
//  7. For Claude CONFIRM (confidence >= min): buy the SAME contract Claude saw
//     via execution.BuyOptionPosition (DB-first → Alpaca order).
//  8. For Claude REJECT → watch_only.
//  9. Non-shortlisted entry_ready → watch_only (didn't pass score bar).
//  10. Update daily_summaries.candidates_confirmed.
//
// WHAT BREAKS: If bars are unavailable, deterministic signals default to failing.
//
//	Claude sees this in the evidence and will likely reject. The system is safe
//	to the conservative side when data is absent.
func (d *ActivityDeps) RunOpeningConfirmationActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	tradeDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	marketOpen := time.Date(now.Year(), now.Month(), now.Day(), 6, 30, 0, 0, loc)

	log.Printf("schedule_opening_confirmation_started date=%s time=%s opening_confirmation_window=06:30-06:40",
		tradeDate.Format("2006-01-02"), now.Format("15:04"))

	// ── Staleness guard ──────────────────────────────────────────────────────────
	// The first-10-minute candle evidence is only meaningful at open (6:30–6:40 PT).
	// If this activity runs late (retry storm, scheduler lag), reject it so stale
	// opening-candle logic is never used as a substitute for continuation context.
	cutoffHour, cutoffMin := 6, 55
	rules := d.Rules
	if rules == nil {
		rules = strategy.DefaultRules()
	}
	if rules.Schedule.OpeningConfirmationCutoff != "" {
		var h, m int
		if _, err := fmt.Sscanf(rules.Schedule.OpeningConfirmationCutoff, "%d:%d", &h, &m); err == nil {
			cutoffHour, cutoffMin = h, m
		}
	}
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), cutoffHour, cutoffMin, 0, 0, loc)
	if now.After(cutoff) {
		msg := fmt.Sprintf("opening_confirmation_stale: current PT time %s is past cutoff %02d:%02d — use continuation review instead",
			now.Format("15:04"), cutoffHour, cutoffMin)
		log.Printf("activity: %s", msg)
		return msg, nil
	}

	// ── 1. Load entry_ready candidates ──────────────────────────────────────────
	candidates, err := store.GetEntryReadyCandidates(ctx, d.Pool, tradeDate)
	if err != nil {
		return "", fmt.Errorf("load entry_ready candidates: %w", err)
	}
	if len(candidates) == 0 {
		log.Println("activity: no entry_ready candidates — skipping confirmation")
		return "confirmed=0 watch_only=0 no_entry_ready=true", nil
	}

	tickers := make([]string, 0, len(candidates)+1)
	for _, c := range candidates {
		tickers = append(tickers, c.Ticker)
	}
	tickers = append(tickers, "SPY")

	// ── 2. Fetch 1-min bars: 6:30–6:40 AM PT ────────────────────────────────────
	windowMinutes := rules.OpenConfirmation.ConfirmationWindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = 10
	}

	barsMap, fetchErr := d.Alpaca.FetchIntradayBars(tickers, marketOpen, windowMinutes)
	if fetchErr != nil {
		log.Printf("activity: intraday bars error: %v", fetchErr)
		barsMap = make(map[string][]indicators.Bar)
	}
	spyBars := barsMap["SPY"]

	// ── 3. Shortlist by score ────────────────────────────────────────────────────
	// ClaudeConf (stored 0-100) is the proxy for the overnight deterministic score.
	tf := rules.TradeFrequency
	maxShortlist := tf.MaxEntryReadyToConfirm
	if maxShortlist <= 0 {
		maxShortlist = 5
	}
	minScore := tf.MinEntryReadyScore
	if minScore <= 0 {
		minScore = 65
	}
	// candidates already ordered by claude_confidence DESC from store
	var shortlisted []store.Candidate
	for _, c := range candidates {
		if c.ClaudeConf >= minScore {
			shortlisted = append(shortlisted, c)
		}
		if len(shortlisted) >= maxShortlist {
			break
		}
	}

	// Candidates that didn't make the shortlist → watch_only immediately
	shortlistedIDs := make(map[string]bool, len(shortlisted))
	for _, c := range shortlisted {
		shortlistedIDs[c.ID] = true
	}
	watchOnlyCount := 0
	for _, c := range candidates {
		if !shortlistedIDs[c.ID] {
			if err := store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only"); err != nil {
				log.Printf("activity: mark watch_only %s: %v", c.Ticker, err)
			}
			watchOnlyCount++
			log.Printf("activity: %s → watch_only (below shortlist score bar %.0f, score=%.0f)",
				c.Ticker, minScore, c.ClaudeConf)
		}
	}

	if len(shortlisted) == 0 {
		log.Println("activity: no candidates passed shortlist score bar")
		return fmt.Sprintf("confirmed=0 watch_only=%d no_shortlisted=true", watchOnlyCount), nil
	}

	// ── 4-5. Deterministic evidence + chain selection BEFORE Claude call ────────
	// Contract must be selected before building the Claude payload so that Claude
	// can evaluate the actual option: strike, DTE, delta, spread, premium.
	// If no valid contract exists, the candidate is skipped (watch_only) — there
	// is nothing to confirm without a tradable contract.
	type candidateEvidence struct {
		cand        store.Candidate
		result      strategy.ConfirmationResult
		hardBlocked bool
		contract    *market.OptionContract // pre-selected contract (nil if none found)
		limitPrice  float64
		optionType  string
	}

	ccfg := rules.ClaudeConfirmation
	lf := rules.OptionsTranslation.LiquidityFilters
	todayStr := tradeDate.Format("2006-01-02")

	var forClaude []claudeclient.ConfirmationCandidate
	evidenceMap := make(map[string]candidateEvidence, len(shortlisted))

	for _, c := range shortlisted {
		// ── a. Deterministic opening signals (evidence only) ─────────────────────
		result := strategy.EvaluateConfirmation(strategy.ConfirmationInput{
			Ticker:        c.Ticker,
			Direction:     c.Direction,
			EntryLow:      c.EntryLow,
			EntryHigh:     c.EntryHigh,
			StopLoss:      c.StopLoss,
			PrevDayVolume: c.PrevDayVolume,
			Bars:          barsMap[c.Ticker],
			SPYBars:       spyBars,
		}, rules.OpenConfirmation)

		// Always persist the confirmation evidence row
		_ = store.UpsertTradeConfirmation(ctx, d.Pool, store.ConfirmationStoreInput{
			CandidateID:       c.ID,
			Ticker:            c.Ticker,
			TradeDate:         tradeDate,
			Status:            result.Status,
			SignalLevelHolds:  result.SignalLevelHolds,
			SignalOpenRange:   result.SignalOpenRange,
			SignalNoRejection: result.SignalNoRejection,
			SignalVolumeOK:    result.SignalVolumeOK,
			SignalMarketOK:    result.SignalMarketOK,
			SignalsPassed:     result.SignalsPassed,
			AutoRejected:      result.AutoRejected,
			AutoRejectReason:  result.AutoRejectReason,
			OpenPrice:         result.OpenPrice,
			First10High:       result.First10High,
			First10Low:        result.First10Low,
			First10Close:      result.First10Close,
			First10Volume:     result.First10Volume,
		})

		// Hard block: auto_reject fired AND config says it is non-overridable
		if result.AutoRejected && ccfg.DeterministicAutoRejectIsHardBlock {
			log.Printf("activity: %s → watch_only (hard_block: %s)", c.Ticker, result.AutoRejectReason)
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			evidenceMap[c.Ticker] = candidateEvidence{cand: c, result: result, hardBlocked: true}
			continue
		}

		// ── b. Fetch option chain and select best contract ───────────────────────
		// Contract selection MUST happen before the Claude payload is built.
		// Claude cannot make a real options confirmation without seeing the
		// actual contract: symbol, strike, DTE, delta, spread, premium.
		optionType := "call"
		if c.Direction == "bearish" {
			optionType = "put"
		}

		contracts, chainErr := d.Alpaca.FetchOptionChain(c.Ticker, c.ClosePrice, todayStr)
		if chainErr != nil {
			log.Printf("activity: %s → watch_only (chain_fetch_error: %v)", c.Ticker, chainErr)
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			evidenceMap[c.Ticker] = candidateEvidence{cand: c, result: result, hardBlocked: false}
			continue
		}

		qualified := market.FilterChainQuality(contracts,
			lf.MinOpenInterest, lf.MinOptionVolume, lf.MaxBidAskSpreadPctOfMid)
		best := market.SelectBestContract(qualified, optionType)
		if best == nil {
			log.Printf("activity: %s → watch_only (candidate_skipped_no_valid_contract: type=%s chain=%d qualified=%d)",
				c.Ticker, optionType, len(contracts), len(qualified))
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			evidenceMap[c.Ticker] = candidateEvidence{cand: c, result: result, hardBlocked: false}
			continue
		}

		limitPrice := (best.Bid + best.Ask) / 2.0
		if best.Bid <= 0 {
			limitPrice = best.Ask
		}

		log.Printf("activity: contract_selected_before_claude: %s %s strike=%.2f dte=%d delta=%.3f mid=%.2f spread=%.1f%%",
			c.Ticker, best.Symbol, best.Strike, best.DTE, best.Delta, limitPrice, best.SpreadPct)

		// ── c. Build opening context from intraday bars ──────────────────────────
		var oc claudeclient.OpeningContext
		if bars := barsMap[c.Ticker]; len(bars) > 0 {
			n := len(bars)
			oc.First10mClose = bars[n-1].Close
			if len(bars) >= 5 {
				oc.First5mClose = bars[4].Close
			}
			var hi, lo, volSum float64
			for _, b := range bars {
				if hi == 0 || b.High > hi {
					hi = b.High
				}
				if lo == 0 || b.Low < lo {
					lo = b.Low
				}
				volSum += b.Volume
			}
			oc.OpeningRangeHigh = hi
			oc.OpeningRangeLow = lo
			oc.OpeningVolume = volSum / float64(n)
			var vwapNumer float64
			for _, b := range bars {
				vwapNumer += b.Close * b.Volume
			}
			if volSum > 0 {
				oc.VWAP = vwapNumer / volSum
			}
		}

		// ── d. Build confirmation candidate with contract populated ──────────────
		softMinMet := result.SignalsPassed >= ccfg.DeterministicSignalsSoftMin

		var sigDetails []string
		if result.SignalLevelHolds {
			sigDetails = append(sigDetails, "level_holds")
		}
		if result.SignalOpenRange {
			sigDetails = append(sigDetails, "open_range_ok")
		}
		if result.SignalNoRejection {
			sigDetails = append(sigDetails, "no_rejection")
		}
		if result.SignalVolumeOK {
			sigDetails = append(sigDetails, "volume_ok")
		}
		if result.SignalMarketOK {
			sigDetails = append(sigDetails, "market_ok")
		}

		pc := claudeclient.ConfirmationCandidate{
			Ticker:      c.Ticker,
			SetupFamily: c.SetupFamily,
			Direction:   optionType,
			Daily: claudeclient.DailyContext{
				Close:        c.ClosePrice,
				EMA20:        c.EMA20,
				EMA100:       c.EMA100,
				RSI:          c.RSI14,
				MACDHist:     c.MACDHist,
				VolumeRatio:  c.VolumeRatio,
				FinalScore:   c.ClaudeConf,
				PriorDayHigh: c.EntryHigh,
				PriorDayLow:  c.EntryLow,
			},
			Opening: oc,
			// Contract is the selected option contract — critical for Claude's decision.
			Contract: claudeclient.ConfirmationContract{
				Symbol:       best.Symbol,
				Type:         best.Type,
				Strike:       best.Strike,
				Expiration:   best.Expiration,
				DTE:          best.DTE,
				Delta:        best.Delta,
				MidPrice:     limitPrice,
				BidAskSpread: best.SpreadPct,
				OpenInterest: best.OpenInterest,
				OptionVolume: best.OptionVolume,
			},
			Risk: claudeclient.RiskContext{
				EntryPrice:       limitPrice,
				StopLossPct:      c.StopLoss,
				BaseTargetPct:    c.Target1,
				StretchTargetPct: c.Target2,
				RRRatio:          c.RRRatio,
			},
			DeterministicSignals: claudeclient.DeterministicSignals{
				TrueCount:    result.SignalsPassed,
				TotalChecked: 5,
				SoftMinMet:   softMinMet,
				Details:      sigDetails,
			},
			HardBlocks: claudeclient.HardBlockSummary{
				IsClean: !result.AutoRejected,
			},
		}
		if result.AutoRejected {
			pc.HardBlocks.Fired = []string{result.AutoRejectReason}
		}

		forClaude = append(forClaude, pc)
		evidenceMap[c.Ticker] = candidateEvidence{
			cand:       c,
			result:     result,
			contract:   best,
			limitPrice: limitPrice,
			optionType: optionType,
		}
	}

	if len(forClaude) == 0 {
		log.Println("activity: all shortlisted candidates blocked or no valid contracts — skipping Claude call")
		return fmt.Sprintf("confirmed=0 watch_only=%d all_blocked=true", watchOnlyCount), nil
	}

	// ── 6. Build market context + call Claude for final confirmation ─────────────
	// Fetch VIX and BTC ROC20 so Claude can factor broad market state into the
	// confirm/reject decision. Both calls are best-effort — failure defaults to
	// safe neutral values rather than blocking the confirmation run.
	confirmVIX := 20.0
	if vix, _, err := d.FRED.FetchLatestVIX(); err == nil {
		confirmVIX = vix
	} else {
		log.Printf("activity: confirmation VIX fetch warning: %v — using %.1f", err, confirmVIX)
	}

	confirmBTCROC := 0.0
	if btcBars, err := d.Alpaca.FetchCryptoDailyBars("BTC/USD", tradeDate.AddDate(0, -2, 0), 60); err == nil && len(btcBars) >= 21 {
		btcCloses := indicators.Closes(btcBars)
		if roc, ok := indicators.ROCLast(btcCloses, 20); ok {
			confirmBTCROC = roc
		}
	}

	// Derive SPY trend from first-5m bar relative to prior close.
	spyTrend := "neutral"
	if len(spyBars) > 0 {
		spyFirst := spyBars[0]
		if spyFirst.Close > spyFirst.Open*1.002 {
			spyTrend = "bullish"
		} else if spyFirst.Close < spyFirst.Open*0.998 {
			spyTrend = "bearish"
		}
	}

	claudeCli := claudeclient.NewClient(
		d.Cfg.AnthropicAPIKey,
		"claude-sonnet-4-6",
		d.Cfg.ClaudeMaxOutputTokens,
		"", // ConfirmEntry uses its own built-in system prompt
	)

	confirmPayload := claudeclient.EntryConfirmationPayload{
		MarketContext: claudeclient.MarketContext{
			VIX:           confirmVIX,
			BTCRoc20:      confirmBTCROC,
			SPXTrend:      spyTrend,
			MacroNewsBias: "neutral",
		},
		Candidates: forClaude,
	}
	log.Printf("claude_confirmation_time=%s candidates=%d vix=%.1f btc_roc=%.1f spy_trend=%s",
		time.Now().In(loc).Format("15:04"), len(forClaude), confirmVIX, confirmBTCROC, spyTrend)
	claudeResp, claudeErr := claudeCli.ConfirmEntry(confirmPayload)
	if claudeErr != nil {
		log.Printf("activity: Claude ConfirmEntry error: %v — defaulting all to watch_only", claudeErr)
		for _, c := range shortlisted {
			ev := evidenceMap[c.Ticker]
			if !ev.hardBlocked && ev.contract != nil {
				_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
				watchOnlyCount++
			}
		}
		return fmt.Sprintf("confirmed=0 watch_only=%d claude_error=true", watchOnlyCount), nil
	}

	log.Printf("activity: Claude ConfirmEntry returned %d decisions (regime=%s)",
		len(claudeResp.Decisions), claudeResp.Regime)

	// ── 7-8. Apply Claude decisions ──────────────────────────────────────────────
	minConfidence := ccfg.MinConfidence
	if minConfidence <= 0 {
		minConfidence = 0.65
	}

	confirmedCount := 0
	decisionMap := make(map[string]claudeclient.EntryConfirmationDecision, len(claudeResp.Decisions))
	for _, d2 := range claudeResp.Decisions {
		decisionMap[d2.Ticker] = d2
	}

	for _, c := range shortlisted {
		ev, ok := evidenceMap[c.Ticker]
		if !ok || ev.hardBlocked || ev.contract == nil {
			continue // already handled above
		}

		dec, hasDec := decisionMap[c.Ticker]
		isConfirm := hasDec && dec.Decision == "CONFIRM" && dec.Confidence >= minConfidence

		if !isConfirm {
			reason := "no_decision"
			if hasDec {
				reason = fmt.Sprintf("claude_reject(conf=%.2f)", dec.Confidence)
			}
			log.Printf("activity: %s → watch_only (%s)", c.Ticker, reason)
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			continue
		}

		// ── Claude confirmed — use the SAME contract Claude saw ──────────────────
		best := ev.contract
		limitPrice := ev.limitPrice
		// If Claude suggests a price and it's within 20% of mid, prefer it
		if hasDec && dec.LimitPrice > 0 && dec.LimitPrice < limitPrice*1.20 {
			limitPrice = dec.LimitPrice
		}

		log.Printf("activity: claude_confirmed_contract: %s %s conf=%.2f reason=%q limit=%.2f",
			c.Ticker, best.Symbol, dec.Confidence, dec.Reason, limitPrice)

		_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "confirmed")

		// Suppress duplicate: don't open a second position if one is already open
		if hasPos, _ := store.HasOpenPositionForTicker(ctx, d.Pool, c.Ticker); hasPos {
			log.Printf("activity: %s already has open position — skipping buy", c.Ticker)
			confirmedCount++
			continue
		}

		// ── 7b. Buy via shared execution service (DB-first, single lifecycle owner) ─
		// execution.BuyOptionPosition: CreatePaperPosition → PlaceOptionOrder →
		// UpdatePositionAlpacaOrderID → UpdatePositionOptionDetails
		buyResult, buyErr := execution.BuyOptionPosition(ctx, d.Pool, d.Alpaca, execution.BuyInput{
			CandidateID:    c.ID,
			Ticker:         c.Ticker,
			SetupFamily:    c.SetupFamily,
			OptionType:     ev.optionType,
			ContractSymbol: best.Symbol,
			LimitPrice:     limitPrice,
			StopLoss:       c.StopLoss,
			Target1:        c.Target1,
			Target2:        c.Target2,
		})
		if buyErr != nil {
			log.Printf("activity: buy option position %s: %v", c.Ticker, buyErr)
			confirmedCount++
			continue
		}

		_ = store.InsertPositionEvent(ctx, d.Pool, buyResult.PositionID, c.Ticker, "position_opened",
			limitPrice, map[string]any{
				"candidate_status": "confirmed",
				"setup_family":     c.SetupFamily,
				"contract":         best.Symbol,
				"strike":           best.Strike,
				"dte":              best.DTE,
				"delta":            best.Delta,
				"claude_conf":      dec.Confidence,
				"claude_reason":    dec.Reason,
			})
		log.Printf("activity: paper position created %s posID=%s contract=%s limit=%.2f orderID=%s",
			c.Ticker, buyResult.PositionID, best.Symbol, limitPrice, buyResult.AlpacaOrderID)
		confirmedCount++
	}

	// ── 10. Update daily_summaries.candidates_confirmed ─────────────────────────
	if _, err := d.Pool.Exec(ctx,
		`UPDATE daily_summaries SET candidates_confirmed=$2, updated_at=NOW() WHERE trade_date=$1`,
		tradeDate, confirmedCount,
	); err != nil {
		log.Printf("activity: update summary confirmed count: %v", err)
	}

	summary := fmt.Sprintf("confirmed=%d watch_only=%d total_entry_ready=%d shortlisted=%d",
		confirmedCount, watchOnlyCount, len(candidates), len(shortlisted))
	log.Printf("activity: RunOpeningConfirmation done — %s", summary)
	return summary, nil
}

// RunPositionReviewActivity reviews all held paper positions.
//
// WHAT: Runs once per trading day (~12:45 PM PT, before market close).
// WHY:  Ensures every open position gets a daily HOLD/EXIT decision rather
//
//	than sitting unmonitored until expiration.
//
// HOW:
//  1. Load all open positions (with option_type / setup_family).
//  2. Fetch current price for each ticker via Alpaca latest quote.
//  3. Compute PnL% and days held.
//  4. Call Claude DecideOptions with OpenPositions list (no new candidates).
//  5. For each position: write position_reviews row.
//  6. Execute EXIT: close position + insert position_closed event.
func (d *ActivityDeps) RunPositionReviewActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	reviewDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	log.Printf("first_position_review_started date=%s time=%s", reviewDate.Format("2006-01-02"), now.Format("15:04"))

	// ── 1. Load open positions ───────────────────────────────────────────────
	positions, err := store.GetOpenPositionsForReview(ctx, d.Pool)
	if err != nil {
		return "", fmt.Errorf("load open positions: %w", err)
	}
	if len(positions) == 0 {
		log.Println("activity: no open positions — skipping review")
		return "reviewed=0", nil
	}

	// ── 2. Fetch current prices and compute PnL ──────────────────────────────
	type enriched struct {
		pos          store.ReviewablePosition
		currentPrice float64
		pnlPct       float64
		daysHeld     int
	}
	var items []enriched
	var posInputs []claudeclient.PositionInput

	for _, p := range positions {
		// ── Compute option P&L ───────────────────────────────────────────────────
		// Prefer option-level P&L (migration 000005): fetch current option mid-price
		// and compare to premium paid. This is correct for both calls and puts:
		//   (current_option_price - premium_paid) / premium_paid * 100
		//
		// Fallback for positions opened before migration 000005 (no option_symbol):
		// use underlying stock price delta — directionally inverted for puts but
		// better than nothing. These old positions should be exited naturally.
		pnlPct := 0.0
		currentPrice := 0.0 // used for exit records; option mid when available

		if p.OptionSymbol != "" && p.OptionPremium > 0 {
			// New path: option-level P&L.
			midPrice, midErr := d.Alpaca.FetchOptionMidPrice(p.OptionSymbol)
			if midErr != nil {
				log.Printf("activity: option mid-price %s (%s): %v — using stock fallback",
					p.Ticker, p.OptionSymbol, midErr)
				// Fall through to stock fallback below.
				stockPrice, quoteErr := d.Alpaca.FetchLatestQuote(p.Ticker)
				if quoteErr != nil {
					stockPrice = p.EntryPrice
				}
				currentPrice = stockPrice
				// For puts: stock down = option up. Approximate via negative of stock move.
				if p.EntryPrice > 0 {
					stockMovePct := (stockPrice - p.EntryPrice) / p.EntryPrice * 100.0
					if p.OptionType == "put" {
						pnlPct = -stockMovePct
					} else {
						pnlPct = stockMovePct
					}
				}
			} else {
				currentPrice = midPrice
				pnlPct = (midPrice - p.OptionPremium) / p.OptionPremium * 100.0
				log.Printf("activity: option P&L %s %s: mid=%.2f premium=%.2f pnl=%.1f%%",
					p.Ticker, p.OptionSymbol, midPrice, p.OptionPremium, pnlPct)
			}
		} else {
			// Legacy path: no option tracking — use underlying stock price.
			// Inverted for puts (stock falling = put winning).
			stockPrice, quoteErr := d.Alpaca.FetchLatestQuote(p.Ticker)
			if quoteErr != nil {
				log.Printf("activity: quote %s: %v — using entry price", p.Ticker, quoteErr)
				stockPrice = p.EntryPrice
			}
			currentPrice = stockPrice
			if p.EntryPrice > 0 {
				stockMovePct := (stockPrice - p.EntryPrice) / p.EntryPrice * 100.0
				if p.OptionType == "put" {
					pnlPct = -stockMovePct
				} else {
					pnlPct = stockMovePct
				}
			}
			log.Printf("activity: legacy P&L %s %s: stock=%.2f entry=%.2f pnl=%.1f%% (no option_symbol)",
				p.Ticker, p.OptionType, currentPrice, p.EntryPrice, pnlPct)
		}

		daysHeld := int(now.Sub(p.EntryDate).Hours() / 24)
		dte := 14 - daysHeld
		if dte < 0 {
			dte = 0
		}

		items = append(items, enriched{pos: p, currentPrice: currentPrice, pnlPct: pnlPct, daysHeld: daysHeld})
		posInputs = append(posInputs, claudeclient.PositionInput{
			Ticker:     p.Ticker,
			OptionType: p.OptionType,
			EntryPrice: p.OptionPremium, // send premium to Claude, not underlying price
			CurrentPnL: pnlPct,
			DTE:        dte,
			Status:     "open",
		})
	}

	// ── 3. Call Claude for review decisions ──────────────────────────────────
	vixLevel, _, _ := d.FRED.FetchLatestVIX()
	systemPrompt := claudeclient.BuildSystemPrompt()
	claudeCli := claudeclient.NewClient(d.Cfg.AnthropicAPIKey, "claude-sonnet-4-6", d.Cfg.ClaudeMaxOutputTokens, systemPrompt)

	payload := claudeclient.RuntimePayload{
		ScanTimePT:    now.Format("15:04"),
		MarketContext: claudeclient.MarketContext{VIX: vixLevel, MacroNewsBias: "neutral"},
		OpenPositions: posInputs,
		Candidates:    []claudeclient.CandidateInput{},
	}

	decision, claudeErr := claudeCli.DecideOptions(payload)
	reviewByTicker := make(map[string]claudeclient.PositionReview)
	if claudeErr != nil {
		log.Printf("activity: Claude review error: %v — defaulting all to HOLD", claudeErr)
	} else {
		for _, r := range decision.OpenPositionReview {
			reviewByTicker[r.Ticker] = r
		}
	}

	// ── 4-5. Persist reviews; execute EXIT actions ────────────────────────────
	reviewedCount, exitedCount := 0, 0

	for _, e := range items {
		p := e.pos
		action := "HOLD"
		rationale := "defaulted to HOLD — no Claude review"
		if r, ok := reviewByTicker[p.Ticker]; ok {
			action = mapPositionAction(r.Status)
			rationale = r.Reason
		}

		executed := false
		if action == "EXIT" {
			// Sell via shared execution service. Sell order must succeed before DB
			// is closed — if Alpaca rejects the sell, position stays open and an
			// event is recorded so the next review can retry.
			_, sellErr := execution.SellOptionPosition(ctx, d.Pool, d.Alpaca, execution.SellInput{
				PositionID:     p.ID,
				Ticker:         p.Ticker,
				ContractSymbol: p.OptionSymbol,
				SellPrice:      e.currentPrice, // 0 → SellOptionPosition fetches mid
				PnLPct:         e.pnlPct,
				ExitReason:     "review_exit",
			})
			if sellErr != nil {
				log.Printf("activity: sell option position %s: %v — keeping open", p.Ticker, sellErr)
				_ = store.InsertPositionEvent(ctx, d.Pool, p.ID, p.Ticker, "sell_failed",
					e.currentPrice, map[string]any{"error": sellErr.Error(), "pnl_pct": e.pnlPct})
			} else {
				_ = store.InsertPositionEvent(ctx, d.Pool, p.ID, p.Ticker, "position_closed",
					e.currentPrice, map[string]any{"reason": "review_exit", "pnl_pct": e.pnlPct})
				executed = true
				exitedCount++
				log.Printf("activity: closed position %s pnl=%.2f%%", p.Ticker, e.pnlPct)
			}
		}

		_ = store.UpsertPositionReview(ctx, d.Pool, store.PositionReviewInput{
			PositionID:      p.ID,
			Ticker:          p.Ticker,
			ReviewDate:      reviewDate,
			CurrentPrice:    e.currentPrice,
			PnLPctToday:     e.pnlPct,
			DaysHeld:        e.daysHeld,
			SuggestedAction: action,
			ActionRationale: rationale,
			ActionExecuted:  executed,
		})
		reviewedCount++
	}

	result := fmt.Sprintf("reviewed=%d exited=%d", reviewedCount, exitedCount)
	log.Printf("activity: RunPositionReview done — %s", result)
	return result, nil
}

// RunContinuationReviewActivity runs at 7:45 AM PT.
//
// WHAT:  Reviews open paper positions using fresh intraday bars from 6:30 AM to now.
//
//	NOT a repeat of opening confirmation — the first-10-minute window is closed.
//
// WHY:   After the opening candle, positions may need tightening or early exit
//
//	based on 60-minute structure, VWAP reclaim/rejection, or overextension.
//
// HOW:
//  1. Fetch intraday bars from 6:30 AM to current time (full continuation window).
//  2. Review open positions with fresh price context via RunPositionReviewActivity logic.
//  3. Do NOT use first-10-min entry logic. Do NOT re-run opening confirmation.
//
// TODO (future): add continuation entry scan for still-valid entry_ready setups —
//
//	check 60-min high/low structure, VWAP hold, volume continuation, spread still
//	acceptable, Claude re-confirms with fresh continuation payload.
func (d *ActivityDeps) RunContinuationReviewActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	reviewDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	log.Printf("continuation_review_started date=%s time=%s continuation_window=06:30-%s",
		reviewDate.Format("2006-01-02"), now.Format("15:04"), now.Format("15:04"))

	// Delegate to the same position review logic — it fetches current mid-prices
	// and sends to Claude. The continuation window context is captured in the log.
	// Full continuation entry logic (VWAP structure, 60-min high/low, second-leg
	// confirmation) is a TODO — this pass is position risk management only.
	result, err := d.RunPositionReviewActivity(ctx)
	if err != nil {
		return result, err
	}

	log.Printf("continuation_review_done result=%s", result)
	return result, nil
}

// mapPositionAction converts Claude's position review status string to the
// DB enum value used in position_reviews.suggested_action.
// partial_profit maps to EXIT: paper trades use 1 contract — no partial exit possible.
func mapPositionAction(claudeStatus string) string {
	switch claudeStatus {
	case "hold":
		return "HOLD"
	case "tighten_trail":
		return "HOLD_TIGHTEN_STOP"
	case "partial_profit":
		return "EXIT" // 1 contract = no partial; treat as full exit
	case "exit_now":
		return "EXIT"
	case "exit_on_trigger":
		return "WATCH_CLOSELY"
	default:
		return "HOLD"
	}
}

// RunWeeklyReviewActivity aggregates the past 7 days of paper-trade data,
// sends it to Claude for analysis, and persists the review to weekly_reviews.
//
// WHAT: Runs once per week (Sunday morning).
// WHY:  Autonomous operation requires periodic self-assessment. This activity
//
//	generates explicit tuning proposals without auto-applying them.
//
// HOW:
//  1. Compute week_start / week_end (today − 7 days).
//  2. Count confirmed candidates and closed positions for the period.
//  3. Build a structured text prompt with trade statistics.
//  4. Call Claude.GenerateText for a free-text review + proposals.
//  5. Persist to weekly_reviews (ON CONFLICT replaces if re-run).
func (d *ActivityDeps) RunWeeklyReviewActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	weekEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	weekStart := weekEnd.AddDate(0, 0, -7)

	log.Printf("activity: RunWeeklyReview %s → %s",
		weekStart.Format("2006-01-02"), weekEnd.Format("2006-01-02"))

	// ── 1. Aggregate data ────────────────────────────────────────────────────
	openPositions, _ := store.GetOpenPositionsForReview(ctx, d.Pool)
	closedPositions, _ := store.GetClosedPositionsInRange(ctx, d.Pool, weekStart, weekEnd)

	winCount, lossCount := 0, 0
	var totalPnL float64
	for _, p := range closedPositions {
		if p.RealizedPnLPct > 0 {
			winCount++
		} else {
			lossCount++
		}
		totalPnL += float64(p.RealizedPnLPct)
	}
	avgPnL := 0.0
	if len(closedPositions) > 0 {
		avgPnL = totalPnL / float64(len(closedPositions))
	}

	var confirmedCount int
	_ = d.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM trade_candidates
		 WHERE trade_date BETWEEN $1 AND $2 AND candidate_status='confirmed'`,
		weekStart, weekEnd,
	).Scan(&confirmedCount)

	// ── 2. Build prompt ──────────────────────────────────────────────────────
	closedLines := formatPaperPositionList(closedPositions)
	openLines := formatReviewablePositionList(openPositions)

	prompt := fmt.Sprintf(`Weekly paper-trade review.
Period: %s to %s.

TRADE SUMMARY:
- Confirmed candidates this week: %d
- Open positions at end of week: %d
- Positions closed this week: %d (wins=%d losses=%d)
- Average closed P&L: %.2f%%

CLOSED POSITIONS THIS WEEK:
%s

OPEN POSITIONS (carrying forward):
%s

Per the weekly review protocol, please produce:
1. Trade summary
2. Performance metrics (avg PnL, max gain, max loss, expectancy)
3. Failure analysis (false positives, missed trades)
4. Setup-family breakdown (continuation vs momentum_breakout)
5. Regime analysis
6. Chain quality observations
7. Strategy tuning proposals (explicit bullets — do NOT auto-apply)`,
		weekStart.Format("2006-01-02"), weekEnd.Format("2006-01-02"),
		confirmedCount,
		len(openPositions),
		len(closedPositions), winCount, lossCount,
		avgPnL,
		closedLines,
		openLines,
	)

	weeklySystemPrompt := `You are MakeMyTrade's weekly review analyst. Analyze the provided paper-trade data and return a structured weekly review. Proposals must be explicit and actionable but must NOT be auto-applied to strategy_rules.yaml.`

	// ── 3. Call Claude ───────────────────────────────────────────────────────
	claudeCli := claudeclient.NewClient(d.Cfg.AnthropicAPIKey, "claude-sonnet-4-6", d.Cfg.ClaudeMaxOutputTokens, weeklySystemPrompt)
	summary, err := claudeCli.GenerateText(weeklySystemPrompt, prompt)
	if err != nil {
		log.Printf("activity: weekly review Claude error: %v", err)
		summary = fmt.Sprintf(
			"[weekly review generation failed: %v]\nRaw stats: confirmed=%d closed=%d wins=%d losses=%d avgPnL=%.2f%%",
			err, confirmedCount, len(closedPositions), winCount, lossCount, avgPnL,
		)
	}

	// ── 4. Persist ───────────────────────────────────────────────────────────
	if insertErr := store.InsertWeeklyReview(ctx, d.Pool, store.WeeklyReviewInput{
		WeekStart: weekStart,
		WeekEnd:   weekEnd,
		Summary:   summary,
	}); insertErr != nil {
		log.Printf("activity: persist weekly review: %v", insertErr)
	}

	result := fmt.Sprintf("week=%s/%s confirmed=%d closed=%d wins=%d losses=%d",
		weekStart.Format("2006-01-02"), weekEnd.Format("2006-01-02"),
		confirmedCount, len(closedPositions), winCount, lossCount)
	log.Printf("activity: RunWeeklyReview done — %s", result)
	return result, nil
}

func formatPaperPositionList(positions []store.PaperPosition) string {
	if len(positions) == 0 {
		return "  (none)"
	}
	var sb strings.Builder
	for _, p := range positions {
		sb.WriteString(fmt.Sprintf("  - %s: entry=%.2f exit=%.2f pnl=%.2f%%\n",
			p.Ticker, p.EntryPrice, p.ExitPrice, p.RealizedPnLPct))
	}
	return sb.String()
}

func formatReviewablePositionList(positions []store.ReviewablePosition) string {
	if len(positions) == 0 {
		return "  (none)"
	}
	var sb strings.Builder
	for _, p := range positions {
		sb.WriteString(fmt.Sprintf("  - %s (%s/%s): entry=%.2f\n",
			p.Ticker, p.OptionType, p.SetupFamily, p.EntryPrice))
	}
	return sb.String()
}

// ── helpers ───────────────────────────────────────────────────────────────────

func loadWatchlist(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT ticker FROM symbols WHERE is_active=TRUE ORDER BY ticker`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// filterChainQuality applies the liquidity_filters from strategy_rules.yaml.
// Thresholds come from the YAML (OI ≥ 500, vol ≥ 100, spread ≤ 5%).
// Contracts that fail are removed before being sent to Claude.
func filterChainQuality(contracts []market.OptionContract, lf strategy.LiquidityFilters) []market.OptionContract {
	var qualified []market.OptionContract
	for _, c := range contracts {
		if c.SpreadPct > lf.MaxBidAskSpreadPctOfMid {
			continue
		}
		if c.OpenInterest < lf.MinOpenInterest {
			continue
		}
		if c.OptionVolume < lf.MinOptionVolume {
			continue
		}
		qualified = append(qualified, c)
	}
	return qualified
}
