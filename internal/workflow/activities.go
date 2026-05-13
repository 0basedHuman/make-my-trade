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
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/makemytrade/config"
"github.com/yourname/makemytrade/internal/execution"
	"github.com/yourname/makemytrade/internal/indicators"
	"github.com/yourname/makemytrade/internal/market"
	"github.com/yourname/makemytrade/internal/risk"
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
	todayRaw := time.Now().In(loc)
	today := time.Date(todayRaw.Year(), todayRaw.Month(), todayRaw.Day(), 0, 0, 0, 0, loc)
	todayStr := today.Format("2006-01-02")

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

	btcROC := 0.0 // BTC regime block removed; kept for summary logging only
	regime := strategy.Regime{VIX: vixLevel, BTCROC20: 0.0}
	regimeLabel := strategy.RegimeLabel(vixLevel, btcROC)
	earningsEvents, _ := d.Finnhub.FetchUpcomingEarnings(today, today.AddDate(0, 0, 7))

	// Load strategy config before goroutines so rsveConfig is captured by value.
	scanRules := d.Rules
	if scanRules == nil {
		scanRules = strategy.DefaultRules()
	}
	rsveConfig := scanRules.RSVE

	// Prescreen symbols in parallel
	type symResult struct {
		ticker           string
		analysis         strategy.SymbolAnalysis
		rsve             strategy.RSVEResult
		candID           string
		finnhubHeadlines []string
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

			// Fetch Finnhub company news (last 3 days) — already built, wired here
			newsItems, _ := d.Finnhub.FetchCompanyNews(t, today.AddDate(0, 0, -3), today)
			var finnhubHeadlines []string
			for i, n := range newsItems {
				if i >= 8 {
					break
				}
				if n.Headline != "" {
					finnhubHeadlines = append(finnhubHeadlines, n.Headline)
				}
			}

			a := d.Engine.Analyze(t, todayStr, b, regime, spyBars, earningsRisk, sentiment)

			// RSVE gate evaluation — replaces 5-family gate decisions.
			earningsDaysAway := -1
			if earningsRisk {
				earningsDaysAway = 0
			}
			rsveResult := strategy.EvaluateRSVE(strategy.RSVEInput{
				Ticker:           t,
				Date:             todayStr,
				Bars:             b,
				SPYBars:          spyBars,
				VIX:              vixLevel,
				EarningsDaysAway: earningsDaysAway,
				IVRank:           -1,
				BidAskSpreadPct:  -1,
				OpenInterest:     -1,
				OptionVolume:     -1,
			}, rsveConfig)

			// Map RSVE status to DB candidate_status.
			// paper_trade_created → entry_ready (goes through intraday confirmation)
			// stock_signal_passed → monitoring only, no paper trade until options verified
			// option_blocked      → monitoring only, option quality failed
			// rejected            → stored as-is
			dailyStatus := rsveResult.Status
			if rsveResult.Status == "paper_trade_created" {
				dailyStatus = "entry_ready"
			}
			a.Eligible = rsveResult.AllPass
			a.CandidateStatus = dailyStatus
			a.SetupFamily = "rsve_" + rsveResult.Direction
			a.PatternScoreInt = int(rsveResult.Score)
			a.PatternScore = rsveResult.Score

			// Map RSVE 12 gates → DB boolean columns.
			// GateBTC column now stores relative_strength gate result.
			// GateRSI column unused (rsi_range gate removed); stays false.
			var gateTrend, gateMomentum, gateVolume, gateVIX, gateBTC bool
			for _, g := range rsveResult.Gates {
				switch g.Name {
				case "vix_regime":
					gateVIX = g.Passed
				case "market_uptrend", "market_downtrend":
					gateTrend = g.Passed
				case "volume_expansion":
					gateVolume = g.Passed
				case "vol_squeeze":
					gateMomentum = g.Passed
				case "relative_strength", "relative_weakness":
					gateBTC = g.Passed
				}
			}

			var prevVol int64
			if len(b) > 0 {
				prevVol = int64(b[len(b)-1].Volume)
			}

			candID, upsertErr := store.UpsertCandidate(ctx, d.Pool, store.UpsertCandidateInput{
				TradeDate:       today,
				Ticker:          t,
				GateTrend:       gateTrend,
				GateMomentum:    gateMomentum,
				GateVolume:      gateVolume,
				GateVIX:         gateVIX,
				GateRelativeStrength: gateBTC,
				GateRSI:         false,
				AllGates:        rsveResult.AllPass,
				ClosePrice:      a.ClosePrice,
				EMA20:           a.EMA20,
				EMA100:          a.EMA100,
				RSI14:           a.RSI14,
				MACDHist:        a.MACDHist,
				VolumeRatio:     a.VolumeRatio,
				VIXLevel:        vixLevel,
				BTCROC20:        btcROC,
				PatternName:     "rsve_" + rsveResult.Direction,
				PatternScore:    rsveResult.Score,
				AntiPatterns:    nil,
				RejectedByAnti:  false,
				EntryLow:        a.EntryLow,
				EntryHigh:       a.EntryHigh,
				StopLoss:        a.StopLoss,
				Target1:         a.Target1,
				Target2:         a.Target2,
				RRRatio:         a.RRRatio,
				HoldDaysMin:     a.HoldDaysMin,
				HoldDaysBase:    a.HoldDaysBase,
				HoldDaysMax:     a.HoldDaysMax,
				RejectReason:    rsveResult.RejectGate,
				CandidateStatus: dailyStatus,
				SetupFamily:     a.SetupFamily,
				Direction:       rsveResult.Direction,
				PrevDayVolume:   prevVol,
			})
			if upsertErr != nil {
				log.Printf("activity: upsert %s: %v", t, upsertErr)
			}

			// Sentiment diagnostics — context only, never a gate.
			sc := market.FetchSentimentContext(t)
			if sc.Signal != "unavailable" {
				log.Printf("activity: sentiment_context ticker=%s signal=%s source=%s note=%s",
					sc.Ticker, sc.Signal, sc.Source, sc.Note)
			}

			mu.Lock()
			allResults = append(allResults, symResult{
				ticker:           t,
				analysis:         a,
				rsve:             rsveResult,
				candID:           candID,
				finnhubHeadlines: finnhubHeadlines,
			})
			mu.Unlock()
		}(ticker, bars)
	}
	wg.Wait()

	// Count candidates by status.
	candidateCount := 0
	stockSignalCount := 0
	optionBlockedCount := 0
	for _, r := range allResults {
		switch r.analysis.CandidateStatus {
		case "entry_ready":
			candidateCount++
		case "stock_signal_passed":
			stockSignalCount++
		case "option_blocked":
			optionBlockedCount++
		}
	}
	log.Printf("activity: rsve_scan_complete entry_ready=%d stock_signal_passed=%d option_blocked=%d rejected=%d",
		candidateCount, stockSignalCount, optionBlockedCount,
		len(allResults)-candidateCount-stockSignalCount-optionBlockedCount)

	noTrade := candidateCount == 0
	noTradeReason := ""
	if noTrade {
		noTradeReason = "No symbols reached entry_ready status"
	}

	store.UpsertDailySummary(ctx, d.Pool, store.DailySummary{
		TradeDate:         today,
		VIXLevel:          vixLevel,
		BTCROC20:          btcROC,
		RegimeLabel:       regimeLabel,
		SymbolsScanned:    len(allResults),
		CandidatesFound:   candidateCount,
		NoTradeToday:      noTrade,
		NoTradeReason:     noTradeReason,
		RegimeSummary:     fmt.Sprintf("VIX %.1f | BTC ROC %.1f%% | %s", vixLevel, btcROC, regimeLabel),
		WatchTickers:      nil,
		AnalysisCompleted: true,
	})

	return fmt.Sprintf("scanned=%d entry_ready=%d stock_signal=%d option_blocked=%d no_trade=%v",
		len(allResults), candidateCount, stockSignalCount, optionBlockedCount, noTrade), nil
}

