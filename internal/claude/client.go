// internal/claude/client.go
//
// WHAT: Claude API client for the options paper-trade decision engine.
//       Sends a RuntimePayload (matching examples/runtime_payload.json) and
//       returns an OptionsDecision (matching decision_schema.json).
//
// WHY:  Claude is a bounded reviewer — it applies rules from strategy_rules.yaml
//       to app-computed evidence and returns a deterministic JSON output.
//       It never sees raw bars, never decides strategy rules, and never
//       overrides chain quality filtering done on the app side.
//
// HOW:  System prompt = prompts/decision_prompt.md + strategy_rules.yaml (embedded).
//       User message = RuntimePayload JSON (built from prescreened candidates).
//       Response must validate against decision_schema.json.
//
//       We load prompts/decision_prompt.md and strategy_rules.yaml from disk
//       at runtime (project root). If files are missing we log a warning and
//       fall back to a minimal system prompt so the app keeps running.
//
// WHAT BREAKS: If the API key is invalid, all calls return 401.
//              If Claude returns malformed JSON (rare), we log raw response
//              and mark the scan as partially failed.
//
// VERIFY: After /api/run-analysis, claude_rationale in trade_candidates should
//         contain valid JSON matching decision_schema.json.

package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yourname/makemytrade/internal/market"
)

// Client wraps the Anthropic Messages API.
type Client struct {
	apiKey          string
	model           string
	maxOutputTokens int
	systemPrompt    string
	http            *http.Client
}

// NewClient creates a Claude API client.
// systemPrompt is built from prompts/decision_prompt.md + strategy_rules.yaml.
func NewClient(apiKey, model string, maxOutputTokens int, systemPrompt string) *Client {
	return &Client{
		apiKey:          apiKey,
		model:           model,
		maxOutputTokens: maxOutputTokens,
		systemPrompt:    systemPrompt,
		http:            &http.Client{Timeout: 5 * time.Minute},
	}
}

// BuildSystemPrompt loads prompts/decision_prompt.md and strategy_rules.yaml
// from the filesystem and concatenates them into a single system prompt.
// Falls back to a minimal prompt if files are not found.
func BuildSystemPrompt() string {
	decisionPrompt := loadFile("prompts/decision_prompt.md",
		"You are an options paper trade decision engine. Return valid JSON only.")
	strategyRules := loadFile("strategy_rules.yaml", "")
	schemaJSON := loadFile("decision_schema.json", "")

	var sb strings.Builder
	sb.WriteString(decisionPrompt)
	if strategyRules != "" {
		sb.WriteString("\n\n---\n# strategy_rules.yaml\n\n```yaml\n")
		sb.WriteString(strategyRules)
		sb.WriteString("\n```\n")
	}
	if schemaJSON != "" {
		sb.WriteString("\n\n---\n# decision_schema.json (your output must validate against this)\n\n```json\n")
		sb.WriteString(schemaJSON)
		sb.WriteString("\n```\n")
	}
	return sb.String()
}

func loadFile(path, fallback string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		if fallback != "" {
			fmt.Printf("claude: warning: could not load %s: %v — using fallback\n", path, err)
		}
		return fallback
	}
	return string(b)
}

// ── RuntimePayload — sent to Claude as the user message ──────────────────────

// RuntimePayload is the complete input payload for Claude's decision.
// It matches the structure of examples/runtime_payload.json.
type RuntimePayload struct {
	ScanTimePT    string           `json:"scan_time_pt"`
	MarketContext MarketContext    `json:"market_context"`
	OpenPositions []PositionInput  `json:"open_positions"`
	Candidates    []CandidateInput `json:"candidates"`
}

