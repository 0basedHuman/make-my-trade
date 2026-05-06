// internal/store/store.go
//
// WHAT: All Postgres operations for the new lifecycle tables.
//       Wraps pgxpool queries behind clean function signatures.
//
// WHY:  Keeps database logic out of HTTP handlers and strategy code.
//       Every function maps 1:1 to a single business operation.
//       Callers don't write SQL — they call functions with typed parameters.
//
// HOW:  All functions receive context.Context and *pgxpool.Pool.
//       UpsertCandidate uses ON CONFLICT (trade_date, ticker) DO UPDATE
//       so re-running analysis for the same day is idempotent.
//
// WHAT BREAKS: If the migration hasn't run, table-not-found errors appear here.
//              The server startup runs migrations before opening the pool, so
//              this should never happen in practice.
//
// VERIFY: After /api/run-analysis, run:
//   psql $DB_URL -c "SELECT ticker, all_gates_passed, claude_action FROM trade_candidates WHERE trade_date = CURRENT_DATE ORDER BY all_gates_passed DESC;"

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── trade_candidates ─────────────────────────────────────────────────────────

// Candidate mirrors the trade_candidates table row.
type Candidate struct {
	ID        string    `json:"id"`
	TradeDate time.Time `json:"trade_date"`
	Ticker    string    `json:"ticker"`
	CreatedAt time.Time `json:"created_at"`

	GateTrend      bool `json:"gate_trend"`
	GateMomentum   bool `json:"gate_momentum"`
	GateVolume     bool `json:"gate_volume"`
	GateVIX        bool `json:"gate_vix"`
	GateBTC        bool `json:"gate_btc"`
	GateRSI        bool `json:"gate_rsi"`
	AllGatesPassed bool `json:"all_gates_passed"`

	ClosePrice  float64 `json:"close_price"`
	EMA20       float64 `json:"ema20"`
	EMA100      float64 `json:"ema100"`
	RSI14       float64 `json:"rsi14"`
	MACDHist    float64 `json:"macd_hist"`
	VolumeRatio float64 `json:"volume_ratio"`
	VIXLevel    float64 `json:"vix_level"`
	BTCROC20    float64 `json:"btc_roc20"`

	PatternName    string   `json:"pattern_name"`
	PatternScore   float64  `json:"pattern_score"`
	AntiPatterns   []string `json:"anti_patterns"`
	RejectedByAnti bool     `json:"rejected_by_anti"`

	EntryLow  float64 `json:"entry_low"`
	EntryHigh float64 `json:"entry_high"`
	StopLoss  float64 `json:"stop_loss"`
	Target1   float64 `json:"target1"`
	Target2   float64 `json:"target2"`
	RRRatio   float64 `json:"rr_ratio"`

	HoldDaysMin  int `json:"hold_days_min"`
	HoldDaysBase int `json:"hold_days_base"`
	HoldDaysMax  int `json:"hold_days_max"`

	ClaudeAction    string  `json:"claude_action"`
	ClaudeConf      float64 `json:"claude_confidence"`
	ClaudeRationale string  `json:"claude_rationale"`

	RejectReason string `json:"reject_reason"`

	// Lifecycle columns added in migration 000003
	CandidateStatus string `json:"candidate_status"`
	SetupFamily     string `json:"setup_family"`
	Direction       string `json:"direction"`
	PrevDayVolume   int64  `json:"prev_day_volume"`
}

// UpsertCandidateInput holds the values to upsert.
type UpsertCandidateInput struct {
	TradeDate    time.Time
	Ticker       string
	GateTrend    bool
	GateMomentum bool
	GateVolume   bool
	GateVIX      bool
	GateBTC      bool
	GateRSI      bool
	AllGates     bool

	ClosePrice  float64
	EMA20       float64
	EMA100      float64
	RSI14       float64
	MACDHist    float64
	VolumeRatio float64
	VIXLevel    float64
	BTCROC20    float64

	PatternName    string
	PatternScore   float64
	AntiPatterns   []string
	RejectedByAnti bool

	EntryLow  float64
	EntryHigh float64
	StopLoss  float64
	Target1   float64
	Target2   float64
	RRRatio   float64

	HoldDaysMin  int
	HoldDaysBase int
	HoldDaysMax  int

	RejectReason string

	// Lifecycle columns added in migration 000003
	CandidateStatus string
	SetupFamily     string
	Direction       string
	PrevDayVolume   int64
}