// RunOpeningConfirmationActivity evaluates the first 10 minutes of trading for
// WHAT:  Runs after 6:40 AM PT on the trade date, once opening bars are available.
// WHY:   Separates structural setup quality (overnight analysis) from live open behavior.
//        Deterministic confirmation signals are the sole authority — no Claude.
//
// HOW:
//  1. Load entry_ready candidates for today.
//  2. Fetch 1-min bars (6:30–6:40 AM PT) for all tickers + SPY in one batch.
//  3. Shortlist top N by score (strategy.TradeFrequency config).
//  4. For each shortlisted candidate:
//     a. Run deterministic confirmation (required + optional signals).
//     b. If auto_reject fired → watch_only (hard block).
//     c. Fetch option chain, filter for quality, select best contract.
//     d. If no qualifying contract → watch_only.
//     e. If required signals pass + ≥1 optional → confirmed; buy via execution.BuyOptionPosition.
//  5. Non-shortlisted entry_ready → watch_only (didn't pass score bar).
//  6. Update daily_summaries.candidates_confirmed.
//
// WHAT BREAKS: If bars are unavailable, confirmation signals default to failing → watch_only.
func (d *ActivityDeps) RunOpeningConfirmationActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	tradeDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	marketOpen := time.Date(now.Year(), now.Month(), now.Day(), 6, 30, 0, 0, loc)

	log.Printf("schedule_opening_confirmation_started date=%s time=%s opening_confirmation_window=06:30-06:40",
		tradeDate.Format("2006-01-02"), now.Format("15:04"))

	// ── Too-early guard ──────────────────────────────────────────────────────────
	// Opening bars (6:30–6:40 candle) are not available before 6:40 AM.
	// DailyResearchCycle triggers this activity as a recovery step after finishing
	// analysis — on normal days research finishes before 6:30, so this guard fires
	// and returns cleanly; the scheduled OpeningConfirmationCycle at 6:42 handles it.
	earlyCheck := time.Date(now.Year(), now.Month(), now.Day(), 6, 40, 0, 0, loc)
	if now.Before(earlyCheck) {
		msg := fmt.Sprintf("opening_confirmation_early: PT time %s before 06:40 — opening bars not yet available",
			now.Format("15:04"))
		log.Printf("activity: %s", msg)
		return msg, nil
	}

	// ── Staleness guard ──────────────────────────────────────────────────────────
	// Reject runs past the configured cutoff so stale opening-candle logic is never
	// used. Cutoff is 08:30 to support late-start recovery (worker offline scenario).
	cutoffHour, cutoffMin := 8, 30
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

	// Load bearish_exhaustion_reversal structural candidates for intraday rejection check.
	// Their tickers are included in the bar-fetch batch so we only make one Alpaca call.
	exhaustionCandidates, exhaustionErr := store.GetExhaustionReversalStructuralCandidates(ctx, d.Pool, tradeDate)
	if exhaustionErr != nil {
		log.Printf("activity: load exhaustion structural candidates: %v", exhaustionErr)
		exhaustionCandidates = nil
	}
	// De-duplicate: only add tickers not already in the entry_ready list
	entryReadyTickers := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		entryReadyTickers[c.Ticker] = true
	}
	for _, c := range exhaustionCandidates {
		if !entryReadyTickers[c.Ticker] {
			tickers = append(tickers, c.Ticker)
		}
	}

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

	// ── Exhaustion reversal rejection pass ──────────────────────────────────────
	// For each bearish_exhaustion_reversal structural_candidate, run the intraday
	// rejection check. If confirmed, promote to entry_ready and add to shortlist
	// so they flow through the same chain-fetch + Claude confirmation path.
	// Candidates that fail (hard block or rejection not confirmed) stay as
	// structural_candidate and are not promoted.
	for _, c := range exhaustionCandidates {
		rejResult := strategy.EvaluateExhaustionRejection(strategy.ExhaustionRejectionInput{
			Ticker:        c.Ticker,
			PrevDayVolume: c.PrevDayVolume,
			Bars:          barsMap[c.Ticker],
			SPYBars:       spyBars,
		})

		if rejResult.HardBlockFired {
			log.Printf("activity: %s exhaustion_hard_block=%s — stays structural_candidate",
				c.Ticker, rejResult.HardBlockReason)
			continue
		}

		if !rejResult.RejectionConfirmed {
			log.Printf("activity: %s exhaustion_not_confirmed vwap_break=%v or_mid_break=%v rel_vol=%.2f market_not_bullish=%v",
				c.Ticker, rejResult.VWAPBreak, rejResult.ORMidBreak, rejResult.RelVolume, rejResult.MarketNotBullish)
			continue
		}

		// Rejection confirmed — promote structural_candidate → entry_ready
		log.Printf("activity: %s exhaustion_rejection_confirmed → promoting to entry_ready (vwap=%.2f or_mid=%.2f rel_vol=%.2f)",
			c.Ticker, rejResult.VWAP, rejResult.ORMid, rejResult.RelVolume)

		if promoteErr := store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "entry_ready"); promoteErr != nil {
			log.Printf("activity: promote exhaustion %s to entry_ready: %v", c.Ticker, promoteErr)
			continue
		}
		c.CandidateStatus = "entry_ready"
		shortlisted = append(shortlisted, c)
		shortlistedIDs[c.ID] = true
	}

	if len(shortlisted) == 0 {
		log.Println("activity: no candidates passed shortlist score bar or exhaustion rejection check")
		return fmt.Sprintf("confirmed=0 watch_only=%d no_shortlisted=true", watchOnlyCount), nil
	}

	// ── 4-5. Deterministic evidence + chain selection ──────────────────────────
	// Contract must be selected before confirmation so the actual option can be
	// evaluated: strike, DTE, delta, spread, premium.
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

	lf := rules.OptionsTranslation.LiquidityFilters
	todayStr := tradeDate.Format("2006-01-02")

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

		// Hard block: auto_reject is always non-overridable
		if result.AutoRejected {
			log.Printf("activity: %s → watch_only (hard_block: %s)", c.Ticker, result.AutoRejectReason)
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			evidenceMap[c.Ticker] = candidateEvidence{cand: c, result: result, hardBlocked: true}
			continue
		}

		// ── b. Fetch option chain and select best contract ───────────────────────
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

		// Build contract selection opts: family-specific DTE/delta, global target DTE.
		selOpts := market.ContractSelectionOpts{
			TargetDTE:     rules.Risk.OptionLifecycle.TargetDTE,
			AvoidDTEBelow: rules.Risk.OptionLifecycle.AvoidDTEBelow,
		}
		if fc, ok := rules.FamilyFor(c.SetupFamily); ok && fc.Options.DTEMin > 0 {
			selOpts.DTEMin = fc.Options.DTEMin
			selOpts.DTEMax = fc.Options.DTEMax
			selOpts.DeltaMin = fc.Options.DeltaMin
			selOpts.DeltaMax = fc.Options.DeltaMax
		} else {
			selOpts.DTEMin = rules.Risk.OptionLifecycle.DTEMin
			selOpts.DTEMax = rules.Risk.OptionLifecycle.DTEMax
		}

		best := market.SelectBestContract(qualified, optionType, selOpts)
		if best == nil {
			log.Printf("activity: %s → watch_only (candidate_skipped_no_valid_contract: type=%s chain=%d qualified=%d dteRange=%d-%d targetDTE=%d)",
				c.Ticker, optionType, len(contracts), len(qualified), selOpts.DTEMin, selOpts.DTEMax, selOpts.TargetDTE)
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			evidenceMap[c.Ticker] = candidateEvidence{cand: c, result: result, hardBlocked: false}
			continue
		}

		// Quote-realistic fill: mid for tight spreads, haircut for wide, reject if too wide.
		fill := market.ComputeEntryFill(best.Bid, best.Ask)
		if fill.Rejected {
			log.Printf("activity: %s → watch_only (fill_rejected=%s bid=%.2f ask=%.2f spread=%.1f%%)",
				c.Ticker, fill.RejectReason, best.Bid, best.Ask, fill.SpreadPct)
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			continue
		}
		limitPrice := fill.Price

		log.Printf("activity: contract_selected: ticker=%s family=%s contract=%s strike=%.2f dte=%d dte_range=%d-%d target_dte=%d delta=%.3f fill=%.2f mode=%s spread=%.1f%% quality=%.0f slippage=%.4f oi=%d vol=%d",
			c.Ticker, c.SetupFamily, best.Symbol, best.Strike, best.DTE, selOpts.DTEMin, selOpts.DTEMax, selOpts.TargetDTE, best.Delta, limitPrice, fill.Mode, fill.SpreadPct, fill.QualityScore, fill.SlippageEst, best.OpenInterest, best.OptionVolume)

		// ── b2. Save daily IV snapshot (best-effort; never blocks entry) ────────────
		// Proxy IV = ask / (underlying * sqrt(DTE/252)). Used for rolling IV rank.
		proxyIV := market.ComputeProxyIV(*best, c.ClosePrice)
		if proxyIV > 0 {
			if err := store.SaveIVSnapshot(ctx, d.Pool,
				c.Ticker, tradeDate.Format("2006-01-02"),
				best.Symbol, best.Strike, c.ClosePrice, best.Ask,
				best.DTE, proxyIV,
			); err != nil {
				log.Printf("activity: iv_snapshot save %s: %v (non-fatal)", c.Ticker, err)
			}
		}

		// ── b3. IV rank gate — reject if buying above-median volatility ─────────────
		ivf := rules.Risk.IVFilter
		if ivf.Enabled && proxyIV > 0 {
			ivRank, ivSnaps, ivErr := store.GetIVRank(ctx, d.Pool, c.Ticker, proxyIV, ivf.LookbackDays)
			if ivErr != nil {
				log.Printf("activity: iv_rank query %s: %v (skipping gate)", c.Ticker, ivErr)
			} else if ivSnaps < ivf.MinSnapshotsRequired {
				log.Printf("activity: iv_rank %s: only %d snapshots (need %d) — warmup, gate skipped proxy_iv=%.4f",
					c.Ticker, ivSnaps, ivf.MinSnapshotsRequired, proxyIV)
			} else if ivRank > ivf.MaxIVRankPct {
				log.Printf("activity: %s → watch_only (iv_rank=%.0f%% > max=%.0f%% proxy_iv=%.4f snaps=%d — expensive premium)",
					c.Ticker, ivRank, ivf.MaxIVRankPct, proxyIV, ivSnaps)
				_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
				watchOnlyCount++
				continue
			} else {
				qualifier := ""
				if ivRank <= ivf.IdealIVRankPct {
					qualifier = " [IDEAL]"
				}
				log.Printf("activity: iv_rank %s: %.0f%% proxy_iv=%.4f snaps=%d%s",
					c.Ticker, ivRank, proxyIV, ivSnaps, qualifier)
			}
		}

		evidenceMap[c.Ticker] = candidateEvidence{
			cand:       c,
			result:     result,
			contract:   best,
			limitPrice: limitPrice,
			optionType: optionType,
		}
	}

	// Count confirmable candidates (not hard-blocked, have a valid contract).
	confirmable := 0
	for _, ev := range evidenceMap {
		if !ev.hardBlocked && ev.contract != nil {
			confirmable++
		}
	}
	if confirmable == 0 {
		log.Println("activity: all shortlisted candidates blocked or no valid contracts")
		return fmt.Sprintf("confirmed=0 watch_only=%d all_blocked=true", watchOnlyCount), nil
	}

	// ── 6. Deterministic confirmation — RSVE gates already passed at daily scan. ─
	// Opening confirmation status is "confirmed" or "watch_only" — set by
	// EvaluateConfirmation using required+optional signal logic.
	// No Claude call: RSVE is the sole gate engine.

	confirmedCount := 0

	for _, c := range shortlisted {
		ev, ok := evidenceMap[c.Ticker]
		if !ok || ev.hardBlocked || ev.contract == nil {
			continue
		}

		if ev.result.Status != "confirmed" {
			log.Printf("activity: %s → watch_only (confirmation_status=%s signals=%d)",
				c.Ticker, ev.result.Status, ev.result.SignalsPassed)
			_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "watch_only")
			watchOnlyCount++
			continue
		}

		best := ev.contract
		limitPrice := ev.limitPrice

		log.Printf("activity: rsve_confirmed: %s %s status=confirmed signals=%d limit=%.2f",
			c.Ticker, best.Symbol, ev.result.SignalsPassed, limitPrice)

		_ = store.UpdateCandidateStatus(ctx, d.Pool, c.ID, "confirmed")

		// Suppress duplicate: don't open a second position if one is already open.
		if hasPos, _ := store.HasOpenPositionForTicker(ctx, d.Pool, c.Ticker); hasPos {
			log.Printf("activity: %s already has open position — skipping buy", c.Ticker)
			confirmedCount++
			continue
		}

		// Portfolio limits gate.
		pl := rules.Risk.PortfolioLimits
		if pl.MaxOpenPositions > 0 {
			totalOpen, _ := store.GetOpenPositionCount(ctx, d.Pool)
			if totalOpen >= pl.MaxOpenPositions {
				log.Printf("activity: %s → skipped (portfolio_limit: open=%d >= max=%d)",
					c.Ticker, totalOpen, pl.MaxOpenPositions)
				confirmedCount++
				continue
			}
		}
		if pl.MaxSameDirection > 0 {
			dirCount, _ := store.GetOpenPositionCountByDirection(ctx, d.Pool, ev.optionType)
			if dirCount >= pl.MaxSameDirection {
				log.Printf("activity: %s → skipped (direction_limit: %s open=%d >= max=%d)",
					c.Ticker, ev.optionType, dirCount, pl.MaxSameDirection)
				confirmedCount++
				continue
			}
		}
		if pl.MaxPremiumPctPortfolio > 0 && pl.PaperPortfolioValue > 0 {
			maxPremium := pl.PaperPortfolioValue * pl.MaxPremiumPctPortfolio / 100.0
			if ev.limitPrice > maxPremium {
				log.Printf("activity: %s → skipped (premium_budget: limit=%.2f > max=%.2f (%.1f%% of $%.0f))",
					c.Ticker, ev.limitPrice, maxPremium, pl.MaxPremiumPctPortfolio, pl.PaperPortfolioValue)
				confirmedCount++
				continue
			}
		}

		// Structure invalidation level: use candidate's stop_loss (underlying-price level
		// computed during daily analysis from pattern detection or ATR).
		structureLevel := c.StopLoss

		buyResult, buyErr := execution.BuyOptionPosition(ctx, d.Pool, d.Alpaca, execution.BuyInput{
			CandidateID:                c.ID,
			Ticker:                     c.Ticker,
			SetupFamily:                c.SetupFamily,
			OptionType:                 ev.optionType,
			ContractSymbol:             best.Symbol,
			LimitPrice:                 limitPrice,
			StopLoss:                   c.StopLoss,
			Target1:                    c.Target1,
			Target2:                    c.Target2,
			StructureInvalidationLevel: structureLevel,
		})
		if buyErr != nil {
			log.Printf("activity: buy option position %s: %v", c.Ticker, buyErr)
			confirmedCount++
			continue
		}

		_ = store.InsertPositionEvent(ctx, d.Pool, buyResult.PositionID, c.Ticker, "position_opened",
			limitPrice, map[string]any{
				"candidate_status":  "confirmed",
				"setup_family":      c.SetupFamily,
				"contract":          best.Symbol,
				"strike":            best.Strike,
				"dte":               best.DTE,
				"delta":             best.Delta,
				"signals_passed":    ev.result.SignalsPassed,
				"confirmation_type": "rsve_deterministic",
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
// WHAT: Runs at 07:15 and 07:45 PT. All positions default to HOLD.
//       Mechanical exits (stop/TP/trail/time-stop/max-hold) handle hard cases.
//       No Claude — fully deterministic.
//
// HOW:
//  1. Run mechanical exits first — hard exits fire immediately.
//  2. Load positions still open after mechanical exits.
//  3. Fetch current option mid-price and compute PnL%.
//  4. All positions: HOLD. Record position_reviews row.
func (d *ActivityDeps) RunPositionReviewActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	reviewDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	log.Printf("first_position_review_started date=%s time=%s", reviewDate.Format("2006-01-02"), now.Format("15:04"))

	rules := d.Rules
	if rules == nil {
		rules = strategy.DefaultRules()
	}

	// ── Run mechanical exits first — hard exits must fire before Claude review ─
	mexitExited, mexitChecked, mexitErr := runMechanicalChecks(ctx, d.Pool, d.Alpaca, rules, now, "position_review")
	if mexitErr != nil {
		log.Printf("activity: mechanical check error in position review: %v — continuing", mexitErr)
	} else {
		log.Printf("activity: mechanical_check_complete checked=%d exited=%d", mexitChecked, mexitExited)
	}
	_ = mexitChecked // used in log above; suppress unused warning

	// ── 1. Load open positions (those still open after mechanical exits) ──────
	positions, err := store.GetOpenPositionsForReview(ctx, d.Pool)
	if err != nil {
		return "", fmt.Errorf("load open positions: %w", err)
	}
	if len(positions) == 0 {
		log.Println("activity: no open positions — skipping review")
		return fmt.Sprintf("reviewed=0 mechanical_exited=%d", mexitExited), nil
	}

	// ── 2. Fetch current prices and compute PnL ──────────────────────────────
	type enriched struct {
		pos          store.ReviewablePosition
		currentPrice float64
		pnlPct       float64
		daysHeld     int
	}
	var items []enriched

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
	}

	// ── 3. All positions default to HOLD — mechanical exits handle hard cases ─
	log.Printf("activity: %d positions reviewed mechanically — mechanical exits handle stops/TP/trail", len(items))

	// ── 4-5. Persist reviews ──────────────────────────────────────────────────
	reviewedCount, exitedCount := 0, 0

	for _, e := range items {
		p := e.pos
		action := "HOLD"
		rationale := "mechanical_hold — stops and TP managed by MechanicalRiskCycle"

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

// RunEODPositionReviewActivity runs at 12:45 PM PT.
//
// WHAT: End-of-day position review for 21-45 DTE swing positions.
//  1. Run mechanical checks (hard stop/TP/trail/time-stop/max-hold fire immediately).
//  2. Remaining positions are logged as held overnight — no forced EOD exit.
//
// WHY:  21-45 DTE swing options must hold overnight. Theta decay and bid-ask spread
//       make same-day forced exits extremely destructive. Mechanical invalidation
//       (stop, TP, trail, time stop, max hold) handles hard exits; everything else holds.
func (d *ActivityDeps) RunEODPositionReviewActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	reviewDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	log.Printf("eod_review_started date=%s time=%s", reviewDate.Format("2006-01-02"), now.Format("15:04"))

	rules := d.Rules
	if rules == nil {
		rules = strategy.DefaultRules()
	}

	// ── Step 1: mechanical exits first ───────────────────────────────────────
	mexitExited, mexitChecked, mexitErr := runMechanicalChecks(ctx, d.Pool, d.Alpaca, rules, now, "eod_review")
	if mexitErr != nil {
		log.Printf("activity: eod mechanical check error: %v — continuing", mexitErr)
	}
	_ = mexitChecked

	// ── Step 2: load positions still open after mechanical exits ──────────────
	positions, err := store.GetOpenPositionsForReview(ctx, d.Pool)
	if err != nil {
		return "", fmt.Errorf("eod load positions: %w", err)
	}
	if len(positions) == 0 {
		return fmt.Sprintf("eod_reviewed=0 mechanical_exited=%d", mexitExited), nil
	}

	// ── Step 3: log overnight holds — positions hold by default ─────────────
	// No forced EOD exit for 21-45 DTE swings. Mechanical exits (stop/TP/trail/
	// time-stop/max-hold) handle all hard invalidations in the 10-min risk cycle.
	holdCount := 0
	for _, p := range positions {
		pnlPct := 0.0
		currentPrice := p.EntryPrice
		if p.OptionSymbol != "" && p.OptionPremium > 0 {
			if mid, midErr := d.Alpaca.FetchOptionMidPrice(p.OptionSymbol); midErr == nil {
				currentPrice = mid
				pnlPct = (mid - p.OptionPremium) / p.OptionPremium * 100.0
			}
		}
		daysHeld := int(now.Sub(p.EntryDate).Hours() / 24)
		_ = store.InsertPositionEvent(ctx, d.Pool, p.ID, p.Ticker, "eod_holding_overnight",
			currentPrice, map[string]any{"days_held": daysHeld, "pnl_pct": pnlPct})
		log.Printf("activity: eod holding overnight %s (days_held=%d pnl=%.1f%%)",
			p.Ticker, daysHeld, pnlPct)
		holdCount++
	}

	result := fmt.Sprintf("mechanical_exited=%d held_overnight=%d", mexitExited, holdCount)
	log.Printf("activity: RunEODPositionReview done — %s", result)
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

// RunMechanicalRiskCheckActivity runs every 10 minutes during market hours.
//
// WHAT: Loads all open paper positions, fetches current option mid-price for each,
//
//	and evaluates mechanical exit rules (stop loss, take profit, trailing, EOD).
//	Exits immediately when a hard rule fires — does NOT wait for Claude.
//
// WHY:  7–14 DTE options decay fast. Hard stops and take-profits must fire within
//
//	minutes, not hours. Claude-only scheduled reviews (07:15, 07:45, 12:45 PT)
//	are too infrequent for mechanical protection.
//
// HOW:
//  1. Load open positions with risk state.
//  2. For each: fetch current option mid-price.
//  3. Evaluate EvaluateMechanicalExit.
//  4. Always persist updated risk state (peak, trailing, last_price).
//  5. If ShouldExit: call execution.SellOptionPosition → insert event.
func (d *ActivityDeps) RunMechanicalRiskCheckActivity(ctx context.Context) (string, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	nowPT := time.Now().In(loc)

	log.Printf("mechanical_risk_check_started time=%s", nowPT.Format("15:04"))

	rules := d.Rules
	if rules == nil {
		rules = strategy.DefaultRules()
	}

	exited, checked, err := runMechanicalChecks(ctx, d.Pool, d.Alpaca, rules, nowPT, "mechanical_risk_cycle")
	if err != nil {
		return "", err
	}

	result := fmt.Sprintf("checked=%d exited=%d", checked, exited)
	log.Printf("mechanical_risk_check_done result=%s", result)
	return result, nil
}

// runMechanicalChecks is the shared implementation used by RunMechanicalRiskCheckActivity
// and by the review activities (which call it before passing remaining positions to Claude).
// Returns (exitedCount, checkedCount, error).
func runMechanicalChecks(ctx context.Context, pool *pgxpool.Pool, alpaca execution.Alpaca, rules *strategy.Rules, nowPT time.Time, source string) (int, int, error) {
	positions, err := store.GetOpenPositionsForRiskCheck(ctx, pool)
	if err != nil {
		return 0, 0, fmt.Errorf("runMechanicalChecks: load positions: %w", err)
	}
	if len(positions) == 0 {
		return 0, 0, nil
	}

	mexitRules := rules.Risk.MechanicalExits
	if !mexitRules.Enabled {
		log.Printf("mechanical_risk: disabled in rules — skipping (%d positions)", len(positions))
		return 0, len(positions), nil
	}

	exitedCount := 0
	for _, p := range positions {
		if p.OptionSymbol == "" || p.OptionPremium <= 0 {
			log.Printf("mechanical_risk: %s no option_symbol or premium — skipping", p.Ticker)
			continue
		}

		// Fetch current option mid-price
		currentMid, midErr := alpaca.FetchOptionMidPrice(p.OptionSymbol)
		if midErr != nil {
			log.Printf("mechanical_risk: %s fetch mid %s: %v — skipping", p.Ticker, p.OptionSymbol, midErr)
			continue
		}

		// Fetch underlying price for structure invalidation check (best-effort; non-blocking).
		underlyingClose, _ := alpaca.FetchLatestQuote(p.Ticker)

		direction := "bullish"
		if p.OptionType == "put" {
			direction = "bearish"
		}

		// Use pattern-derived structure level when available; fall back to entry price as proxy.
		structureLevel := p.StructureInvalidationLevel
		if structureLevel <= 0 {
			structureLevel = p.EntryPrice
		}

		pos := risk.PositionRiskState{
			PositionID:                 p.ID,
			Ticker:                     p.Ticker,
			OptionSymbol:               p.OptionSymbol,
			EntryPremium:               p.OptionPremium,
			PeakOptionPrice:            p.PeakOptionPrice,
			TrailingActive:             p.TrailingActive,
			EntryDate:                  p.EntryDate,
			DaysHeld:                   int(nowPT.Sub(p.EntryDate).Hours() / 24),
			Direction:                  direction,
			StructureInvalidationLevel: structureLevel,
			UnderlyingClose:            underlyingClose,
		}

		dec := risk.EvaluateMechanicalExit(pos, currentMid, mexitRules, nowPT)

		// Always persist risk state (peak, trailing, last price)
		if riskErr := store.UpdatePositionRiskState(ctx, pool, p.ID, currentMid, dec.PeakPremium, dec.TrailingActive); riskErr != nil {
			log.Printf("mechanical_risk: %s persist risk state: %v", p.Ticker, riskErr)
		}

		if !dec.ShouldExit {
			log.Printf("mechanical_risk: %s holding pnl=%.1f%% trail=%v peak=%.2f source=%s",
				p.Ticker, dec.PnLPct, dec.TrailingActive, dec.PeakPremium, source)
			continue
		}

		// Mechanical exit fires — sell immediately without waiting for Claude
		log.Printf("mechanical_risk: %s EXIT reason=%s pnl=%.1f%% current=%.2f entry=%.2f source=%s",
			p.Ticker, dec.Reason, dec.PnLPct, currentMid, p.OptionPremium, source)

		_, sellErr := execution.SellOptionPosition(ctx, pool, alpaca, execution.SellInput{
			PositionID:     p.ID,
			Ticker:         p.Ticker,
			ContractSymbol: p.OptionSymbol,
			SellPrice:      currentMid * 0.99, // bid-safe limit
			PnLPct:         dec.PnLPct,
			ExitReason:     dec.Reason,
		})
		if sellErr != nil {
			log.Printf("mechanical_risk: %s sell failed: %v — keeping open", p.Ticker, sellErr)
			_ = store.InsertPositionEvent(ctx, pool, p.ID, p.Ticker, "mechanical_exit_sell_failed",
				currentMid, map[string]any{"reason": dec.Reason, "error": sellErr.Error(), "pnl_pct": dec.PnLPct})
			continue
		}
		_ = store.InsertPositionEvent(ctx, pool, p.ID, p.Ticker, "mechanical_exit",
			currentMid, map[string]any{
				"reason":   dec.Reason,
				"pnl_pct":  dec.PnLPct,
				"peak":     dec.PeakPremium,
				"trailing": dec.TrailingActive,
				"source":   source,
			})
		exitedCount++
	}

	return exitedCount, len(positions), nil
}

// RunWeeklyReviewActivity is disabled — no Claude API calls in automated paths.
func (d *ActivityDeps) RunWeeklyReviewActivity(ctx context.Context) (string, error) {
	log.Printf("activity: RunWeeklyReview disabled")
	return "disabled", nil
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