// MarketContext holds broad market state.
type MarketContext struct {
	SPXTrend      string  `json:"spx_trend"` // "bullish" | "bearish" | "mixed"
	QQQTrend      string  `json:"qqq_trend"`
	MacroNewsBias string  `json:"macro_news_bias"` // "positive" | "negative" | "neutral"
	BreadthTone   string  `json:"breadth_tone"`
	VIX           float64 `json:"vix"`
	BTCRoc20      float64 `json:"btc_roc20"`

	// Market-wide options sentiment (CBOE equity put/call ratio)
	EquityPCRatio float64 `json:"equity_pc_ratio,omitempty"` // puts/calls; >1.0 = fear
	PCRatioBias   string  `json:"pc_ratio_bias,omitempty"`   // "fear" | "complacency" | "neutral"
}

// CandidateInput is one prescreened ticker in the runtime payload.
// Fields added in v2: SetupFamily, PatternScore, ReasonCodes, HoldWindowBase,
// BaseTarget, StretchTarget — all derived from strategy_rules.yaml by the engine.
type CandidateInput struct {
	Ticker         string                  `json:"ticker"`
	Price          float64                 `json:"price"`
	Daily20EMA     float64                 `json:"daily_20_ema"`
	Daily50EMA     float64                 `json:"daily_50_ema"`
	RSI14          float64                 `json:"rsi14"`
	MACDHist       float64                 `json:"macd_hist"`
	RelativeVolume float64                 `json:"relative_volume"`
	VWAP           float64                 `json:"vwap"`
	PremktHigh     float64                 `json:"premarket_high"`
	PremktLow      float64                 `json:"premarket_low"`
	PriorDayHigh   float64                 `json:"prior_day_high"`
	PriorDayLow    float64                 `json:"prior_day_low"`
	TrendBias      string                  `json:"trend_bias"` // "bullish" | "bearish" | "mixed"
	Sentiment      string                  `json:"sentiment"`  // "positive" | "negative" | "neutral"
	EarningsRisk   bool                    `json:"earnings_risk"`
	AntiPatterns   []string                `json:"anti_patterns,omitempty"`
	Options        []market.OptionContract `json:"options"`

	// v2 fields — derived from strategy_rules.yaml by the strategy engine
	SetupFamily    string   `json:"setup_family"`           // e.g. "bullish_continuation"
	PatternScore   int      `json:"pattern_score"`          // integer from YAML pattern_scores
	ReasonCodes    []string `json:"reason_codes,omitempty"` // from YAML reason_codes enum
	HoldWindowBase int      `json:"hold_window_base_days"`  // from YAML target_model
	BaseTarget     float64  `json:"base_target"`            // structure-based (ATR/swing)
	StretchTarget  float64  `json:"stretch_target"`         // structure-based extended target
	OptionsStatus  string   `json:"options_status"`         // options_not_allowed | options_ready

	// v3 fields — external market signals fetched at analysis time
	PremarketGapPct   float64  `json:"premarket_gap_pct,omitempty"`   // % gap vs prior close; + = gap up
	PremarketGapDir   string   `json:"premarket_gap_dir,omitempty"`   // "up" | "down" | "flat"
	PremarketVol      int64    `json:"premarket_volume,omitempty"`    // pre-market share volume
	ShortFloatPct     float64  `json:"short_float_pct,omitempty"`     // % of float sold short (Finviz)
	ShortRatioDays    float64  `json:"short_ratio_days,omitempty"`    // days to cover (Finviz)
	ShortTrend        string   `json:"short_trend,omitempty"`         // "rising" | "falling" | "flat" (FINRA)
	TickerPCRatio     float64  `json:"ticker_pc_ratio,omitempty"`     // per-ticker put/call OI ratio (Yahoo)
	TickerPCBias      string   `json:"ticker_pc_bias,omitempty"`      // "put_heavy" | "call_heavy" | "balanced"
	NewsHeadlines     []string `json:"news_headlines,omitempty"`      // recent headlines (Finviz + Finnhub)

	// v4 fields — opening session data (only populated when analysis runs after market open)
	Opening5MinBars     []market.Opening5MinBar `json:"opening_5min_bars,omitempty"` // first 3 candles of regular session
	RelativeStrength20d float64                 `json:"relative_strength_20d,omitempty"` // ticker 20d return minus SPY 20d return; positive = outperforming
}

