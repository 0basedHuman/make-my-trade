// internal/api/handlers.go
//
// WHAT: HTTP handler functions and the main daily analysis pipeline.
//
// WHY:  The HTTP layer is thin — it validates input, runs the pipeline,
//       and returns JSON. All strategy logic lives in strategy_rules.yaml
//       and is applied by Claude. This layer handles:
//         1. App-side indicator preprocessing (strategy/engine.go)
//         2. Option chain fetching and quality filtering (market/alpaca.go)
//         3. Building the RuntimePayload for Claude (claude/client.go)
//         4. Persisting the decision to the DB (store/store.go)
//         5. Returning the result as JSON
//
// HOW:  POST /api/run-analysis → runPipeline() → returns AnalysisResponse
//       GET  /api/daily-analysis → reads stored results for today
//       GET  /api/paper-positions → returns open paper positions
//
// WHAT BREAKS: If Alpaca returns no bars for a symbol, it is skipped.
//              If the option chain API returns 403 (paper account without
//              options enabled), we proceed with empty chain data — Claude
//              will classify the setup as structural_candidate.
//              If Claude fails, the candidate stays with claude_action=PENDING.
//
// VERIFY: After /api/run-analysis:
//   psql $DB_URL -c "SELECT ticker, all_gates_passed, claude_action, claude_confidence FROM trade_candidates WHERE trade_date=CURRENT_DATE;"

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

// Handler holds the shared dependencies injected at startup.
type Handler struct {
	pool    *pgxpool.Pool
	cfg     *config.Config
	alpaca  *market.AlpacaClient
	finnhub *market.FinnhubClient
	fred    *market.FREDClient
	engine  *strategy.Engine
	rules   *strategy.Rules
}

// New creates a Handler with all dependencies wired.
// It loads strategy_rules.yaml at startup so the engine and filterChainQuality
// use the YAML thresholds rather than hardcoded constants.
func New(pool *pgxpool.Pool, cfg *config.Config) *Handler {
	rules, err := strategy.LoadRules("strategy_rules.yaml")
	if err != nil {
		log.Printf("handlers: warning — could not load strategy_rules.yaml: %v — using defaults", err)
		rules = strategy.DefaultRules()
	}
	return &Handler{
		pool:    pool,
		cfg:     cfg,
		alpaca:  market.NewAlpacaClient(cfg.AlpacaPaperAPIKey, cfg.AlpacaPaperSecret, cfg.AlpacaDataURL, cfg.AlpacaBaseURL),
		finnhub: market.NewFinnhubClient(cfg.FinnhubAPIKey),
		fred:    market.NewFREDClient(cfg.FREDAPIKey),
		engine:  strategy.NewEngine(strategy.DefaultConfig(), rules),
		rules:   rules,
	}
}

// ── Response types ────────────────────────────────────────────────────────────

// GateDetail is a single indicator gate result for the UI.
type GateDetail struct {
	Passed bool    `json:"passed"`
	Reason string  `json:"reason,omitempty"`
	Value  float64 `json:"value"`
}

// CandidateResponse is a per-symbol result row in the API response.
// It combines preprocessing indicators with Claude's options decision.
type CandidateResponse struct {
	Ticker       string   `json:"ticker"`
	Eligible     bool     `json:"eligible"`
	TrendBias    string   `json:"trend_bias"`
	ClosePrice   float64  `json:"close_price"`
	EMA20        float64  `json:"ema20"`
	EMA50        float64  `json:"ema50"`
	RSI14        float64  `json:"rsi14"`
	MACDHist     float64  `json:"macd_hist"`
	VolumeRatio  float64  `json:"volume_ratio"`
	PatternName  string   `json:"pattern_name"`
	AntiPatterns []string `json:"anti_patterns,omitempty"`

	// v2: setup family classification from strategy_rules.yaml
	SetupFamily   string   `json:"setup_family,omitempty"` // e.g. "bullish_continuation"
	ReasonCodes   []string `json:"reason_codes,omitempty"` // from YAML reason_codes enum
	BaseTarget    float64  `json:"base_target,omitempty"`  // structure-based target
	StretchTarget float64  `json:"stretch_target,omitempty"`

	// Options decision fields (from Claude)
	// Options fields are populated ONLY for confirmed status; hidden for all others
	// per options_translation.hide_options_for_statuses in strategy_rules.yaml.
	DecisionStatus string   `json:"decision_status"` // rejected | structural_candidate | entry_ready | confirmed | blocked_by_event | watch_only
	StatusLabel    string   `json:"status_label"`    // human-readable display label for the UI
	OptionsStatus  string   `json:"options_status"`  // options_not_allowed | options_ready | options_confirmed | options_hidden_until_confirmed
	Direction      string   `json:"direction"`       // call | put | none
	Score          int      `json:"score"`           // 0 when hidden per scoring.hide_score_for
	ScoreVisible   bool     `json:"score_visible"`   // false → UI must not render score bar
	FinalDecision  string   `json:"final_decision"`  // paper_trade_now | place_trigger_only | watchlist_only | reject
	ContractDTE    *int     `json:"contract_dte,omitempty"`
	ContractType   string   `json:"contract_type,omitempty"`
	ContractDelta  string   `json:"contract_delta_range,omitempty"`
	TriggerPrice   *float64 `json:"entry_trigger_price,omitempty"`
	TriggerType    string   `json:"entry_trigger_type,omitempty"`
	RiskSummary    string   `json:"risk_summary,omitempty"`
	HoldOvernight  bool     `json:"hold_overnight"`
	NextAction     string   `json:"next_action,omitempty"`
	ClaudeThesis   string   `json:"thesis_summary,omitempty"`
	ScreenReason   string   `json:"screen_reason,omitempty"`
	WhatIsMissing  string   `json:"what_is_missing,omitempty"` // human-readable for structural/blocked cards

	// Gate details for UI display
	Gates struct {
		Trend    GateDetail `json:"trend"`
		Momentum GateDetail `json:"momentum"`
		Volume   GateDetail `json:"volume"`
		VIX      GateDetail `json:"vix"`
		BTC      GateDetail `json:"btc"`
		RSI      GateDetail `json:"rsi"`
	} `json:"gates"`

	// Option chain info
	OptionCount int `json:"option_contracts_available"`

	// Full options decision JSON (for downstream / debugging)
	FullDecision *claudeclient.CandidateDecision `json:"full_decision,omitempty"`
}

// AnalysisResponse is the full JSON response for /api/daily-analysis and /api/run-analysis.
// Sections map 1:1 to ui_rules.sections in strategy_rules.yaml.
//
//	confirmed            — true trade signal, full options output allowed
//	entry_ready          — pre-open candidate, options hidden until confirmed
//	structural_candidates — watchlist only, shows family + targets + what's missing
//	blocked_by_event     — watchlist, overrides entry_ready when event blackout fires
//	watch_only           — watchlist, no active setup
//	rejected             — screened out (failed regime/family/data gates)
//
// no_trade_today fires when confirmed is empty (daily_output.no_trade_day_when_no_confirmed).
type AnalysisResponse struct {
	Date           string         `json:"date"`
	RunAt          time.Time      `json:"run_at"`
	Regime         RegimeResponse `json:"regime"`
	ActionBias     string         `json:"action_bias"`
	RegimeSummary  string         `json:"regime_summary,omitempty"`
	NoTradeToday   bool           `json:"no_trade_today"`
	NoTradeReason  string         `json:"no_trade_reason,omitempty"`
	SymbolsScanned int            `json:"symbols_scanned"`
	EligibleCount  int            `json:"eligible_count"`

	// Per-status sections (YAML ui_rules.sections order)
	Confirmed            []CandidateResponse `json:"confirmed"`
	EntryReady           []CandidateResponse `json:"entry_ready"`
	StructuralCandidates []CandidateResponse `json:"structural_candidates"`
	BlockedByEvent       []CandidateResponse `json:"blocked_by_event"`
	WatchOnly            []CandidateResponse `json:"watch_only"`
	Rejected             []CandidateResponse `json:"rejected"`

	Positions []store.PaperPosition `json:"open_positions"`
}