// UpsertCandidate inserts or updates a trade candidate row.
// If a row for (trade_date, ticker) already exists, it is overwritten.
func UpsertCandidate(ctx context.Context, pool *pgxpool.Pool, in UpsertCandidateInput) (string, error) {
	q := `
INSERT INTO trade_candidates (
    trade_date, ticker,
    gate_trend, gate_momentum, gate_volume, gate_vix, gate_btc, gate_rsi, all_gates_passed,
    close_price, ema20, ema100, rsi14, macd_hist, volume_ratio, vix_level, btc_roc20,
    pattern_name, pattern_score, anti_patterns, rejected_by_anti,
    entry_low, entry_high, stop_loss, target1, target2, rr_ratio,
    hold_days_min, hold_days_base, hold_days_max,
    reject_reason, claude_action,
    candidate_status, setup_family, direction, prev_day_volume
) VALUES (
    $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
    $18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,'PENDING',
    $32,$33,$34,$35
)
ON CONFLICT (trade_date, ticker) DO UPDATE SET
    gate_trend=$3, gate_momentum=$4, gate_volume=$5, gate_vix=$6, gate_btc=$7, gate_rsi=$8,
    all_gates_passed=$9,
    close_price=$10, ema20=$11, ema100=$12, rsi14=$13, macd_hist=$14,
    volume_ratio=$15, vix_level=$16, btc_roc20=$17,
    pattern_name=$18, pattern_score=$19, anti_patterns=$20, rejected_by_anti=$21,
    entry_low=$22, entry_high=$23, stop_loss=$24, target1=$25, target2=$26, rr_ratio=$27,
    hold_days_min=$28, hold_days_base=$29, hold_days_max=$30,
    reject_reason=$31,
    candidate_status=CASE WHEN trade_candidates.candidate_status IN ('entry_ready','confirmed')
                          THEN trade_candidates.candidate_status ELSE $32 END,
    setup_family=$33, direction=$34, prev_day_volume=$35
RETURNING id`

	antiPatterns := in.AntiPatterns
	if antiPatterns == nil {
		antiPatterns = []string{}
	}

	var id string
	err := pool.QueryRow(ctx, q,
		in.TradeDate, in.Ticker,
		in.GateTrend, in.GateMomentum, in.GateVolume, in.GateVIX, in.GateBTC, in.GateRSI, in.AllGates,
		in.ClosePrice, in.EMA20, in.EMA100, in.RSI14, in.MACDHist, in.VolumeRatio, in.VIXLevel, in.BTCROC20,
		in.PatternName, in.PatternScore, antiPatterns, in.RejectedByAnti,
		in.EntryLow, in.EntryHigh, in.StopLoss, in.Target1, in.Target2, in.RRRatio,
		in.HoldDaysMin, in.HoldDaysBase, in.HoldDaysMax,
		in.RejectReason,
		in.CandidateStatus, in.SetupFamily, in.Direction, in.PrevDayVolume,
	).Scan(&id)
	return id, err
}

// UpdateCandidateClaudeReview sets the Claude review result on a candidate.
func UpdateCandidateClaudeReview(ctx context.Context, pool *pgxpool.Pool, id, action string, confidence float64, rationale string) error {
	_, err := pool.Exec(ctx,
		`UPDATE trade_candidates SET claude_action=$2, claude_confidence=$3, claude_rationale=$4, claude_reviewed_at=NOW()
         WHERE id=$1`,
		id, action, confidence, rationale,
	)
	return err
}