// PositionInput describes an open paper position for daily review.
type PositionInput struct {
	Ticker     string  `json:"ticker"`
	OptionType string  `json:"option_type"` // "call" or "put"
	EntryPrice float64 `json:"entry_price"`
	CurrentPnL float64 `json:"current_pnl_pct"`
	DTE        int     `json:"dte_remaining"`
	Status     string  `json:"status"` // "open"
}

// ── OptionsDecision — returned by Claude (matches decision_schema.json) ───────

// OptionsDecision is the full schema-validated response from Claude.
type OptionsDecision struct {
	ScanTimePT         string               `json:"scan_time_pt"`
	MarketRegime       RegimeDecision       `json:"market_regime"`
	DailySummary       DailySummaryDecision `json:"daily_summary"`
	Candidates         []CandidateDecision  `json:"candidates"`
	OpenPositionReview []PositionReview     `json:"open_positions_review"`
}

type RegimeDecision struct {
	Label      string `json:"label"`      // "bullish" | "bearish" | "mixed" | "choppy_high_risk"
	Confidence int    `json:"confidence"` // 0–100
	Reason     string `json:"reason"`
}

type DailySummaryDecision struct {
	ActionBias string `json:"action_bias"` // "long_calls" | "long_puts" | "selective_both" | "no_trade_bias"
	Notes      string `json:"notes"`
}

type CandidateDecision struct {
	Ticker        string            `json:"ticker"`
	Direction     string            `json:"direction"`  // "call" | "put" | "none"
	Status        string            `json:"status"`     // "rejected" | "structural_candidate" | "entry_ready" | "confirmed" | "watch_only"
	Score         int               `json:"score"`      // 0–100
	RegimeFit     int               `json:"regime_fit"` // 0–100
	Thesis        ThesisDecision    `json:"thesis"`
	Levels        LevelsDecision    `json:"levels"`
	Targets       TargetsDecision   `json:"targets"`
	Contract      ContractDecision  `json:"contract_selection"`
	EntryTrigger  EntryTrigger      `json:"entry_trigger"`
	RiskPlan      RiskPlan          `json:"risk_plan"`
	HoldOvernight HoldOvernightRule `json:"hold_overnight_rule"`
	DailyReview   DailyReviewRule   `json:"daily_review_rule"`
	FinalDecision string            `json:"final_decision"` // "paper_trade_now" | "place_trigger_only" | "watchlist_only" | "reject"

	// v2 fields
	SetupFamily   string   `json:"setup_family"`           // family name from strategy_rules.yaml
	OptionsStatus string   `json:"options_status"`         // options_not_allowed | options_ready | options_confirmed
	ReasonCodes   []string `json:"reason_codes,omitempty"` // from YAML reason_codes enum
}

type ThesisDecision struct {
	TrendStructure        string `json:"trend_structure"`
	CatalystSentiment     string `json:"catalyst_sentiment"`
	VolumeParticipation   string `json:"volume_participation"`
	TechnicalConfirmation string `json:"technical_confirmation"`
	FundamentalContext    string `json:"fundamental_context"`
}

type LevelsDecision struct {
	PremktHigh    *float64  `json:"premarket_high"`
	PremktLow     *float64  `json:"premarket_low"`
	PriorDayHigh  *float64  `json:"prior_day_high"`
	PriorDayLow   *float64  `json:"prior_day_low"`
	VWAP          *float64  `json:"vwap"`
	KeySupport    []float64 `json:"key_support"`
	KeyResistance []float64 `json:"key_resistance"`
}

type TargetsDecision struct {
	Conservative *float64 `json:"conservative_underlying_target"`
	Base         *float64 `json:"base_underlying_target"`
	Stretch      *float64 `json:"stretch_underlying_target"`
}