type RegimeResponse struct {
	VIX      float64 `json:"vix"`
	BTCROC20 float64 `json:"btc_roc20"`
	Label    string  `json:"label"`
	VIXDate  string  `json:"vix_date"`
	Warning  string  `json:"warning,omitempty"`
}

// ── POST /api/run-analysis ────────────────────────────────────────────────────

func (h *Handler) RunAnalysis(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	result, err := h.runPipeline(ctx)
	if err != nil {
		log.Printf("run-analysis error: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, result)
}

// ── POST /api/run-confirmation ────────────────────────────────────────────────

// RunConfirmation fetches 1-min opening bars for all entry_ready candidates,
// evaluates the Section N confirmation signals from strategy_rules.yaml, and
// promotes qualifying candidates to "confirmed" (or "watch_only" on failure).
// Idempotent: re-running for the same day updates existing rows.
func (h *Handler) RunConfirmation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	tradeDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	marketOpen := time.Date(now.Year(), now.Month(), now.Day(), 6, 30, 0, 0, loc)

	// Load entry_ready candidates for today
	candidates, err := store.GetEntryReadyCandidates(ctx, h.pool, tradeDate)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load entry_ready: %v", err))
		return
	}

	rules := h.rules
	if rules == nil {
		rules = strategy.DefaultRules()
	}

	if len(candidates) == 0 {
		// Nothing to confirm — return current state
		allCandidates, _ := store.GetCandidatesForDate(ctx, h.pool, tradeDate)
		summary, _ := store.GetDailySummary(ctx, h.pool, tradeDate)
		positions, _ := store.GetOpenPaperPositions(ctx, h.pool)
		writeJSON(w, buildAnalysisResponseFromDB(allCandidates, summary, positions, tradeDate))
		return
	}

	// Build ticker list (candidates + SPY for market alignment signal)
	tickers := make([]string, 0, len(candidates)+1)
	for _, c := range candidates {
		tickers = append(tickers, c.Ticker)
	}
	tickers = append(tickers, "SPY")

	windowMinutes := rules.OpenConfirmation.ConfirmationWindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = 10
	}

	barsMap, fetchErr := h.alpaca.FetchIntradayBars(tickers, marketOpen, windowMinutes)
	if fetchErr != nil {
		log.Printf("run-confirmation: intraday bars error: %v", fetchErr)
		barsMap = make(map[string][]indicators.Bar)
	}
	spyBars := barsMap["SPY"]

	confirmedCount := 0
	for _, c := range candidates {
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

		log.Printf("run-confirmation: %s → %s (signals=%d auto_reject=%v)",
			c.Ticker, result.Status, result.SignalsPassed, result.AutoRejected)

		if updateErr := store.UpdateCandidateStatus(ctx, h.pool, c.ID, result.Status); updateErr != nil {
			log.Printf("run-confirmation: update status %s: %v", c.Ticker, updateErr)
		}

		if upsertErr := store.UpsertTradeConfirmation(ctx, h.pool, store.ConfirmationStoreInput{
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
		}); upsertErr != nil {
			log.Printf("run-confirmation: persist %s: %v", c.Ticker, upsertErr)
		}

		if result.Status == "confirmed" {
			confirmedCount++

			// Auto paper entry — select contract first, then buy via execution service.
			entryPrice := result.First10Close
			if entryPrice <= 0 {
				entryPrice = c.EntryHigh
			}
			optionType := "call"
			if c.Direction == "bearish" {
				optionType = "put"
			}

			// ── Portfolio limits gate (mirrors Temporal activity) ────────────────
			pl := rules.Risk.PortfolioLimits
			if pl.MaxOpenPositions > 0 {
				if totalOpen, _ := store.GetOpenPositionCount(ctx, h.pool); totalOpen >= pl.MaxOpenPositions {
					log.Printf("run-confirmation: %s skipped (portfolio_limit: open=%d >= max=%d)", c.Ticker, totalOpen, pl.MaxOpenPositions)
					continue
				}
			}
			if pl.MaxSameDirection > 0 {
				if dirCount, _ := store.GetOpenPositionCountByDirection(ctx, h.pool, optionType); dirCount >= pl.MaxSameDirection {
					log.Printf("run-confirmation: %s skipped (direction_limit: %s open=%d >= max=%d)", c.Ticker, optionType, dirCount, pl.MaxSameDirection)
					continue
				}
			}

			// Select best contract before buying (mirrors Temporal confirmation flow).
			contractSym, contractPrice := h.selectBestContract(ctx, c.Ticker, optionType, c.ClosePrice)
			if contractPrice > 0 {
				entryPrice = contractPrice
			}

			// Premium budget gate
			if pl.MaxPremiumPctPortfolio > 0 && pl.PaperPortfolioValue > 0 {
				maxPremium := pl.PaperPortfolioValue * pl.MaxPremiumPctPortfolio / 100.0
				if entryPrice > maxPremium {
					log.Printf("run-confirmation: %s skipped (premium_budget: price=%.2f > max=%.2f)", c.Ticker, entryPrice, maxPremium)
					continue
				}
			}

			buyResult, buyErr := execution.BuyOptionPosition(ctx, h.pool, h.alpaca, execution.BuyInput{
				CandidateID:    c.ID,
				Ticker:         c.Ticker,
				SetupFamily:    c.SetupFamily,
				OptionType:     optionType,
				ContractSymbol: contractSym,
				LimitPrice:     entryPrice,
				StopLoss:       c.StopLoss,
				Target1:        c.Target1,
				Target2:        c.Target2,
			})
			if buyErr != nil {
				log.Printf("run-confirmation: buy option position %s: %v", c.Ticker, buyErr)
			} else {
				_ = store.InsertPositionEvent(ctx, h.pool, buyResult.PositionID, c.Ticker, "position_opened",
					entryPrice, map[string]any{
						"candidate_status": "confirmed",
						"setup_family":     c.SetupFamily,
						"stop_loss":        c.StopLoss,
						"target1":          c.Target1,
					})
				log.Printf("run-confirmation: paper position created %s id=%s entry=%.2f orderID=%s",
					c.Ticker, buyResult.PositionID, entryPrice, buyResult.AlpacaOrderID)
			}
		}
	}

	// Update daily_summaries.candidates_confirmed
	h.pool.Exec(ctx,
		`UPDATE daily_summaries SET candidates_confirmed=$2, updated_at=NOW() WHERE trade_date=$1`,
		tradeDate, confirmedCount,
	)

	// Return the updated analysis view
	allCandidates, _ := store.GetCandidatesForDate(ctx, h.pool, tradeDate)
	summary, _ := store.GetDailySummary(ctx, h.pool, tradeDate)
	positions, _ := store.GetOpenPaperPositions(ctx, h.pool)
	writeJSON(w, buildAnalysisResponseFromDB(allCandidates, summary, positions, tradeDate))
}

// ── POST /api/force-confirm ───────────────────────────────────────────────────