// GetCandidatesForDate returns all trade_candidates for a given date, ordered by all_gates_passed DESC.
func GetCandidatesForDate(ctx context.Context, pool *pgxpool.Pool, date time.Time) ([]Candidate, error) {
	rows, err := pool.Query(ctx, `
SELECT id, trade_date, ticker, created_at,
       gate_trend, gate_momentum, gate_volume, gate_vix, gate_btc, gate_rsi, all_gates_passed,
       COALESCE(close_price,0), COALESCE(ema20,0), COALESCE(ema100,0), COALESCE(rsi14,0),
       COALESCE(macd_hist,0), COALESCE(volume_ratio,0), COALESCE(vix_level,0), COALESCE(btc_roc20,0),
       COALESCE(pattern_name,''), COALESCE(pattern_score,0), COALESCE(anti_patterns,'{}'), rejected_by_anti,
       COALESCE(entry_low,0), COALESCE(entry_high,0), COALESCE(stop_loss,0),
       COALESCE(target1,0), COALESCE(target2,0), COALESCE(rr_ratio,0),
       COALESCE(hold_days_min,0), COALESCE(hold_days_base,0), COALESCE(hold_days_max,0),
       COALESCE(claude_action,'PENDING'), COALESCE(claude_confidence,0), COALESCE(claude_rationale,''),
       COALESCE(reject_reason,''),
       COALESCE(candidate_status,''), COALESCE(setup_family,''), COALESCE(direction,''),
       COALESCE(prev_day_volume,0)
FROM trade_candidates
WHERE trade_date = $1
ORDER BY all_gates_passed DESC, COALESCE(claude_confidence,0) DESC`,
		date,
	)
	if err != nil {
		return nil, fmt.Errorf("get candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var tradeDate time.Time
		err := rows.Scan(
			&c.ID, &tradeDate, &c.Ticker, &c.CreatedAt,
			&c.GateTrend, &c.GateMomentum, &c.GateVolume, &c.GateVIX, &c.GateBTC, &c.GateRSI, &c.AllGatesPassed,
			&c.ClosePrice, &c.EMA20, &c.EMA100, &c.RSI14,
			&c.MACDHist, &c.VolumeRatio, &c.VIXLevel, &c.BTCROC20,
			&c.PatternName, &c.PatternScore, &c.AntiPatterns, &c.RejectedByAnti,
			&c.EntryLow, &c.EntryHigh, &c.StopLoss,
			&c.Target1, &c.Target2, &c.RRRatio,
			&c.HoldDaysMin, &c.HoldDaysBase, &c.HoldDaysMax,
			&c.ClaudeAction, &c.ClaudeConf, &c.ClaudeRationale,
			&c.RejectReason,
			&c.CandidateStatus, &c.SetupFamily, &c.Direction, &c.PrevDayVolume,
		)
		if err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		c.TradeDate = tradeDate
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCandidateStatus sets candidate_status for a single row.
// Called by the confirmation activity after evaluating each candidate.
func UpdateCandidateStatus(ctx context.Context, pool *pgxpool.Pool, id, status string) error {
	_, err := pool.Exec(ctx,
		`UPDATE trade_candidates SET candidate_status=$2 WHERE id=$1`,
		id, status,
	)
	return err
}

// GetEntryReadyCandidates returns all candidates for date whose candidate_status
// is 'entry_ready'. These are the rows the confirmation activity processes.
func GetEntryReadyCandidates(ctx context.Context, pool *pgxpool.Pool, date time.Time) ([]Candidate, error) {
	rows, err := pool.Query(ctx, `
SELECT id, trade_date, ticker, created_at,
       gate_trend, gate_momentum, gate_volume, gate_vix, gate_btc, gate_rsi, all_gates_passed,
       COALESCE(close_price,0), COALESCE(ema20,0), COALESCE(ema100,0), COALESCE(rsi14,0),
       COALESCE(macd_hist,0), COALESCE(volume_ratio,0), COALESCE(vix_level,0), COALESCE(btc_roc20,0),
       COALESCE(pattern_name,''), COALESCE(pattern_score,0), COALESCE(anti_patterns,'{}'), rejected_by_anti,
       COALESCE(entry_low,0), COALESCE(entry_high,0), COALESCE(stop_loss,0),
       COALESCE(target1,0), COALESCE(target2,0), COALESCE(rr_ratio,0),
       COALESCE(hold_days_min,0), COALESCE(hold_days_base,0), COALESCE(hold_days_max,0),
       COALESCE(claude_action,'PENDING'), COALESCE(claude_confidence,0), COALESCE(claude_rationale,''),
       COALESCE(reject_reason,''),
       COALESCE(candidate_status,''), COALESCE(setup_family,''), COALESCE(direction,''),
       COALESCE(prev_day_volume,0)
FROM trade_candidates
WHERE trade_date=$1 AND candidate_status='entry_ready'
ORDER BY COALESCE(claude_confidence,0) DESC`,
		date,
	)
	if err != nil {
		return nil, fmt.Errorf("get entry-ready candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var tradeDate time.Time
		err := rows.Scan(
			&c.ID, &tradeDate, &c.Ticker, &c.CreatedAt,
			&c.GateTrend, &c.GateMomentum, &c.GateVolume, &c.GateVIX, &c.GateBTC, &c.GateRSI, &c.AllGatesPassed,
			&c.ClosePrice, &c.EMA20, &c.EMA100, &c.RSI14,
			&c.MACDHist, &c.VolumeRatio, &c.VIXLevel, &c.BTCROC20,
			&c.PatternName, &c.PatternScore, &c.AntiPatterns, &c.RejectedByAnti,
			&c.EntryLow, &c.EntryHigh, &c.StopLoss,
			&c.Target1, &c.Target2, &c.RRRatio,
			&c.HoldDaysMin, &c.HoldDaysBase, &c.HoldDaysMax,
			&c.ClaudeAction, &c.ClaudeConf, &c.ClaudeRationale,
			&c.RejectReason,
			&c.CandidateStatus, &c.SetupFamily, &c.Direction, &c.PrevDayVolume,
		)
		if err != nil {
			return nil, fmt.Errorf("scan entry-ready candidate: %w", err)
		}
		c.TradeDate = tradeDate
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetExhaustionReversalStructuralCandidates returns structural_candidate rows for
// the given date whose setup_family is 'bearish_exhaustion_reversal'.
// These are loaded by the opening-confirmation activity for the intraday rejection
// check: if rejection is confirmed, they are promoted to entry_ready in the DB
// and added to the Claude confirmation payload.
func GetExhaustionReversalStructuralCandidates(ctx context.Context, pool *pgxpool.Pool, date time.Time) ([]Candidate, error) {
	rows, err := pool.Query(ctx, `
SELECT id, trade_date, ticker, created_at,
       gate_trend, gate_momentum, gate_volume, gate_vix, gate_btc, gate_rsi, all_gates_passed,
       COALESCE(close_price,0), COALESCE(ema20,0), COALESCE(ema100,0), COALESCE(rsi14,0),
       COALESCE(macd_hist,0), COALESCE(volume_ratio,0), COALESCE(vix_level,0), COALESCE(btc_roc20,0),
       COALESCE(pattern_name,''), COALESCE(pattern_score,0), COALESCE(anti_patterns,'{}'), rejected_by_anti,
       COALESCE(entry_low,0), COALESCE(entry_high,0), COALESCE(stop_loss,0),
       COALESCE(target1,0), COALESCE(target2,0), COALESCE(rr_ratio,0),
       COALESCE(hold_days_min,0), COALESCE(hold_days_base,0), COALESCE(hold_days_max,0),
       COALESCE(claude_action,'PENDING'), COALESCE(claude_confidence,0), COALESCE(claude_rationale,''),
       COALESCE(reject_reason,''),
       COALESCE(candidate_status,''), COALESCE(setup_family,''), COALESCE(direction,''),
       COALESCE(prev_day_volume,0)
FROM trade_candidates
WHERE trade_date=$1 AND candidate_status='structural_candidate' AND setup_family='bearish_exhaustion_reversal'
ORDER BY COALESCE(claude_confidence,0) DESC`,
		date,
	)
	if err != nil {
		return nil, fmt.Errorf("get exhaustion reversal structural candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var tradeDate time.Time
		err := rows.Scan(
			&c.ID, &tradeDate, &c.Ticker, &c.CreatedAt,
			&c.GateTrend, &c.GateMomentum, &c.GateVolume, &c.GateVIX, &c.GateBTC, &c.GateRSI, &c.AllGatesPassed,
			&c.ClosePrice, &c.EMA20, &c.EMA100, &c.RSI14,
			&c.MACDHist, &c.VolumeRatio, &c.VIXLevel, &c.BTCROC20,
			&c.PatternName, &c.PatternScore, &c.AntiPatterns, &c.RejectedByAnti,
			&c.EntryLow, &c.EntryHigh, &c.StopLoss,
			&c.Target1, &c.Target2, &c.RRRatio,
			&c.HoldDaysMin, &c.HoldDaysBase, &c.HoldDaysMax,
			&c.ClaudeAction, &c.ClaudeConf, &c.ClaudeRationale,
			&c.RejectReason,
			&c.CandidateStatus, &c.SetupFamily, &c.Direction, &c.PrevDayVolume,
		)
		if err != nil {
			return nil, fmt.Errorf("scan exhaustion reversal candidate: %w", err)
		}
		c.TradeDate = tradeDate
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConfirmationStoreInput carries all fields needed for a trade_confirmations row.
type ConfirmationStoreInput struct {
	CandidateID       string
	Ticker            string
	TradeDate         time.Time
	Status            string // "confirmed" or "watch_only"
	SignalLevelHolds  bool
	SignalOpenRange   bool
	SignalNoRejection bool
	SignalVolumeOK    bool
	SignalMarketOK    bool
	SignalsPassed     int
	AutoRejected      bool
	AutoRejectReason  string
	OpenPrice         float64
	First10High       float64
	First10Low        float64
	First10Close      float64
	First10Volume     int64
}

// UpsertTradeConfirmation writes a confirmation result row.
// ON CONFLICT (candidate_id) DO UPDATE keeps the row idempotent if the
// confirmation activity is retried by Temporal.
func UpsertTradeConfirmation(ctx context.Context, pool *pgxpool.Pool, in ConfirmationStoreInput) error {
	_, err := pool.Exec(ctx, `
INSERT INTO trade_confirmations (
    candidate_id, trade_date, ticker,
    signal_level_holds, signal_open_range, signal_no_rejection,
    signal_volume_ok, signal_market_ok, signals_passed,
    auto_rejected, auto_reject_reason,
    status,
    open_price, first10_high, first10_low, first10_close, first10_volume,
    confirmed_at
) VALUES (
    $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,NOW()
)
ON CONFLICT (candidate_id) DO UPDATE SET
    signal_level_holds=$4, signal_open_range=$5, signal_no_rejection=$6,
    signal_volume_ok=$7, signal_market_ok=$8, signals_passed=$9,
    auto_rejected=$10, auto_reject_reason=$11,
    status=$12,
    open_price=$13, first10_high=$14, first10_low=$15, first10_close=$16, first10_volume=$17,
    confirmed_at=NOW()`,
		in.CandidateID, in.TradeDate, in.Ticker,
		in.SignalLevelHolds, in.SignalOpenRange, in.SignalNoRejection,
		in.SignalVolumeOK, in.SignalMarketOK, in.SignalsPassed,
		in.AutoRejected, in.AutoRejectReason,
		in.Status,
		in.OpenPrice, in.First10High, in.First10Low, in.First10Close, in.First10Volume,
	)
	return err
}

// ─── daily_summaries ─────────────────────────────────────────────────────────

// DailySummary mirrors the daily_summaries table.
type DailySummary struct {
	TradeDate           time.Time `json:"trade_date"`
	VIXLevel            float64   `json:"vix_level"`
	BTCROC20            float64   `json:"btc_roc20"`
	RegimeLabel         string    `json:"regime_label"`
	SymbolsScanned      int       `json:"symbols_scanned"`
	CandidatesFound     int       `json:"candidates_found"`
	CandidatesConfirmed int       `json:"candidates_confirmed"`
	NoTradeToday        bool      `json:"no_trade_today"`
	NoTradeReason       string    `json:"no_trade_reason"`
	RegimeSummary       string    `json:"regime_summary"`
	WatchTickers        []string  `json:"watch_tickers"`
	OpenPositions       int       `json:"open_positions"`
	AnalysisCompleted   bool      `json:"analysis_completed"`
}

// UpsertDailySummary inserts or updates a daily summary row.
func UpsertDailySummary(ctx context.Context, pool *pgxpool.Pool, s DailySummary) error {
	watchTickers := s.WatchTickers
	if watchTickers == nil {
		watchTickers = []string{}
	}
	_, err := pool.Exec(ctx, `
INSERT INTO daily_summaries (
    trade_date, vix_level, btc_roc20, regime_label,
    symbols_scanned, candidates_found, candidates_confirmed,
    no_trade_today, no_trade_reason, regime_summary, watch_tickers,
    open_positions, analysis_completed, analysis_completed_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NOW(),NOW())
ON CONFLICT (trade_date) DO UPDATE SET
    vix_level=$2, btc_roc20=$3, regime_label=$4,
    symbols_scanned=$5, candidates_found=$6, candidates_confirmed=$7,
    no_trade_today=$8, no_trade_reason=$9, regime_summary=$10, watch_tickers=$11,
    open_positions=$12, analysis_completed=$13, analysis_completed_at=NOW(), updated_at=NOW()`,
		s.TradeDate, s.VIXLevel, s.BTCROC20, s.RegimeLabel,
		s.SymbolsScanned, s.CandidatesFound, s.CandidatesConfirmed,
		s.NoTradeToday, s.NoTradeReason, s.RegimeSummary, watchTickers,
		s.OpenPositions, s.AnalysisCompleted,
	)
	return err
}

// GetDailySummary retrieves the summary for a specific date.
func GetDailySummary(ctx context.Context, pool *pgxpool.Pool, date time.Time) (*DailySummary, error) {
	var s DailySummary
	var tradeDate time.Time
	err := pool.QueryRow(ctx, `
SELECT trade_date, COALESCE(vix_level,0), COALESCE(btc_roc20,0), COALESCE(regime_label,''),
       symbols_scanned, candidates_found, candidates_confirmed,
       no_trade_today, COALESCE(no_trade_reason,''), COALESCE(regime_summary,''),
       COALESCE(watch_tickers,'{}'), open_positions, analysis_completed
FROM daily_summaries WHERE trade_date=$1`, date).Scan(
		&tradeDate, &s.VIXLevel, &s.BTCROC20, &s.RegimeLabel,
		&s.SymbolsScanned, &s.CandidatesFound, &s.CandidatesConfirmed,
		&s.NoTradeToday, &s.NoTradeReason, &s.RegimeSummary,
		&s.WatchTickers, &s.OpenPositions, &s.AnalysisCompleted,
	)
	if err != nil {
		return nil, err
	}
	s.TradeDate = tradeDate
	return &s, nil
}

// ─── paper_positions ─────────────────────────────────────────────────────────

// PaperPosition mirrors the paper_positions table (migrations 000001–000007).
// Risk-state fields (migration 000007) are included so the UI and API can
// display full position state without a separate query.
type PaperPosition struct {
	ID             string    `json:"id"`
	Ticker         string    `json:"ticker"`
	Status         string    `json:"status"`
	EntryPrice     float64   `json:"entry_price"`
	EntryDate      time.Time `json:"entry_date"`
	Shares         float64   `json:"shares"`
	StopLoss       float64   `json:"stop_loss"`
	Target1        float64   `json:"target1"`
	Target2        float64   `json:"target2"`
	ExitPrice      float64   `json:"exit_price"`
	RealizedPnLPct float64   `json:"realized_pnl_pct"`
	OpenedAt       time.Time `json:"opened_at"`
	Notes          string    `json:"notes"`

	// Option tracking (migration 000005)
	OptionSymbol  string  `json:"option_symbol"`
	OptionPremium float64 `json:"option_premium"` // premium paid at entry

	// Risk state (migration 000007)
	PeakOptionPrice       float64 `json:"peak_option_price"`       // highest mid-price seen
	TrailingActive        bool    `json:"trailing_active"`         // true once +35% hit
	LastOptionPrice       float64 `json:"last_option_price"`       // last fetched mid
	HoldOvernightApproved bool    `json:"hold_overnight_approved"` // Claude approved hold
}

// GetOpenPaperPositions returns all open paper positions including risk state.
func GetOpenPaperPositions(ctx context.Context, pool *pgxpool.Pool) ([]PaperPosition, error) {
	rows, err := pool.Query(ctx, `
SELECT id, ticker, status,
       entry_price, entry_date, shares,
       stop_loss, COALESCE(target1,0), COALESCE(target2,0),
       COALESCE(exit_price,0), COALESCE(realized_pnl_pct,0),
       opened_at, COALESCE(notes,''),
       COALESCE(option_symbol,''), COALESCE(option_premium,0),
       COALESCE(peak_option_price,0), COALESCE(trailing_active,false),
       COALESCE(last_option_price,0), COALESCE(hold_overnight_approved,false)
FROM paper_positions WHERE status='open'
ORDER BY opened_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PaperPosition
	for rows.Next() {
		var p PaperPosition
		if err := rows.Scan(
			&p.ID, &p.Ticker, &p.Status,
			&p.EntryPrice, &p.EntryDate, &p.Shares,
			&p.StopLoss, &p.Target1, &p.Target2,
			&p.ExitPrice, &p.RealizedPnLPct,
			&p.OpenedAt, &p.Notes,
			&p.OptionSymbol, &p.OptionPremium,
			&p.PeakOptionPrice, &p.TrailingActive,
			&p.LastOptionPrice, &p.HoldOvernightApproved,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RiskablePosition extends ReviewablePosition with the risk-state columns
// added by migration 000007. Used by RunMechanicalRiskCheckActivity.
type RiskablePosition struct {
	ReviewablePosition
	PeakOptionPrice       float64
	TrailingActive        bool
	LastOptionPrice       float64
	HoldOvernightApproved bool
}

// GetOpenPositionsForRiskCheck returns open positions with all columns needed
// by the mechanical risk check (option_symbol, option_premium + risk state).
func GetOpenPositionsForRiskCheck(ctx context.Context, pool *pgxpool.Pool) ([]RiskablePosition, error) {
	rows, err := pool.Query(ctx, `
SELECT id, ticker, status,
       entry_price, entry_date, shares,
       stop_loss, COALESCE(target1,0), COALESCE(target2,0),
       COALESCE(exit_price,0), COALESCE(realized_pnl_pct,0),
       opened_at, COALESCE(notes,''),
       COALESCE(option_type,'call'), COALESCE(setup_family,''),
       COALESCE(option_symbol,''), COALESCE(option_premium,0),
       COALESCE(peak_option_price,0), COALESCE(trailing_active,false),
       COALESCE(last_option_price,0), COALESCE(hold_overnight_approved,false)
FROM paper_positions WHERE status='open'
ORDER BY opened_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RiskablePosition
	for rows.Next() {
		var p RiskablePosition
		if err := rows.Scan(
			&p.ID, &p.Ticker, &p.Status,
			&p.EntryPrice, &p.EntryDate, &p.Shares,
			&p.StopLoss, &p.Target1, &p.Target2,
			&p.ExitPrice, &p.RealizedPnLPct,
			&p.OpenedAt, &p.Notes,
			&p.OptionType, &p.SetupFamily,
			&p.OptionSymbol, &p.OptionPremium,
			&p.PeakOptionPrice, &p.TrailingActive,
			&p.LastOptionPrice, &p.HoldOvernightApproved,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePositionRiskState persists the result of a mechanical risk check:
// the latest mid-price, the updated high-water mark, and trailing state.
// last_risk_check_at is always set to NOW().
func UpdatePositionRiskState(ctx context.Context, pool *pgxpool.Pool, positionID string, lastPrice, peakPrice float64, trailingActive bool) error {
	_, err := pool.Exec(ctx,
		`UPDATE paper_positions
		 SET last_option_price=$2,
		     peak_option_price=$3,
		     trailing_active=$4,
		     last_risk_check_at=NOW()
		 WHERE id=$1`,
		positionID, lastPrice, peakPrice, trailingActive,
	)
	return err
}

// SetHoldOvernightApproved marks a position as approved (or revoked) for
// overnight hold. Claude calls this via the EOD review activity.
func SetHoldOvernightApproved(ctx context.Context, pool *pgxpool.Pool, positionID string, approved bool) error {
	_, err := pool.Exec(ctx,
		`UPDATE paper_positions
		 SET hold_overnight_approved=$2,
		     hold_overnight_approved_at=CASE WHEN $2 THEN NOW() ELSE NULL END
		 WHERE id=$1`,
		positionID, approved,
	)
	return err
}

// ─── auto paper entry ────────────────────────────────────────────────────────

// PaperPositionInput holds the values needed to open a new paper position.
type PaperPositionInput struct {
	CandidateID string
	Ticker      string
	EntryPrice  float64
	EntryDate   time.Time
	Shares      float64
	StopLoss    float64
	Target1     float64
	Target2     float64
	OptionType  string // "call" | "put"
	SetupFamily string
}

// CreatePaperPosition inserts a new open paper position and returns its ID.
// ON CONFLICT (candidate_id) DO UPDATE ensures idempotency for Temporal retries:
// if the position already exists the existing row is returned unchanged.
func CreatePaperPosition(ctx context.Context, pool *pgxpool.Pool, in PaperPositionInput) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
INSERT INTO paper_positions (
    candidate_id, ticker,
    entry_price, entry_date, shares,
    stop_loss, target1, target2,
    option_type, setup_family,
    status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'open')
ON CONFLICT (candidate_id) DO UPDATE SET ticker=EXCLUDED.ticker
RETURNING id`,
		in.CandidateID, in.Ticker,
		in.EntryPrice, in.EntryDate, in.Shares,
		in.StopLoss, in.Target1, in.Target2,
		in.OptionType, in.SetupFamily,
	).Scan(&id)
	return id, err
}

// HasOpenPositionForTicker returns true when the ticker already has an open
// paper position. Used by runPipeline to suppress duplicate entries.
func HasOpenPositionForTicker(ctx context.Context, pool *pgxpool.Pool, ticker string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM paper_positions WHERE ticker=$1 AND status='open')`,
		ticker,
	).Scan(&exists)
	return exists, err
}

// GetOpenPositionCount returns the total number of currently open paper positions.
func GetOpenPositionCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM paper_positions WHERE status='open'`,
	).Scan(&n)
	return n, err
}

// GetOpenPositionCountByDirection returns the number of open paper positions
// for the given option_type ("call" = bullish, "put" = bearish).
func GetOpenPositionCountByDirection(ctx context.Context, pool *pgxpool.Pool, optionType string) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM paper_positions WHERE status='open' AND option_type=$1`,
		optionType,
	).Scan(&n)
	return n, err
}

// SaveIVSnapshot upserts one daily proxy-IV snapshot for the given ticker.
// proxy_iv = atm_call_ask / (underlying_price * sqrt(dte/252)).
func SaveIVSnapshot(ctx context.Context, pool *pgxpool.Pool,
	ticker, snapshotDate, atmSymbol string,
	atmStrike, underlyingPrice, atmCallAsk float64,
	dte int, proxyIV float64,
) error {
	_, err := pool.Exec(ctx, `
INSERT INTO iv_snapshots
    (ticker, snapshot_date, atm_symbol, atm_strike, underlying_price, atm_call_ask, dte, proxy_iv)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (ticker, snapshot_date) DO UPDATE
    SET atm_symbol       = EXCLUDED.atm_symbol,
        atm_strike       = EXCLUDED.atm_strike,
        underlying_price = EXCLUDED.underlying_price,
        atm_call_ask     = EXCLUDED.atm_call_ask,
        dte              = EXCLUDED.dte,
        proxy_iv         = EXCLUDED.proxy_iv`,
		ticker, snapshotDate, atmSymbol, atmStrike, underlyingPrice, atmCallAsk, dte, proxyIV,
	)
	return err
}

// GetIVRank returns the rolling percentile rank (0–100) of currentProxyIV
// against the last lookbackDays snapshots for this ticker.
// Also returns the number of snapshots found (caller can skip the gate if too few).
// Rank 0 = cheapest volatility seen; 100 = most expensive.
func GetIVRank(ctx context.Context, pool *pgxpool.Pool,
	ticker string, currentProxyIV float64, lookbackDays int,
) (rank float64, snapshots int, err error) {
	row := pool.QueryRow(ctx, `
SELECT
    COUNT(*)                                                             AS total,
    COUNT(*) FILTER (WHERE proxy_iv <= $3)                              AS at_or_below
FROM iv_snapshots
WHERE ticker = $1
  AND snapshot_date >= CURRENT_DATE - ($2 || ' days')::INTERVAL
  AND proxy_iv > 0`,
		ticker, lookbackDays, currentProxyIV,
	)
	var total, atOrBelow int
	if err = row.Scan(&total, &atOrBelow); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, nil
	}
	rank = float64(atOrBelow) / float64(total) * 100.0
	return rank, total, nil
}

// UpdatePositionAlpacaOrderID stores the Alpaca order ID for a paper position.
// Called after PlaceOptionOrder succeeds so the position row links to the live order.
func UpdatePositionAlpacaOrderID(ctx context.Context, pool *pgxpool.Pool, positionID, orderID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE paper_positions SET alpaca_order_id=$2 WHERE id=$1`,
		positionID, orderID,
	)
	return err
}

// UpdatePositionOptionDetails stores the OCC option symbol and premium paid.
// Called alongside UpdatePositionAlpacaOrderID so the daily review can compute
// correct option-level P&L instead of using the underlying stock price.
func UpdatePositionOptionDetails(ctx context.Context, pool *pgxpool.Pool, positionID, optionSymbol string, premium float64) error {
	_, err := pool.Exec(ctx,
		`UPDATE paper_positions SET option_symbol=$2, option_premium=$3 WHERE id=$1`,
		positionID, optionSymbol, premium,
	)
	return err
}

// InsertPositionEvent appends one lifecycle event to paper_position_events.
// payload is serialised to JSONB; pass nil for an empty payload.
func InsertPositionEvent(ctx context.Context, pool *pgxpool.Pool, positionID, ticker, eventType string, priceAtEvent float64, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO paper_position_events (position_id, ticker, event_type, price_at_event, payload)
		 VALUES ($1,$2,$3,$4,$5)`,
		positionID, ticker, eventType, priceAtEvent, raw,
	)
	return err
}

// ClosePosition marks a paper position closed with final exit data.
func ClosePosition(ctx context.Context, pool *pgxpool.Pool, positionID string, exitPrice, pnlPct float64, exitReason string) error {
	_, err := pool.Exec(ctx,
		`UPDATE paper_positions SET
		    status='closed', exit_price=$2, exit_date=CURRENT_DATE,
		    exit_reason=$3, realized_pnl_pct=$4, closed_at=NOW()
		 WHERE id=$1`,
		positionID, exitPrice, exitReason, pnlPct,
	)
	return err
}

// ─── position reviews ─────────────────────────────────────────────────────────

// PositionReviewInput holds the data for one daily position review row.
type PositionReviewInput struct {
	PositionID      string
	Ticker          string
	ReviewDate      time.Time
	CurrentPrice    float64
	PnLPctToday     float64
	DaysHeld        int
	SuggestedAction string // HOLD | HOLD_TIGHTEN_STOP | PARTIAL_TAKE_PROFIT | EXIT | WATCH_CLOSELY
	ActionRationale string
	NewStop         float64 // 0 means not tightening
	ActionExecuted  bool
}

// UpsertPositionReview writes a daily review result.
// UNIQUE (position_id, review_date) makes it idempotent.
func UpsertPositionReview(ctx context.Context, pool *pgxpool.Pool, in PositionReviewInput) error {
	var newStop interface{}
	if in.NewStop > 0 {
		newStop = in.NewStop
	}
	_, err := pool.Exec(ctx, `
INSERT INTO position_reviews (
    position_id, ticker, review_date,
    current_price, pnl_pct_today, days_held,
    suggested_action, action_rationale, new_stop, action_executed
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (position_id, review_date) DO UPDATE SET
    current_price=$4, pnl_pct_today=$5, days_held=$6,
    suggested_action=$7, action_rationale=$8, new_stop=$9, action_executed=$10`,
		in.PositionID, in.Ticker, in.ReviewDate,
		in.CurrentPrice, in.PnLPctToday, in.DaysHeld,
		in.SuggestedAction, in.ActionRationale, newStop, in.ActionExecuted,
	)
	return err
}

// ReviewablePosition is a PaperPosition augmented with review-context fields.
// option_symbol and option_premium (migration 000005) enable correct option P&L.
type ReviewablePosition struct {
	PaperPosition
	OptionType    string
	SetupFamily   string
	OptionSymbol  string  // OCC contract symbol, e.g. "RTX260508P00190000"; "" if not set
	OptionPremium float64 // premium paid per contract in $; 0 if not set (old positions)
}

// GetOpenPositionsForReview returns open positions with option_type, setup_family,
// option_symbol, and option_premium populated (migrations 000004 + 000005).
func GetOpenPositionsForReview(ctx context.Context, pool *pgxpool.Pool) ([]ReviewablePosition, error) {
	rows, err := pool.Query(ctx, `
SELECT id, ticker, status,
       entry_price, entry_date, shares,
       stop_loss, COALESCE(target1,0), COALESCE(target2,0),
       COALESCE(exit_price,0), COALESCE(realized_pnl_pct,0),
       opened_at, COALESCE(notes,''),
       COALESCE(option_type,'call'), COALESCE(setup_family,''),
       COALESCE(option_symbol,''), COALESCE(option_premium,0)
FROM paper_positions WHERE status='open'
ORDER BY opened_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewablePosition
	for rows.Next() {
		var p ReviewablePosition
		if err := rows.Scan(
			&p.ID, &p.Ticker, &p.Status,
			&p.EntryPrice, &p.EntryDate, &p.Shares,
			&p.StopLoss, &p.Target1, &p.Target2,
			&p.ExitPrice, &p.RealizedPnLPct,
			&p.OpenedAt, &p.Notes,
			&p.OptionType, &p.SetupFamily,
			&p.OptionSymbol, &p.OptionPremium,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─── weekly reviews ───────────────────────────────────────────────────────────

// WeeklyReviewInput carries the data for one weekly review row.
type WeeklyReviewInput struct {
	WeekStart time.Time
	WeekEnd   time.Time
	Summary   string
}

// InsertWeeklyReview persists a weekly review summary.
// ON CONFLICT (week_start) replaces the existing row if re-run.
func InsertWeeklyReview(ctx context.Context, pool *pgxpool.Pool, in WeeklyReviewInput) error {
	_, err := pool.Exec(ctx, `
INSERT INTO weekly_reviews (week_start, week_end, summary)
VALUES ($1,$2,$3)
ON CONFLICT (week_start) DO UPDATE SET week_end=$2, summary=$3, created_at=NOW()`,
		in.WeekStart, in.WeekEnd, in.Summary,
	)
	return err
}

// GetClosedPositionsInRange returns positions whose exit_date falls within
// [from, to]. Used by the weekly review activity.
func GetClosedPositionsInRange(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) ([]PaperPosition, error) {
	rows, err := pool.Query(ctx, `
SELECT id, ticker, status,
       entry_price, entry_date, shares,
       stop_loss, COALESCE(target1,0), COALESCE(target2,0),
       COALESCE(exit_price,0), COALESCE(realized_pnl_pct,0),
       opened_at, COALESCE(notes,'')
FROM paper_positions
WHERE status='closed' AND exit_date BETWEEN $1 AND $2
ORDER BY exit_date DESC`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaperPosition
	for rows.Next() {
		var p PaperPosition
		if err := rows.Scan(
			&p.ID, &p.Ticker, &p.Status,
			&p.EntryPrice, &p.EntryDate, &p.Shares,
			&p.StopLoss, &p.Target1, &p.Target2,
			&p.ExitPrice, &p.RealizedPnLPct,
			&p.OpenedAt, &p.Notes,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─── Active strategy prompt ───────────────────────────────────────────────────

// GetActiveStrategyPrompt returns the prompt_text from the active strategy version.
func GetActiveStrategyPrompt(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var prompt string
	err := pool.QueryRow(ctx,
		`SELECT prompt_text FROM strategy_versions WHERE is_active=TRUE LIMIT 1`,
	).Scan(&prompt)
	return prompt, err
}