type ContractDecision struct {
	Selected         bool    `json:"selected"`
	Type             string  `json:"type"` // "call" | "put" | "none"
	ExpirationDTE    *int    `json:"expiration_dte"`
	TargetDeltaRange *string `json:"target_delta_range"`
	StrikeLogi       string  `json:"strike_logic"`
	LiquidityCheck   string  `json:"liquidity_check"` // "pass" | "fail" | "borderline"
	Reason           string  `json:"reason"`
}

type EntryTrigger struct {
	Type                   string   `json:"type"` // "breakout_stop_limit" | "breakdown_stop_limit" | "pullback_limit" | "rejection_limit" | "none"
	UnderlyingTriggerPrice *float64 `json:"underlying_trigger_price"`
	Explanation            string   `json:"explanation"`
	Invalidation           string   `json:"invalidation"`
}

type RiskPlan struct {
	InitialStopLossPct       float64 `json:"initial_stop_loss_pct"`
	FirstProfitZonePct       string  `json:"first_profit_zone_pct"`
	BreakevenShiftTriggerPct float64 `json:"breakeven_shift_trigger_pct"`
	TrailActivationPct       string  `json:"trail_activation_pct"`
	TrailingMethod           string  `json:"trailing_method"` // "premium_pct" | "structure_based" | "none"
	TrailingValue            string  `json:"trailing_value"`
}

type HoldOvernightRule struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

type DailyReviewRule struct {
	NextActionIfOpen string `json:"next_action_if_open"` // "hold" | "tighten_trail" | "partial_profit" | "exit_now" | "exit_on_trigger"
	Reason           string `json:"reason"`
}

type PositionReview struct {
	Ticker     string `json:"ticker"`
	OptionType string `json:"option_type"` // "call" | "put"
	Status     string `json:"status"`      // "hold" | "tighten_trail" | "partial_profit" | "exit_now" | "exit_on_trigger"
	Reason     string `json:"reason"`
}

// ── Backward-compat types (used by workflow activities until refactored) ────────

// Recommendation is the legacy response type kept for workflow activities.
type Recommendation struct {
	Action          string   `json:"action"` // BUY | WATCH | INVALID
	Ticker          string   `json:"ticker"`
	Confidence      float64  `json:"confidence"`
	TimeHorizon     string   `json:"time_horizon"`
	EntryNote       string   `json:"entry_note"`
	StopNote        string   `json:"stop_note"`
	EvidenceFor     []string `json:"evidence_for"`
	EvidenceAgainst []string `json:"evidence_against"`
	KeyRisk         string   `json:"key_risk"`
	KillSwitch      *string  `json:"kill_switch_reason"`
	RawResponse     string
}

// ── Slim review structs (compact payload — ~100 chars input + output per candidate) ──

// SlimCandidate is the minimal input sent to Claude per candidate.
type SlimCandidate struct {
	T     string   `json:"t"`              // ticker
	Fam   string   `json:"fam"`            // setup family
	Score int      `json:"score"`          // pre-screen score 0-100
	Px    float64  `json:"px"`             // close price
	RSI   float64  `json:"rsi"`            // RSI-14
	MACD  float64  `json:"macd"`           // MACD histogram
	RVol  float64  `json:"rvol"`           // relative volume
	Trend string   `json:"trend"`          // bullish|bearish|mixed
	T1    float64  `json:"t1"`             // base target
	T2    float64  `json:"t2"`             // stretch target
	RC    []string `json:"rc,omitempty"`   // reason codes from engine
	Earn  bool     `json:"earn,omitempty"` // earnings risk
	Opts  bool     `json:"opts"`           // option chain available
}

// SlimReview is the minimal output Claude returns per candidate.
type SlimReview struct {
	T      string   `json:"t"`      // ticker
	Status string   `json:"status"` // structural_candidate|entry_ready|watch_only|rejected
	Dir    string   `json:"dir"`    // call|put|none
	Score  int      `json:"score"`  // 0-100
	FD     string   `json:"fd"`     // paper_trade_now|place_trigger_only|watchlist_only|reject
	Why    string   `json:"why"`    // ≤15 words
	RC     []string `json:"rc,omitempty"`
}