// ForceConfirm manually promotes an entry_ready candidate to confirmed and
// creates a paper position. Used when market is closed and intraday bars
// are unavailable, or for testing the end-to-end flow.
// Body: { "ticker": "RTX" }
func (h *Handler) ForceConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	var req struct {
		Ticker string `json:"ticker"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Ticker == "" {
		writeError(w, http.StatusBadRequest, "body must be {\"ticker\":\"SYMBOL\"}")
		return
	}

	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	tradeDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// Load entry_ready candidates for today
	candidates, err := store.GetEntryReadyCandidates(ctx, h.pool, tradeDate)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load entry_ready: %v", err))
		return
	}

	var found *store.Candidate
	for i := range candidates {
		if candidates[i].Ticker == req.Ticker {
			found = &candidates[i]
			break
		}
	}

	// If not in entry_ready, check if there is an open paper position (any recent date)
	// and a matching confirmed candidate — handles re-order after date rollover.
	if found == nil {
		var posID string
		var posClosePrice float64
		var posOptionType string
		h.pool.QueryRow(ctx,
			`SELECT pp.id, tc.close_price,
			        CASE WHEN tc.direction='bearish' OR lower(tc.setup_family) LIKE '%bearish%' THEN 'put' ELSE 'call' END
			 FROM paper_positions pp
			 JOIN trade_candidates tc ON tc.id = pp.candidate_id
			 WHERE pp.ticker=$1 AND pp.status='open'
			 ORDER BY pp.opened_at DESC LIMIT 1`,
			req.Ticker,
		).Scan(&posID, &posClosePrice, &posOptionType)
		if posID != "" {
			// Position already exists — only place the Alpaca order (no new DB row).
			contractSym, limitPrice := h.selectBestContract(ctx, req.Ticker, posOptionType, posClosePrice)
			alpacaOrderID := ""
			if contractSym != "" {
				alpacaOrderID, _ = h.alpaca.PlaceOptionOrder(contractSym, limitPrice)
				if alpacaOrderID != "" {
					_ = store.UpdatePositionAlpacaOrderID(ctx, h.pool, posID, alpacaOrderID)
					_ = store.UpdatePositionOptionDetails(ctx, h.pool, posID, contractSym, limitPrice)
				}
			}
			writeJSON(w, map[string]any{
				"status":          "confirmed",
				"ticker":          req.Ticker,
				"position_id":     posID,
				"option_type":     posOptionType,
				"alpaca_order_id": alpacaOrderID,
				"note":            "open position found — placed Alpaca order",
			})
			return
		}
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s not in entry_ready for today and no open position found", req.Ticker))
		return
	}

	// Promote to confirmed
	if err := store.UpdateCandidateStatus(ctx, h.pool, found.ID, "confirmed"); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("update status: %v", err))
		return
	}

	// Determine option type from direction
	optionType := "call"
	if found.Direction == "bearish" || strings.Contains(strings.ToLower(found.SetupFamily), "bearish") {
		optionType = "put"
	}

	// Use close price as entry price (best available after-hours)
	entryPrice := found.ClosePrice
	if found.EntryHigh > 0 {
		entryPrice = found.EntryHigh
	}

	// Select best contract before buying (single lifecycle path via execution service).
	contractSym, contractPrice := h.selectBestContract(ctx, found.Ticker, optionType, found.ClosePrice)
	if contractPrice > 0 {
		entryPrice = contractPrice
	}

	buyResult, buyErr := execution.BuyOptionPosition(ctx, h.pool, h.alpaca, execution.BuyInput{
		CandidateID:    found.ID,
		Ticker:         found.Ticker,
		SetupFamily:    found.SetupFamily,
		OptionType:     optionType,
		ContractSymbol: contractSym,
		LimitPrice:     entryPrice,
		StopLoss:       found.StopLoss,
		Target1:        found.Target1,
		Target2:        found.Target2,
	})
	if buyErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("buy option position: %v", buyErr))
		return
	}

	_ = store.InsertPositionEvent(ctx, h.pool, buyResult.PositionID, found.Ticker, "position_opened",
		entryPrice, map[string]any{
			"candidate_status": "confirmed",
			"setup_family":     found.SetupFamily,
			"forced":           true,
			"stop_loss":        found.StopLoss,
			"target1":          found.Target1,
		})

	// Store rationale so the DB reader can reconstruct direction/score.
	ratJSON, _ := json.Marshal(map[string]any{
		"status":         "confirmed",
		"direction":      optionType,
		"score":          70,
		"final_decision": "paper_trade_now",
		"thesis":         "Force-confirmed entry_ready candidate",
	})
	_ = store.UpdateCandidateClaudeReview(ctx, h.pool, found.ID, "paper_trade_now", 0.70, string(ratJSON))

	log.Printf("force-confirm: paper position created %s id=%s entry=%.2f opt=%s orderID=%s",
		found.Ticker, buyResult.PositionID, entryPrice, optionType, buyResult.AlpacaOrderID)

	writeJSON(w, map[string]any{
		"status":          "confirmed",
		"ticker":          found.Ticker,
		"position_id":     buyResult.PositionID,
		"entry_price":     entryPrice,
		"option_type":     optionType,
		"setup_family":    found.SetupFamily,
		"stop_loss":       found.StopLoss,
		"target1":         found.Target1,
		"target2":         found.Target2,
		"alpaca_order_id": buyResult.AlpacaOrderID,
	})
}

// ── GET /api/daily-analysis ───────────────────────────────────────────────────

func (h *Handler) DailyAnalysis(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	dateStr := r.URL.Query().Get("date")
	var date time.Time
	if dateStr == "" {
		loc, _ := time.LoadLocation("America/Los_Angeles")
		date = time.Now().In(loc).Truncate(24 * time.Hour)
	} else {
		var err error
		date, err = time.Parse("2006-01-02", dateStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid date format, use YYYY-MM-DD")
			return
		}
	}

	candidates, err := store.GetCandidatesForDate(ctx, h.pool, date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary, _ := store.GetDailySummary(ctx, h.pool, date)
	positions, _ := store.GetOpenPaperPositions(ctx, h.pool)

	resp := buildAnalysisResponseFromDB(candidates, summary, positions, date)
	writeJSON(w, resp)
}

// ── GET /api/paper-positions ──────────────────────────────────────────────────

func (h *Handler) PaperPositions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	positions, err := store.GetOpenPaperPositions(r.Context(), h.pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"positions": positions, "count": len(positions)})
}

// ── Main pipeline ─────────────────────────────────────────────────────────────

func (h *Handler) runPipeline(ctx context.Context) (AnalysisResponse, error) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	// Use PT-local date, not UTC midnight, so runs after 5 PM PT land on the correct date.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	todayStr := today.Format("2006-01-02")
	_ = today.Format("15:04") // scanTime unused — kept for reference

	log.Printf("pipeline: starting options analysis for %s", todayStr)

	// ── Step 1: Load watchlist ───────────────────────────────────────────────
	tickers, err := h.loadWatchlist(ctx)
	if err != nil {
		return AnalysisResponse{}, fmt.Errorf("load watchlist: %w", err)
	}
	log.Printf("pipeline: %d symbols in watchlist", len(tickers))

	// ── Step 2: Fetch price bars ─────────────────────────────────────────────
	startDate := today.AddDate(0, -12, 0) // 12 months for full indicator history
	barsMap, err := h.alpaca.FetchDailyBars(tickers, startDate, today, 300)
	if err != nil {
		log.Printf("pipeline: alpaca bars warning: %v", err)
	}
	log.Printf("pipeline: fetched bars for %d symbols", len(barsMap))

	spyBars := barsMap["SPY"]

	// ── Step 3: Fetch VIX from FRED ──────────────────────────────────────────
	vixLevel, vixDate, err := h.fred.FetchLatestVIX()
	if err != nil {
		log.Printf("pipeline: VIX warning: %v — using 20.0", err)
		vixLevel = 20.0
		vixDate = "unavailable"
	}
	log.Printf("pipeline: VIX = %.1f (date: %s)", vixLevel, vixDate)

	// ── Step 4: Fetch BTC 20-day ROC ─────────────────────────────────────────
	btcROC := 0.0
	btcBars, btcErr := h.alpaca.FetchCryptoDailyBars("BTC/USD", today.AddDate(0, -2, 0), 60)
	if btcErr == nil && len(btcBars) >= 21 {
		btcCloses := indicators.Closes(btcBars)
		if roc, ok := indicators.ROCLast(btcCloses, 20); ok {
			btcROC = roc
		}
	}
	log.Printf("pipeline: BTC 20d ROC = %.2f%%", btcROC)

	regime := strategy.Regime{VIX: vixLevel, BTCROC20: btcROC, VIXDate: vixDate}
	regimeLabel := strategy.RegimeLabel(vixLevel, btcROC)

	// ── Step 5: Fetch earnings calendar ──────────────────────────────────────
	earningsEvents, _ := h.finnhub.FetchUpcomingEarnings(today, today.AddDate(0, 0, 7))

	// ── Step 6: Prescreen symbols in parallel ────────────────────────────────
	type prescreenResult struct {
		ticker   string
		analysis strategy.SymbolAnalysis
		candID   string
	}

	var mu sync.Mutex
	var prescreened []prescreenResult

	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, ticker := range tickers {
		bars, ok := barsMap[ticker]
		if !ok || len(bars) == 0 {
			log.Printf("pipeline: no bars for %s — skipping", ticker)
			continue
		}
		// Suppress tickers that already have an open paper position.
		// This prevents re-suggesting a duplicate entry while the original is live.
		if hasOpen, _ := store.HasOpenPositionForTicker(ctx, h.pool, ticker); hasOpen {
			log.Printf("pipeline: skip %s — open position exists", ticker)
			continue
		}

		wg.Add(1)
		go func(t string, b []indicators.Bar) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			earningsRisk := market.HasEarningsWithin(earningsEvents, t, today, 5)
			sentiment := market.SentimentData{Symbol: t}

			a := h.engine.Analyze(t, todayStr, b, regime, spyBars, earningsRisk, sentiment)

			// Persist preprocessing result to DB
			dir := "bullish"
			if strings.Contains(strings.ToLower(a.SetupFamily), "bearish") {
				dir = "bearish"
			}
			var prevVol int64
			if len(bars) > 0 {
				prevVol = int64(bars[len(bars)-1].Volume)
			}

			candID, err := store.UpsertCandidate(ctx, h.pool, store.UpsertCandidateInput{
				TradeDate:       today,
				Ticker:          t,
				GateTrend:       a.GateTrend.Passed,
				GateMomentum:    a.GateMomentum.Passed,
				GateVolume:      a.GateVolume.Passed,
				GateVIX:         a.GateVIX.Passed,
				GateBTC:         a.GateBTC.Passed,
				GateRSI:         a.GateRSI.Passed,
				AllGates:        a.Eligible, // eligible for options review
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
			if err != nil {
				log.Printf("pipeline: upsert %s: %v", t, err)
			}

			mu.Lock()
			prescreened = append(prescreened, prescreenResult{ticker: t, analysis: a, candID: candID})
			mu.Unlock()
		}(ticker, bars)
	}
	wg.Wait()
	log.Printf("pipeline: prescreened %d symbols", len(prescreened))

	// ── Step 7: Fetch option chains for eligible symbols ─────────────────────
	type chainResult struct {
		ticker    string
		contracts []market.OptionContract
	}
	chains := make(map[string][]market.OptionContract)

	var chainWG sync.WaitGroup
	var chainMu sync.Mutex
	chainSem := make(chan struct{}, 4) // conservative — options API rate limit

	for _, r := range prescreened {
		if !r.analysis.Eligible {
			continue
		}
		chainWG.Add(1)
		go func(t string, price float64) {
			defer chainWG.Done()
			chainSem <- struct{}{}
			defer func() { <-chainSem }()

			contracts, err := h.alpaca.FetchOptionChain(t, price, todayStr)
			if err != nil {
				log.Printf("pipeline: option chain %s: %v", t, err)
				contracts = []market.OptionContract{}
			}
			chainMu.Lock()
			chains[t] = contracts
			chainMu.Unlock()
		}(r.ticker, r.analysis.ClosePrice)
	}
	chainWG.Wait()

	// ── Step 8: Build slim candidate list for Claude ────────────────────────
	lf := h.rules.OptionsTranslation.LiquidityFilters
	var slimCandidates []claudeclient.SlimCandidate
	for _, r := range prescreened {
		if !r.analysis.Eligible {
			continue
		}
		a := r.analysis
		contracts := filterChainQuality(chains[r.ticker], lf)
		slimCandidates = append(slimCandidates, claudeclient.SlimCandidate{
			T:     r.ticker,
			Fam:   a.SetupFamily,
			Score: a.PatternScoreInt,
			Px:    a.ClosePrice,
			RSI:   a.RSI14,
			MACD:  a.MACDHist,
			RVol:  a.VolumeRatio,
			Trend: a.TrendBias,
			T1:    a.BaseTarget,
			T2:    a.StretchTarget,
			RC:    a.ReasonCodes,
			Earn:  a.EarningsRisk,
			Opts:  len(contracts) > 0,
		})
	}

	// ── Step 9: Call Claude (single slim call — all candidates, ~80 chars each) ─
	claudeCli := claudeclient.NewClient(
		h.cfg.AnthropicAPIKey,
		"claude-sonnet-4-6",
		h.cfg.ClaudeMaxOutputTokens,
		"", // system prompt embedded in ReviewCandidates
	)

	spyRegime := deriveTrendBias(barsMap["SPY"])
	log.Printf("pipeline: calling Claude with %d candidates", len(slimCandidates))
	slimResp, claudeErr := claudeCli.ReviewCandidates(vixLevel, btcROC, spyRegime, slimCandidates)
	if claudeErr != nil {
		log.Printf("pipeline: Claude error: %v", claudeErr)
	}

	// ── Step 10: Build decision lookup from slim response ─────────────────────
	type miniDecision struct {
		status string
		dir    string
		score  int
		fd     string
		why    string
		rc     []string
	}
	decisionByTicker := make(map[string]miniDecision)
	for _, r := range slimResp.C {
		decisionByTicker[r.T] = miniDecision{
			status: r.Status, dir: r.Dir, score: r.Score,
			fd: r.FD, why: r.Why, rc: r.RC,
		}
	}
	actionBias := slimResp.Bias
	regimeSummary := slimResp.Regime

	// Per-status buckets matching YAML ui_rules.sections.
	var confirmed []CandidateResponse
	var entryReady []CandidateResponse
	var structuralCandidates []CandidateResponse
	var blockedByEvent []CandidateResponse
	var watchOnly []CandidateResponse
	var rejectedItems []CandidateResponse

	for _, r := range prescreened {
		cr := toCandidateResponse(r.analysis)
		cr.OptionCount = len(chains[r.ticker])

		// blocked_by_event is ineligible for Claude but has its own UI section.
		if r.analysis.CandidateStatus == "blocked_by_event" {
			cr.DecisionStatus = "blocked_by_event"
			cr.FinalDecision = "watchlist_only"
			cr.WhatIsMissing = whatIsMissingForBlocked(r.analysis.ReasonCodes)
			cr = applyScoreVisibility(cr, h.rules)
			cr = stripOptionsOutput(cr)
			blockedByEvent = append(blockedByEvent, cr)
			continue
		}

		// All other ineligible symbols → rejected.
		if !r.analysis.Eligible {
			status := r.analysis.CandidateStatus
			if status == "" {
				status = "rejected"
			}
			cr.DecisionStatus = status
			cr.FinalDecision = "reject"
			cr = applyScoreVisibility(cr, h.rules)
			rejectedItems = append(rejectedItems, cr)
			continue
		}

		// Eligible symbol — apply Claude slim decision if available.
		md, hasDecision := decisionByTicker[r.ticker]
		if hasDecision {
			cr.DecisionStatus = md.status
			if cr.DecisionStatus == "" {
				cr.DecisionStatus = "structural_candidate"
			}
			// Use Claude's direction; fall back to family direction if none.
			cr.Direction = md.dir
			if cr.Direction == "" || cr.Direction == "none" {
				cr.Direction = familyDirection(r.analysis.SetupFamily)
			}
			cr.Score = md.score
			cr.FinalDecision = md.fd
			cr.ClaudeThesis = md.why

			// Persist Claude decision to DB.
			if r.candID != "" {
				decisionJSON, _ := json.Marshal(map[string]interface{}{
					"status": md.status, "direction": md.dir, "score": md.score,
					"final_decision": md.fd, "thesis": md.why, "reason_codes": md.rc,
				})
				// Map final_decision to the DB-allowed claude_action enum values.
				dbAction := finalDecisionToDBAction(md.fd)
				if err := store.UpdateCandidateClaudeReview(ctx, h.pool, r.candID,
					dbAction, float64(md.score)/100.0, string(decisionJSON),
				); err != nil {
					log.Printf("pipeline: persist decision %s: %v", r.ticker, err)
				}
				// Also persist Claude's candidate_status (e.g. entry_ready) to the DB row.
				if md.status != "" {
					if err := store.UpdateCandidateStatus(ctx, h.pool, r.candID, md.status); err != nil {
						log.Printf("pipeline: persist status %s: %v", r.ticker, err)
					}
				}
			}
		} else {
			// No Claude decision — fall back to engine status.
			cr.DecisionStatus = r.analysis.CandidateStatus
			if cr.DecisionStatus == "" {
				cr.DecisionStatus = "structural_candidate"
			}
			cr.FinalDecision = "watchlist_only"
		}

		// Apply score visibility and options gating from YAML.
		cr = applyScoreVisibility(cr, h.rules)

		// Route to the correct per-status bucket.
		switch cr.DecisionStatus {
		case "confirmed":
			// confirmed keeps full options output.
			confirmed = append(confirmed, cr)
		case "entry_ready":
			// Preserve direction before stripping (needed for confirmation flow)
			savedDir := cr.Direction
			cr = stripOptionsOutput(cr)
			cr.Direction = savedDir
			cr.WhatIsMissing = "Awaiting open confirmation (first 10 minutes)"
			entryReady = append(entryReady, cr)
		case "structural_candidate":
			cr = stripOptionsOutput(cr)
			cr.WhatIsMissing = whatIsMissingForStructural(r.analysis.ReasonCodes)
			structuralCandidates = append(structuralCandidates, cr)
		case "watch_only":
			cr = stripOptionsOutput(cr)
			watchOnly = append(watchOnly, cr)
		default:
			// rejected or unknown
			rejectedItems = append(rejectedItems, cr)
		}
	}

	// ── Step 11: Build and persist daily summary ──────────────────────────────
	// no_trade_today fires when zero confirmed names exist per
	// daily_output.no_trade_day_when_no_confirmed in strategy_rules.yaml.
	noTrade := len(confirmed) == 0
	noTradeReason := ""
	if noTrade {
		noTradeReason = h.rules.NoTradeDayMessage()
	}

	if actionBias == "" {
		actionBias = "no_trade_bias"
	}

	// eligible_count = anything that matched a family (structural, entry_ready, confirmed, blocked_by_event)
	eligibleCount := len(confirmed) + len(entryReady) + len(structuralCandidates) + len(blockedByEvent) + len(watchOnly)

	// watchTickers = structural + blocked + watch_only for daily summary
	var watchTickers []string
	for _, c := range structuralCandidates {
		watchTickers = append(watchTickers, c.Ticker)
	}
	for _, c := range blockedByEvent {
		watchTickers = append(watchTickers, c.Ticker)
	}
	for _, c := range watchOnly {
		watchTickers = append(watchTickers, c.Ticker)
	}

	if regimeSummary == "" {
		regimeSummary = fmt.Sprintf("VIX %.1f, BTC ROC %.1f%%, regime %s", vixLevel, btcROC, regimeLabel)
	}

	summary := store.DailySummary{
		TradeDate:         today,
		VIXLevel:          vixLevel,
		BTCROC20:          btcROC,
		RegimeLabel:       regimeLabel,
		SymbolsScanned:    len(prescreened),
		CandidatesFound:   len(confirmed),
		NoTradeToday:      noTrade,
		NoTradeReason:     noTradeReason,
		RegimeSummary:     regimeSummary,
		WatchTickers:      watchTickers,
		AnalysisCompleted: true,
	}
	if err := store.UpsertDailySummary(ctx, h.pool, summary); err != nil {
		log.Printf("pipeline: upsert daily summary: %v", err)
	}

	positions, _ := store.GetOpenPaperPositions(ctx, h.pool)

	log.Printf("pipeline: complete — %d scanned, %d eligible, %d confirmed, %d entry_ready, %d structural, %d blocked, %d watch_only",
		len(prescreened), eligibleCount, len(confirmed), len(entryReady), len(structuralCandidates), len(blockedByEvent), len(watchOnly))

	return AnalysisResponse{
		Date:  todayStr,
		RunAt: time.Now(),
		Regime: RegimeResponse{
			VIX:      vixLevel,
			BTCROC20: btcROC,
			Label:    regimeLabel,
			VIXDate:  vixDate,
			Warning:  vixWarning(vixLevel),
		},
		ActionBias:           actionBias,
		RegimeSummary:        regimeSummary,
		NoTradeToday:         noTrade,
		NoTradeReason:        noTradeReason,
		SymbolsScanned:       len(prescreened),
		EligibleCount:        eligibleCount,
		Confirmed:            nullSlice(confirmed),
		EntryReady:           nullSlice(entryReady),
		StructuralCandidates: nullSlice(structuralCandidates),
		BlockedByEvent:       nullSlice(blockedByEvent),
		WatchOnly:            nullSlice(watchOnly),
		Rejected:             nullSlice(rejectedItems),
		Positions:            positions,
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (h *Handler) loadWatchlist(ctx context.Context) ([]string, error) {
	rows, err := h.pool.Query(ctx, `SELECT ticker FROM symbols WHERE is_active=TRUE ORDER BY ticker`)
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
// Delegates to market.FilterChainQuality so both handlers and activities share logic.
func filterChainQuality(contracts []market.OptionContract, lf strategy.LiquidityFilters) []market.OptionContract {
	return market.FilterChainQuality(contracts, lf.MinOpenInterest, lf.MinOptionVolume, lf.MaxBidAskSpreadPctOfMid)
}

// candidateStatusLabel returns a human-readable UI label for a lifecycle status.
// Used in status_label field of CandidateResponse.
func candidateStatusLabel(status string) string {
	switch status {
	case "entry_ready":
		return "WAITING FOR CLAUDE CONFIRMATION"
	case "confirmed":
		return "PAPER POSITION OPEN"
	case "structural_candidate":
		return "WATCHLIST"
	case "watch_only":
		return "WATCH ONLY"
	case "blocked_by_event":
		return "BLOCKED (EVENT)"
	case "rejected":
		return "REJECTED"
	default:
		return strings.ToUpper(status)
	}
}

func toCandidateResponse(a strategy.SymbolAnalysis) CandidateResponse {
	cr := CandidateResponse{
		Ticker:       a.Ticker,
		Eligible:     a.Eligible,
		TrendBias:    a.TrendBias,
		ClosePrice:   a.ClosePrice,
		EMA20:        a.EMA20,
		EMA50:        a.EMA50,
		RSI14:        a.RSI14,
		MACDHist:     a.MACDHist,
		VolumeRatio:  a.VolumeRatio,
		PatternName:  a.PatternName,
		AntiPatterns: a.AntiPatterns,
		ScreenReason: a.ScreenReason,
		// v2 fields
		SetupFamily:    a.SetupFamily,
		ReasonCodes:    a.ReasonCodes,
		BaseTarget:     a.BaseTarget,
		StretchTarget:  a.StretchTarget,
		DecisionStatus: a.CandidateStatus,
		StatusLabel:    candidateStatusLabel(a.CandidateStatus),
	}
	cr.Gates.Trend = GateDetail{Passed: a.GateTrend.Passed, Reason: a.GateTrend.Reason, Value: a.GateTrend.Value}
	cr.Gates.Momentum = GateDetail{Passed: a.GateMomentum.Passed, Reason: a.GateMomentum.Reason, Value: a.GateMomentum.Value}
	cr.Gates.Volume = GateDetail{Passed: a.GateVolume.Passed, Reason: a.GateVolume.Reason, Value: a.GateVolume.Value}
	cr.Gates.VIX = GateDetail{Passed: a.GateVIX.Passed, Reason: a.GateVIX.Reason, Value: a.GateVIX.Value}
	cr.Gates.BTC = GateDetail{Passed: a.GateBTC.Passed, Reason: a.GateBTC.Reason, Value: a.GateBTC.Value}
	cr.Gates.RSI = GateDetail{Passed: a.GateRSI.Passed, Reason: a.GateRSI.Reason, Value: a.GateRSI.Value}
	return cr
}

// applyDecisionToResponse merges a Claude CandidateDecision into a CandidateResponse.
func applyDecisionToResponse(cr *CandidateResponse, d *claudeclient.CandidateDecision) {
	cr.DecisionStatus = d.Status
	cr.StatusLabel = candidateStatusLabel(d.Status)
	cr.Direction = d.Direction
	cr.Score = d.Score
	cr.FinalDecision = d.FinalDecision
	cr.HoldOvernight = d.HoldOvernight.Allowed
	cr.NextAction = d.DailyReview.NextActionIfOpen
	cr.FullDecision = d

	// v2 fields from Claude decision
	if d.SetupFamily != "" {
		cr.SetupFamily = d.SetupFamily
	}
	if d.OptionsStatus != "" {
		cr.OptionsStatus = d.OptionsStatus
	}
	if len(d.ReasonCodes) > 0 {
		cr.ReasonCodes = d.ReasonCodes
	}

	if d.Contract.Selected {
		cr.ContractType = d.Contract.Type
		cr.ContractDTE = d.Contract.ExpirationDTE
		if d.Contract.TargetDeltaRange != nil {
			cr.ContractDelta = *d.Contract.TargetDeltaRange
		}
	}
	if d.EntryTrigger.UnderlyingTriggerPrice != nil {
		cr.TriggerPrice = d.EntryTrigger.UnderlyingTriggerPrice
		cr.TriggerType = d.EntryTrigger.Type
	}

	cr.RiskSummary = fmt.Sprintf("Stop: %.0f%% | Profit zone: %s | Trail: %s",
		d.RiskPlan.InitialStopLossPct,
		d.RiskPlan.FirstProfitZonePct,
		d.RiskPlan.TrailActivationPct,
	)

	// Base / stretch targets from Claude targets section
	if d.Targets.Base != nil {
		cr.BaseTarget = *d.Targets.Base
	}
	if d.Targets.Stretch != nil {
		cr.StretchTarget = *d.Targets.Stretch
	}

	var parts []string
	if d.Thesis.TrendStructure != "" {
		parts = append(parts, d.Thesis.TrendStructure)
	}
	if d.Thesis.CatalystSentiment != "" {
		parts = append(parts, d.Thesis.CatalystSentiment)
	}
	cr.ClaudeThesis = strings.Join(parts, " | ")
}

// stripOptionsOutput removes all contract/trigger/options fields from a CandidateResponse.
// Called for every status except "confirmed" per hide_options_for_statuses in strategy_rules.yaml.
func stripOptionsOutput(cr CandidateResponse) CandidateResponse {
	cr.ContractDTE = nil
	cr.ContractType = ""
	cr.ContractDelta = ""
	cr.TriggerPrice = nil
	cr.TriggerType = ""
	cr.RiskSummary = ""
	cr.HoldOvernight = false
	cr.OptionsStatus = "options_hidden_until_confirmed"
	// Keep direction visible for entry_ready so confirmation knows call vs put.
	// Only strip direction for statuses that have no directional signal yet.
	// Strip the full decision object so contract/entry data doesn't leak through JSON.
	if cr.FullDecision != nil {
		stripped := *cr.FullDecision
		stripped.Contract = claudeclient.ContractDecision{Selected: false, Type: "none"}
		stripped.EntryTrigger = claudeclient.EntryTrigger{}
		stripped.RiskPlan = claudeclient.RiskPlan{}
		cr.FullDecision = &stripped
	}
	return cr
}

// applyScoreVisibility zeros the score for statuses that must not show it,
// per scoring.hide_score_for in strategy_rules.yaml.
func applyScoreVisibility(cr CandidateResponse, rules *strategy.Rules) CandidateResponse {
	if rules.ShouldHideScore(cr.DecisionStatus) {
		cr.Score = 0
		cr.ScoreVisible = false
	} else {
		cr.ScoreVisible = true
	}
	return cr
}

// whatIsMissingForStructural produces a short human-readable explanation
// of why the symbol is structural_candidate and not entry_ready.
func whatIsMissingForStructural(codes []string) string {
	codeSet := make(map[string]bool, len(codes))
	for _, c := range codes {
		codeSet[c] = true
	}
	var parts []string
	if codeSet["volume_weak"] {
		parts = append(parts, "volume expansion needed")
	}
	if codeSet["rsi_extended"] {
		parts = append(parts, "RSI overbought — wait for pullback")
	}
	if codeSet["rsi_too_weak"] {
		parts = append(parts, "RSI too weak — momentum absent")
	}
	if codeSet["entry_too_extended"] {
		parts = append(parts, "entry location extended past YAML limit")
	}
	if codeSet["reward_risk_poor"] {
		parts = append(parts, "reward/risk below 2.0x minimum")
	}
	if len(parts) == 0 {
		return "Entry conditions not yet met"
	}
	return strings.Join(parts, "; ")
}

// whatIsMissingForBlocked produces a human-readable reason for blocked_by_event.
func whatIsMissingForBlocked(codes []string) string {
	for _, c := range codes {
		switch c {
		case "event_blackout_earnings":
			return "Earnings within blackout window (5 days)"
		case "event_blackout_binary":
			return "Binary event within blackout window (3 days)"
		}
	}
	return "Event blackout active"
}

// familyDirection returns "call", "put", or "none" based on setup family name.
func familyDirection(family string) string {
	switch family {
	case "bullish_continuation", "bullish_momentum_breakout":
		return "call"
	case "bearish_continuation", "bearish_momentum_breakdown":
		return "put"
	}
	return "none"
}

func deriveTrendBias(bars []indicators.Bar) string {
	if len(bars) < 55 {
		return "mixed"
	}
	closes := indicators.Closes(bars)
	ema20, ok20 := indicators.EMALast(closes, 20)
	ema50, ok50 := indicators.EMALast(closes, 50)
	if !ok20 || !ok50 {
		return "mixed"
	}
	if ema20 > ema50 {
		return "bullish"
	}
	if ema20 < ema50 {
		return "bearish"
	}
	return "mixed"
}

func deriveBreadthTone(vix, btcROC float64) string {
	if vix >= 28 {
		return "risk_off"
	}
	if vix >= 22 && btcROC < 0 {
		return "cautious"
	}
	if vix < 18 && btcROC > 5 {
		return "strongly_positive"
	}
	if btcROC > 0 {
		return "moderately_positive"
	}
	return "neutral"
}

func buildRegimeSummary(vix, btcROC float64, label string, decision claudeclient.OptionsDecision) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("VIX %.1f", vix))
	if btcROC >= 0 {
		parts = append(parts, fmt.Sprintf("BTC ROC +%.1f%%", btcROC))
	} else {
		parts = append(parts, fmt.Sprintf("BTC ROC %.1f%%", btcROC))
	}
	parts = append(parts, fmt.Sprintf("Regime: %s", label))
	if decision.DailySummary.Notes != "" {
		parts = append(parts, decision.DailySummary.Notes)
	}
	return strings.Join(parts, " | ")
}

func vixWarning(vix float64) string {
	if vix >= 28 {
		return fmt.Sprintf("High VIX %.1f — elevated premium, tight spreads required", vix)
	}
	if vix >= 22 {
		return fmt.Sprintf("Elevated VIX %.1f — prefer smaller size and tighter stops", vix)
	}
	return ""
}

func tickersFromCandidates(items []CandidateResponse) []string {
	tickers := make([]string, 0, len(items))
	for _, c := range items {
		tickers = append(tickers, c.Ticker)
	}
	return tickers
}

func nullSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// buildAnalysisResponseFromDB reconstructs a response from stored DB records.
// Uses the same per-status routing as runPipeline so the GET endpoint
// returns the same structure as POST.
func buildAnalysisResponseFromDB(candidates []store.Candidate, summary *store.DailySummary, positions []store.PaperPosition, date time.Time) AnalysisResponse {
	resp := AnalysisResponse{
		Date:      date.Format("2006-01-02"),
		RunAt:     time.Now(),
		Positions: positions,
	}

	if summary != nil {
		resp.Regime = RegimeResponse{
			VIX:      summary.VIXLevel,
			BTCROC20: summary.BTCROC20,
			Label:    summary.RegimeLabel,
			Warning:  vixWarning(summary.VIXLevel),
		}
		resp.NoTradeToday = summary.NoTradeToday
		resp.NoTradeReason = summary.NoTradeReason
		resp.SymbolsScanned = summary.SymbolsScanned
		resp.RegimeSummary = summary.RegimeSummary
	}

	// Load rules for score/options visibility — fall back to defaults if YAML unavailable.
	rules, err := strategy.LoadRules("strategy_rules.yaml")
	if err != nil {
		rules = strategy.DefaultRules()
	}

	for _, c := range candidates {
		cr := CandidateResponse{
			Ticker:       c.Ticker,
			Eligible:     c.AllGatesPassed,
			ClosePrice:   c.ClosePrice,
			EMA20:        c.EMA20,
			RSI14:        c.RSI14,
			MACDHist:     c.MACDHist,
			VolumeRatio:  c.VolumeRatio,
			PatternName:  c.PatternName,
			ScreenReason: c.RejectReason,
			// Populate family + targets from engine-computed DB columns so cards
			// render correctly even for rows without a Claude decision.
			SetupFamily:   c.SetupFamily,
			BaseTarget:    c.Target1,
			StretchTarget: c.Target2,
		}
		cr.Gates.Trend = GateDetail{Passed: c.GateTrend}
		cr.Gates.Momentum = GateDetail{Passed: c.GateMomentum}
		cr.Gates.Volume = GateDetail{Passed: c.GateVolume}
		cr.Gates.VIX = GateDetail{Passed: c.GateVIX}
		cr.Gates.BTC = GateDetail{Passed: c.GateBTC}
		cr.Gates.RSI = GateDetail{Passed: c.GateRSI}

		// Re-parse stored Claude decision JSON (eligible rows only).
		// Try slim format first ({"status","direction","score","thesis",...}).
		// Fall back to full CandidateDecision if slim parse yields no data.
		if c.ClaudeRationale != "" && strings.HasPrefix(strings.TrimSpace(c.ClaudeRationale), "{") {
			var slim struct {
				Status    string   `json:"status"`
				Direction string   `json:"direction"`
				Score     int      `json:"score"`
				FD        string   `json:"final_decision"`
				Thesis    string   `json:"thesis"`
				RC        []string `json:"reason_codes"`
			}
			if err := json.Unmarshal([]byte(c.ClaudeRationale), &slim); err == nil && slim.Status != "" {
				cr.DecisionStatus = slim.Status
				cr.Direction = slim.Direction
				if cr.Direction == "" || cr.Direction == "none" {
					cr.Direction = familyDirection(c.SetupFamily)
				}
				cr.Score = slim.Score
				cr.FinalDecision = slim.FD
				cr.ClaudeThesis = slim.Thesis
			} else {
				var cd claudeclient.CandidateDecision
				if err2 := json.Unmarshal([]byte(c.ClaudeRationale), &cd); err2 == nil {
					applyDecisionToResponse(&cr, &cd)
				}
			}
		}

		// Authoritative status: prefer Claude's decision status (set above);
		// fall back to the engine-computed candidate_status column from the DB.
		// Never default to a hardcoded string — that would misroute blocked/entry rows.
		if cr.DecisionStatus == "" {
			cr.DecisionStatus = c.CandidateStatus
			if cr.DecisionStatus == "" {
				cr.DecisionStatus = "rejected" // absolute fallback for rows with no status
			}
			cr.FinalDecision = c.ClaudeAction
		}
		// Always ensure status_label reflects the authoritative DecisionStatus.
		cr.StatusLabel = candidateStatusLabel(cr.DecisionStatus)

		// Direction: if not set from Claude rationale, derive from DB direction col or family.
		if cr.Direction == "" || cr.Direction == "none" {
			switch c.Direction {
			case "bullish":
				cr.Direction = "call"
			case "bearish":
				cr.Direction = "put"
			default:
				cr.Direction = familyDirection(c.SetupFamily)
			}
		}

		// Score: if not set from Claude rationale, use claude_confidence * 100.
		if cr.Score == 0 && c.ClaudeConf > 0 {
			cr.Score = int(c.ClaudeConf * 100)
		}

		// Apply YAML score / options visibility rules.
		cr = applyScoreVisibility(cr, rules)

		// Route to per-status bucket; fill WhatIsMissing for watchlist cards.
		switch cr.DecisionStatus {
		case "confirmed":
			resp.Confirmed = append(resp.Confirmed, cr)
			resp.EligibleCount++
		case "entry_ready":
			cr = stripOptionsOutput(cr)
			if cr.WhatIsMissing == "" {
				cr.WhatIsMissing = "Awaiting open confirmation (first 10 minutes)"
			}
			resp.EntryReady = append(resp.EntryReady, cr)
			resp.EligibleCount++
		case "structural_candidate":
			cr = stripOptionsOutput(cr)
			if cr.WhatIsMissing == "" {
				cr.WhatIsMissing = whatIsMissingForStructural(cr.ReasonCodes)
			}
			resp.StructuralCandidates = append(resp.StructuralCandidates, cr)
			resp.EligibleCount++
		case "blocked_by_event":
			cr = stripOptionsOutput(cr)
			if cr.WhatIsMissing == "" {
				cr.WhatIsMissing = whatIsMissingForBlocked(cr.ReasonCodes)
			}
			resp.BlockedByEvent = append(resp.BlockedByEvent, cr)
			resp.EligibleCount++
		case "watch_only":
			cr = stripOptionsOutput(cr)
			resp.WatchOnly = append(resp.WatchOnly, cr)
			resp.EligibleCount++
		default:
			resp.Rejected = append(resp.Rejected, cr)
		}
	}

	resp.Confirmed = nullSlice(resp.Confirmed)
	resp.EntryReady = nullSlice(resp.EntryReady)
	resp.StructuralCandidates = nullSlice(resp.StructuralCandidates)
	resp.BlockedByEvent = nullSlice(resp.BlockedByEvent)
	resp.WatchOnly = nullSlice(resp.WatchOnly)
	resp.Rejected = nullSlice(resp.Rejected)
	return resp
}

// finalDecisionToDBAction maps Claude's final_decision string to the
// claude_action enum allowed by the trade_candidates DB constraint:
// BUY | WATCH | INVALID | PENDING | SKIPPED
func finalDecisionToDBAction(fd string) string {
	switch fd {
	case "paper_trade_now":
		return "BUY"
	case "place_trigger_only", "watchlist_only":
		return "WATCH"
	case "reject":
		return "INVALID"
	case "":
		return "PENDING"
	default:
		return "WATCH"
	}
}

// selectBestContract fetches the option chain for a ticker, applies quality
// filters, and returns the best contract symbol and limit price. Returns ("", 0)
// if no qualifying contract is found. Used by API buy paths before delegating
// to execution.BuyOptionPosition.
func (h *Handler) selectBestContract(ctx context.Context, ticker, optionType string, underlyingPrice float64) (string, float64) {
	todayStr := time.Now().Format("2006-01-02")
	contracts, err := h.alpaca.FetchOptionChain(ticker, underlyingPrice, todayStr)
	if err != nil {
		log.Printf("select-contract: fetch chain %s: %v", ticker, err)
		return "", 0
	}

	lf := h.rules.OptionsTranslation.LiquidityFilters
	qualified := filterChainQuality(contracts, lf)

	// Use global risk DTE defaults for the API-path contract selector (no family context here).
	selOpts := market.ContractSelectionOpts{
		DTEMin:        h.rules.Risk.OptionLifecycle.DTEMin,
		DTEMax:        h.rules.Risk.OptionLifecycle.DTEMax,
		TargetDTE:     h.rules.Risk.OptionLifecycle.TargetDTE,
		AvoidDTEBelow: h.rules.Risk.OptionLifecycle.AvoidDTEBelow,
	}
	best := market.SelectBestContract(qualified, optionType, selOpts)
	if best == nil {
		log.Printf("select-contract: no qualifying %s contract for %s (chain=%d qualified=%d dteRange=%d-%d)",
			optionType, ticker, len(contracts), len(qualified), selOpts.DTEMin, selOpts.DTEMax)
		return "", 0
	}

	limitPrice := (best.Bid + best.Ask) / 2.0
	if best.Bid <= 0 {
		limitPrice = best.Ask
	}

	log.Printf("select-contract: %s %s %s strike=%.2f dte=%d delta=%.3f limit=%.2f",
		ticker, optionType, best.Symbol, best.Strike, best.DTE, best.Delta, limitPrice)
	return best.Symbol, limitPrice
}

// RunPositionReview runs the position review immediately (same logic as the
// Temporal activity at 20:45 UTC). Useful for manual triggers and testing.
// POST /api/run-position-review
func (h *Handler) RunPositionReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	reviewDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	positions, err := store.GetOpenPositionsForReview(ctx, h.pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load positions: %v", err))
		return
	}
	if len(positions) == 0 {
		writeJSON(w, map[string]any{"reviewed": 0, "exited": 0, "note": "no open positions"})
		return
	}

	type enriched struct {
		pos          store.ReviewablePosition
		currentPrice float64
		pnlPct       float64
		daysHeld     int
	}
	var items []enriched
	var posInputs []claudeclient.PositionInput

	for _, p := range positions {
		pnlPct := 0.0
		currentPrice := 0.0

		if p.OptionSymbol != "" && p.OptionPremium > 0 {
			midPrice, midErr := h.alpaca.FetchOptionMidPrice(p.OptionSymbol)
			if midErr != nil {
				log.Printf("position-review: option mid %s (%s): %v — stock fallback", p.Ticker, p.OptionSymbol, midErr)
				stockPrice, _ := h.alpaca.FetchLatestQuote(p.Ticker)
				if stockPrice <= 0 {
					stockPrice = p.EntryPrice
				}
				currentPrice = stockPrice
				if p.EntryPrice > 0 {
					move := (stockPrice - p.EntryPrice) / p.EntryPrice * 100.0
					if p.OptionType == "put" {
						pnlPct = -move
					} else {
						pnlPct = move
					}
				}
			} else {
				currentPrice = midPrice
				pnlPct = (midPrice - p.OptionPremium) / p.OptionPremium * 100.0
				log.Printf("position-review: option P&L %s %s: mid=%.2f premium=%.2f pnl=%.1f%%",
					p.Ticker, p.OptionSymbol, midPrice, p.OptionPremium, pnlPct)
			}
		} else {
			stockPrice, _ := h.alpaca.FetchLatestQuote(p.Ticker)
			if stockPrice <= 0 {
				stockPrice = p.EntryPrice
			}
			currentPrice = stockPrice
			if p.EntryPrice > 0 {
				move := (stockPrice - p.EntryPrice) / p.EntryPrice * 100.0
				if p.OptionType == "put" {
					pnlPct = -move
				} else {
					pnlPct = move
				}
			}
		}

		daysHeld := int(now.Sub(p.EntryDate.UTC()).Hours() / 24)
		dte := 14 - daysHeld
		if dte < 0 {
			dte = 0
		}
		items = append(items, enriched{pos: p, currentPrice: currentPrice, pnlPct: pnlPct, daysHeld: daysHeld})
		posInputs = append(posInputs, claudeclient.PositionInput{
			Ticker:     p.Ticker,
			OptionType: p.OptionType,
			EntryPrice: p.OptionPremium,
			CurrentPnL: pnlPct,
			DTE:        dte,
			Status:     "open",
		})
	}

	vixLevel, _, _ := h.fred.FetchLatestVIX()
	systemPrompt := claudeclient.BuildSystemPrompt()
	claudeCli := claudeclient.NewClient(h.cfg.AnthropicAPIKey, "claude-sonnet-4-6", h.cfg.ClaudeMaxOutputTokens, systemPrompt)
	payload := claudeclient.RuntimePayload{
		ScanTimePT:    now.Format("15:04"),
		MarketContext: claudeclient.MarketContext{VIX: vixLevel, MacroNewsBias: "neutral"},
		OpenPositions: posInputs,
		Candidates:    []claudeclient.CandidateInput{},
	}
	decision, claudeErr := claudeCli.DecideOptions(payload)
	reviewByTicker := make(map[string]claudeclient.PositionReview)
	if claudeErr != nil {
		log.Printf("position-review: Claude error: %v — defaulting to HOLD", claudeErr)
	} else {
		for _, rv := range decision.OpenPositionReview {
			reviewByTicker[rv.Ticker] = rv
		}
	}

	type reviewResult struct {
		Ticker string  `json:"ticker"`
		PnLPct float64 `json:"pnl_pct"`
		Action string  `json:"action"`
		Reason string  `json:"reason"`
		Exited bool    `json:"exited"`
	}
	var results []reviewResult
	exitedCount := 0

	for _, e := range items {
		p := e.pos
		action := "HOLD"
		rationale := "defaulted to HOLD"
		if rv, ok := reviewByTicker[p.Ticker]; ok {
			action = mapReviewAction(rv.Status)
			rationale = rv.Reason
		}
		executed := false
		if action == "EXIT" {
			// Sell via shared execution service. Position stays open if sell fails.
			_, sellErr := execution.SellOptionPosition(ctx, h.pool, h.alpaca, execution.SellInput{
				PositionID:     p.ID,
				Ticker:         p.Ticker,
				ContractSymbol: p.OptionSymbol,
				SellPrice:      e.currentPrice, // 0 → SellOptionPosition fetches mid
				PnLPct:         e.pnlPct,
				ExitReason:     "review_exit",
			})
			if sellErr != nil {
				log.Printf("position-review: sell option position %s: %v — keeping open", p.Ticker, sellErr)
				_ = store.InsertPositionEvent(ctx, h.pool, p.ID, p.Ticker, "sell_failed",
					e.currentPrice, map[string]any{"error": sellErr.Error(), "pnl_pct": e.pnlPct})
			} else {
				_ = store.InsertPositionEvent(ctx, h.pool, p.ID, p.Ticker, "position_closed",
					e.currentPrice, map[string]any{"reason": "review_exit", "pnl_pct": e.pnlPct})
				executed = true
				exitedCount++
				log.Printf("position-review: closed %s pnl=%.2f%%", p.Ticker, e.pnlPct)
			}
		}
		_ = store.UpsertPositionReview(ctx, h.pool, store.PositionReviewInput{
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
		results = append(results, reviewResult{
			Ticker: p.Ticker,
			PnLPct: e.pnlPct,
			Action: action,
			Reason: rationale,
			Exited: executed,
		})
	}
	writeJSON(w, map[string]any{
		"reviewed": len(results),
		"exited":   exitedCount,
		"results":  results,
	})
}

// mapReviewAction converts Claude's position status to a DB action.
// partial_profit maps to EXIT: paper trades use 1 contract and cannot be split.
func mapReviewAction(claudeStatus string) string {
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

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON error: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