// SlimResponse is the full slim output from Claude.
type SlimResponse struct {
	Regime string       `json:"regime"` // bullish|bearish|mixed
	Bias   string       `json:"bias"`   // long_calls|long_puts|selective_both|no_trade_bias
	C      []SlimReview `json:"c"`
}

// slimSystemPrompt is the compact prompt for the slim review path.
const slimSystemPrompt = `You are a paper-trade options reviewer. Given prescreened candidates, decide status and direction.

Rules:
- structural_candidate = setup exists, entry not yet triggered
- entry_ready = entry trigger near/met, worth watching for open confirmation
- watch_only = was interesting but no longer actionable
- rejected = fails key requirement

Output ONLY raw JSON (no fences, no prose):
{"regime":"bullish|bearish|mixed","bias":"long_calls|long_puts|selective_both|no_trade_bias","c":[{"t":"TICKER","status":"structural_candidate|entry_ready|watch_only|rejected","dir":"call|put|none","score":0-100,"fd":"paper_trade_now|place_trigger_only|watchlist_only|reject","why":"<15 words","rc":["code1"]}]}

Every candidate in input must appear in output. Be conservative — prefer structural_candidate over entry_ready unless clearly ready.`

// ReviewCandidates sends a compact payload to Claude and returns slim reviews.
// Each candidate takes ~80 chars input + ~80 chars output. Handles 50+ candidates in one call.
func (c *Client) ReviewCandidates(vix, btc float64, regime string, candidates []SlimCandidate) (SlimResponse, error) {
	type payload struct {
		VIX    float64         `json:"vix"`
		BTC    float64         `json:"btc"`
		Regime string          `json:"regime"`
		C      []SlimCandidate `json:"c"`
	}
	p := payload{VIX: vix, BTC: btc, Regime: regime, C: candidates}
	b, err := json.Marshal(p)
	if err != nil {
		return SlimResponse{}, fmt.Errorf("claude slim: marshal: %w", err)
	}

	raw, err := c.callAPIWithSystem(string(b), slimSystemPrompt)
	if err != nil {
		return SlimResponse{}, fmt.Errorf("claude slim: API: %w", err)
	}

	log.Printf("claude slim: response length=%d chars", len(raw))
	jsonStr := extractJSON(raw)
	if jsonStr == "" {
		return SlimResponse{}, fmt.Errorf("claude slim: no JSON (len=%d raw: %s)", len(raw), truncate(raw, 200))
	}
	var resp SlimResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return SlimResponse{}, fmt.Errorf("claude slim: unmarshal: %w (raw: %s)", err, truncate(jsonStr, 200))
	}
	return resp, nil
}

// ── Entry confirmation payload (v7) ──────────────────────────────────────────
//
// Used at opening time: after deterministic signals are evaluated, we build a
// rich EntryConfirmationPayload for each shortlisted candidate and send it to
// Claude. Claude is the final authority — it can CONFIRM or REJECT.
//
// The payload combines:
//   - Daily context: all technical indicators from overnight analysis
//   - Opening context: first 5/10 min bars, VWAP, opening range
//   - Selected contract: the best option contract from chain screening
//   - Risk plan: stop, targets, R/R from ATR analysis
//   - Hard blocks: any auto_reject signals fired (Claude cannot override these)
//   - Deterministic signals: soft signals (evidence, not authority)

// EntryConfirmationPayload is the input to Claude's ConfirmEntry call.
type EntryConfirmationPayload struct {
	// Broad market context at open
	MarketContext MarketContext `json:"market_context"`

	// All shortlisted candidates for this confirmation run
	Candidates []ConfirmationCandidate `json:"candidates"`
}

// ConfirmationCandidate is one entry_ready ticker sent for opening confirmation.
type ConfirmationCandidate struct {
	Ticker      string `json:"ticker"`
	SetupFamily string `json:"setup_family"` // e.g. "bullish_continuation"
	Direction   string `json:"direction"`    // "call" | "put"

	// Daily context — from overnight analysis
	Daily DailyContext `json:"daily"`

	// Opening context — from first 5-10 min bars
	Opening OpeningContext `json:"opening"`

	// Selected option contract
	Contract ConfirmationContract `json:"contract"`

	// Risk plan from ATR analysis
	Risk RiskContext `json:"risk"`

	// Deterministic opening evidence (soft signals — evidence, not authority)
	DeterministicSignals DeterministicSignals `json:"deterministic_signals"`

	// Hard blocks: if any fired, Claude MUST reject (these are non-overridable)
	HardBlocks HardBlockSummary `json:"hard_blocks"`
}

// DailyContext holds all technical indicator values from overnight analysis.
type DailyContext struct {
	Close       float64 `json:"close"`
	EMA20       float64 `json:"ema20"`
	EMA50       float64 `json:"ema50"`
	EMA100      float64 `json:"ema100"`
	RSI         float64 `json:"rsi"`
	MACDHist    float64 `json:"macd_hist"`
	VolumeRatio float64 `json:"volume_ratio"`
	ATR14       float64 `json:"atr14"`

	// v7 extended indicators
	RealVol20      float64 `json:"real_vol_20,omitempty"`       // realized vol, 20-day
	VolScaledMom63 float64 `json:"vol_scaled_mom_63,omitempty"` // vol-normalized 63d momentum
	Entropy30      float64 `json:"entropy_30,omitempty"`        // Shannon entropy of 30d returns
	BollingerWidth float64 `json:"bollinger_width,omitempty"`   // Bollinger band width
	SqueezeRatio   float64 `json:"squeeze_ratio,omitempty"`     // Bollinger/Keltner ratio

	FinalScore   float64 `json:"final_score"` // engine's deterministic score
	PriorDayHigh float64 `json:"prior_day_high"`
	PriorDayLow  float64 `json:"prior_day_low"`
}

// OpeningContext holds first 5/10 min bar data at market open.
type OpeningContext struct {
	VWAP             float64 `json:"vwap"`
	OpeningRangeHigh float64 `json:"opening_range_high"` // first 10 min high
	OpeningRangeLow  float64 `json:"opening_range_low"`  // first 10 min low
	First5mClose     float64 `json:"first_5m_close"`
	First10mClose    float64 `json:"first_10m_close"`
	OpeningVolume    float64 `json:"opening_volume_ratio"` // first 10m vs avg 10m
}

// ConfirmationContract holds the pre-selected option contract details.
type ConfirmationContract struct {
	Symbol       string  `json:"symbol"` // OCC symbol e.g. "SPY260620C00580000"
	Type         string  `json:"type"`   // "call" | "put"
	Strike       float64 `json:"strike"`
	Expiration   string  `json:"expiration"` // "2026-06-20"
	DTE          int     `json:"dte"`
	Delta        float64 `json:"delta"`
	MidPrice     float64 `json:"mid_price"`          // current option mid
	BidAskSpread float64 `json:"bid_ask_spread_pct"` // spread as % of mid
	OpenInterest int     `json:"open_interest"`
	OptionVolume int     `json:"option_volume"` // today's traded volume
}

// RiskContext holds stop, target, and R/R from ATR analysis.
type RiskContext struct {
	EntryPrice       float64 `json:"entry_price"`        // option mid price to pay
	StopLossPct      float64 `json:"stop_loss_pct"`      // stop as % of premium
	BaseTargetPct    float64 `json:"base_target_pct"`    // base profit target as % of premium
	StretchTargetPct float64 `json:"stretch_target_pct"` // stretch target
	RRRatio          float64 `json:"rr_ratio"`           // reward/risk ratio
}

// DeterministicSignals is the output of the opening confirmation check.
// Claude sees these as evidence to weigh — they are NOT the final decision.
type DeterministicSignals struct {
	TrueCount    int      `json:"true_count"` // number of bullish signals
	TotalChecked int      `json:"total_checked"`
	SoftMinMet   bool     `json:"soft_min_met"`      // >= deterministic_signals_soft_min
	Details      []string `json:"details,omitempty"` // e.g. ["breakout_holds", "volume_support"]
}

// HardBlockSummary lists any auto_reject signals that fired.
// If any are present, Claude MUST decline — these are hard blocks.
type HardBlockSummary struct {
	Fired   []string `json:"fired,omitempty"` // e.g. ["hard_open_reversal"]
	IsClean bool     `json:"is_clean"`        // true = no hard blocks fired
}

// EntryConfirmationDecision is Claude's response per candidate in ConfirmEntry.
type EntryConfirmationDecision struct {
	Ticker     string  `json:"ticker"`
	Decision   string  `json:"decision"`   // "CONFIRM" | "REJECT"
	Confidence float64 `json:"confidence"` // 0.0-1.0
	Reason     string  `json:"reason"`     // ≤25 words

	// Contract to use (Claude may refine but typically echoes input)
	ContractSymbol string  `json:"contract_symbol,omitempty"`
	LimitPrice     float64 `json:"limit_price,omitempty"` // suggested entry premium
}

// EntryConfirmationResponse is Claude's full response for a ConfirmEntry call.
type EntryConfirmationResponse struct {
	Regime    string                      `json:"regime"` // "bullish" | "bearish" | "mixed"
	Decisions []EntryConfirmationDecision `json:"decisions"`
}

// confirmEntrySystemPrompt is the system prompt for the ConfirmEntry endpoint.
const confirmEntrySystemPrompt = `You are a paper-trade options entry confirmation engine.

At market open you receive shortlisted candidates that passed overnight analysis (entry_ready status).
You must decide whether to CONFIRM or REJECT each entry based on opening price action and the evidence provided.

Rules:
- Hard blocks (hard_blocks.fired is non-empty): ALWAYS REJECT. These are non-overridable.
- If deterministic_signals.soft_min_met is false: weigh carefully — lean toward REJECT unless opening context is exceptional.
- Confidence must be >= 0.65 to CONFIRM. If uncertain, REJECT.
- Only LONG calls or LONG puts — no spreads, no short options.
- Paper trading only — no live money.
- You may confirm 0, 1, or multiple candidates. No minimum. Empty trades are valid.

Output ONLY raw JSON (no fences, no prose):
{"regime":"bullish|bearish|mixed","decisions":[{"ticker":"AAPL","decision":"CONFIRM|REJECT","confidence":0.80,"reason":"<25 words","contract_symbol":"AAPL260620C00200000","limit_price":3.50}]}

Every input candidate must appear in output. Be conservative — one bad entry costs more than a missed opportunity.`

// ConfirmEntry sends shortlisted entry_ready candidates to Claude for final
// opening confirmation. Returns per-ticker CONFIRM/REJECT decisions.
// Claude is the final authority; deterministic signals are evidence only.
func (c *Client) ConfirmEntry(payload EntryConfirmationPayload) (EntryConfirmationResponse, error) {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return EntryConfirmationResponse{}, fmt.Errorf("claude confirm-entry: marshal: %w", err)
	}

	log.Printf("claude confirm-entry: sending %d candidates", len(payload.Candidates))

	raw, err := c.callAPIWithSystem(string(b), confirmEntrySystemPrompt)
	if err != nil {
		return EntryConfirmationResponse{}, fmt.Errorf("claude confirm-entry: API: %w", err)
	}

	log.Printf("claude confirm-entry: response length=%d chars", len(raw))
	jsonStr := extractJSON(raw)
	if jsonStr == "" {
		return EntryConfirmationResponse{}, fmt.Errorf("claude confirm-entry: no JSON (raw: %s)", truncate(raw, 200))
	}

	var resp EntryConfirmationResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return EntryConfirmationResponse{}, fmt.Errorf("claude confirm-entry: unmarshal: %w (raw: %s)", err, truncate(jsonStr, 200))
	}
	return resp, nil
}

// ── Main API methods ──────────────────────────────────────────────────────────

// DecideOptions is the primary method for the options engine.
// It sends a RuntimePayload to Claude and returns an OptionsDecision.
func (c *Client) DecideOptions(payload RuntimePayload) (OptionsDecision, error) {
	payloadJSON, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return OptionsDecision{}, fmt.Errorf("claude: marshal payload: %w", err)
	}

	raw, err := c.callAPIWithSystem(string(payloadJSON), c.systemPrompt)
	if err != nil {
		return OptionsDecision{}, fmt.Errorf("claude: API call: %w", err)
	}

	log.Printf("claude: raw response length = %d chars, first200=%q", len(raw), truncate(raw, 200))
	tail := raw
	if len(tail) > 100 {
		tail = tail[len(tail)-100:]
	}
	log.Printf("claude: raw response last100=%q", tail)
	jsonStr := extractJSON(raw)
	if jsonStr == "" {
		return OptionsDecision{}, fmt.Errorf("claude: no JSON found in response (len=%d raw: %s)", len(raw), truncate(raw, 300))
	}

	var decision OptionsDecision
	if err := json.Unmarshal([]byte(jsonStr), &decision); err != nil {
		return OptionsDecision{}, fmt.Errorf("claude: unmarshal decision: %w (raw: %s)", err, truncate(raw, 300))
	}

	return decision, nil
}

// GenerateText sends a free-text prompt and returns Claude's raw text response.
// Used for weekly review summaries that don't require JSON output.
func (c *Client) GenerateText(systemPrompt, userMessage string) (string, error) {
	return c.callAPIWithSystem(userMessage, systemPrompt)
}

// ── Internal helpers ─────────────────────────────────────────────────────────

func (c *Client) callAPIWithSystem(userMessage, systemPrompt string) (string, error) {
	payload := map[string]interface{}{
		"model":      c.model,
		"max_tokens": c.maxOutputTokens,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userMessage},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("claude: marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("claude: build request: %w", err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("claude: request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("claude: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return "", fmt.Errorf("claude: decode envelope: %w", err)
	}
	if len(envelope.Content) == 0 {
		return "", fmt.Errorf("claude: empty content in response")
	}
	if envelope.StopReason == "max_tokens" {
		return "", fmt.Errorf("claude: response truncated by max_tokens limit — increase CLAUDE_MAX_OUTPUT_TOKENS (current=%d, response_len=%d)", c.maxOutputTokens, len(envelope.Content[0].Text))
	}
	return envelope.Content[0].Text, nil
}

// extractJSON finds the first complete {...} block in a string.
// Strips markdown code fences (```json ... ```) before searching.
// extractJSON strips optional markdown fences and returns the first valid
// JSON object in s. It uses json.RawMessage to find the object boundary
// correctly — a simple brace counter breaks when { or } appear inside
// string values (e.g. thesis text, reasoning fields).
func extractJSON(s string) string {
	// Strip markdown code fences if present.
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "```") {
		if nl := strings.Index(trimmed, "\n"); nl != -1 {
			trimmed = trimmed[nl+1:]
		}
		if end := strings.LastIndex(trimmed, "```"); end != -1 {
			trimmed = strings.TrimSpace(trimmed[:end])
		}
		s = trimmed
	}

	// Find the start of the JSON object.
	start := strings.Index(s, "{")
	if start == -1 {
		return ""
	}
	s = s[start:]

	// Use json.RawMessage to find the exact end of the JSON object.
	// This correctly handles { and } inside string values.
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err == nil {
		return string(raw)
	}

	// Fallback: try each position until a valid object is found.
	// Handles leading garbage before the real object.
	for i := 1; i < len(s); i++ {
		if s[i] == '{' {
			var r json.RawMessage
			if err := json.Unmarshal([]byte(s[i:]), &r); err == nil {
				return string(r)
			}
		}
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
