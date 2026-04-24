// internal/strategy/rules.go
//
// WHAT: Go struct hierarchy that mirrors strategy_rules.yaml v6, plus loader.
//
// WHY:  All strategy thresholds, family definitions, scoring weights, and
//       liquidity filters live in strategy_rules.yaml as the single source of
//       truth. This file parses that YAML at startup so the engine, handler,
//       and confirmation evaluator can reference typed fields rather than
//       magic constants.
//
// HOW:  LoadRules("strategy_rules.yaml") is called once at startup. The returned
//       *Rules is passed to strategy.NewEngine() and used directly by handlers.
//
// WHAT BREAKS: yaml.v3 silently ignores unknown keys (no error). A missing
//              numeric field defaults to zero — which disables that check.
//              Always verify thresholds after editing YAML with:
//              fmt.Printf("%+v\n", rules.Families["bullish_continuation"].Scoring)

package strategy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Rules is the decoded form of strategy_rules.yaml v7.
type Rules struct {
	Version int `yaml:"version"`

	Global    GlobalConfig    `yaml:"global"`
	Regime    RegimeConfig    `yaml:"regime"`
	Penalties PenaltiesConfig `yaml:"penalties"`

	// Per-family complete policy (preconditions + scoring + options + hold).
	Families map[string]FamilyConfig `yaml:"families"`

	PatternScoreConfig PatternScoreConfig `yaml:"pattern_scores"`
	AntiPatternConfig  AntiPatternConfig  `yaml:"anti_patterns"`
	EventBlocks        EventBlocksConfig  `yaml:"event_blocks"`
	StateRules         StateRulesConfig   `yaml:"state_rules"`

	// Kept at top-level for handlers.go/activities.go backward compatibility.
	// Per-family DTE/delta bands live in Families[*].Options.
	OptionsTranslation OptionsTranslationConfig `yaml:"options_translation"`

	OpenConfirmation   OpenConfirmationConfig   `yaml:"open_confirmation"`
	Scoring            ScoringConfig            `yaml:"scoring"`
	DailyOutput        DailyOutputConfig        `yaml:"daily_output"`
	TradeFrequency     TradeFrequencyConfig     `yaml:"trade_frequency"`
	ClaudeConfirmation ClaudeConfirmationConfig `yaml:"claude_confirmation"`
	Schedule           ScheduleConfig           `yaml:"schedule"`
	Risk               RiskConfig               `yaml:"risk"`
}

// ── Risk ──────────────────────────────────────────────────────────────────────

// RiskConfig holds the option lifecycle and mechanical exit rules.
// These are applied universally across all families unless a family overrides DTE.
type RiskConfig struct {
	OptionLifecycle OptionLifecycleConfig `yaml:"option_lifecycle"`
	MechanicalExits MechanicalExitsConfig `yaml:"mechanical_exits"`
}

// OptionLifecycleConfig defines DTE and contract-count policy for all entries.
type OptionLifecycleConfig struct {
	DTEMin            int `yaml:"dte_min"`             // minimum DTE at entry (hard lower bound)
	DTEMax            int `yaml:"dte_max"`             // maximum DTE at entry (hard upper bound)
	TargetDTE         int `yaml:"target_dte"`          // preferred DTE; selector ranks by |dte - target|
	AvoidDTEBelow     int `yaml:"avoid_dte_below"`     // hard floor: never select DTE below this
	ContractsPerTrade int `yaml:"contracts_per_trade"` // always 1 for paper trading
}

// MechanicalExitsConfig defines hard exit rules evaluated every risk-check cycle.
// These are NOT overridable by Claude. Claude can only approve hold_overnight.
type MechanicalExitsConfig struct {
	Enabled                         bool    `yaml:"enabled"`
	PremiumStopLossPct              float64 `yaml:"premium_stop_loss_pct"`                // exit if premium down this % from entry
	PremiumTakeProfitPct            float64 `yaml:"premium_take_profit_pct"`              // exit if premium up this % from entry
	PremiumTrailingStartPct         float64 `yaml:"premium_trailing_start_pct"`           // activate trail once premium up this %
	PremiumTrailingGivebackPct      float64 `yaml:"premium_trailing_giveback_pct"`        // exit if gives back this % from peak after trail starts
	ForceEODExitUnlessHoldConfirmed bool    `yaml:"force_eod_exit_unless_hold_confirmed"` // exit at EOD unless hold_overnight_approved
	MaxHoldDaysWithoutReconfirm     int     `yaml:"max_hold_days_without_reconfirm"`      // require reconfirm after N hold days
}

// ── Global ────────────────────────────────────────────────────────────────────

// GlobalConfig holds feature computation windows and data quality gates.
type GlobalConfig struct {
	FeatureWindows FeatureWindowsConfig `yaml:"feature_windows"`
	DataQuality    DataQualityConfig    `yaml:"data_quality"`
}

// FeatureWindowsConfig enumerates all indicator periods the engine uses.
// Changing these here changes what the engine computes.
type FeatureWindowsConfig struct {
	EMAShort         int `yaml:"ema_short"`
	EMAMedium        int `yaml:"ema_medium"`
	EMALong          int `yaml:"ema_long"`
	RSIPeriod        int `yaml:"rsi_period"`
	MACDFast         int `yaml:"macd_fast"`
	MACDSlow         int `yaml:"macd_slow"`
	MACDSignal       int `yaml:"macd_signal"`
	ATRPeriod        int `yaml:"atr_period"`
	VolumeAvgPeriod  int `yaml:"volume_avg_period"`
	EMASlopeLookback int `yaml:"ema_slope_lookback"`
	// v7: extended windows for new scoring sleeves
	RealizedVolShort int `yaml:"realized_vol_short"` // realized vol lookback (short)
	RealizedVolLong  int `yaml:"realized_vol_long"`  // realized vol lookback (long)
	MomentumShort    int `yaml:"momentum_short"`     // vol-scaled momentum (short, ~63d)
	MomentumLong     int `yaml:"momentum_long"`      // vol-scaled momentum (long, ~126d)
	Entropy          int `yaml:"entropy"`            // Shannon entropy of returns
	Bollinger        int `yaml:"bollinger"`          // Bollinger width window
}

// DataQualityConfig holds minimum bar requirements before scoring begins.
type DataQualityConfig struct {
	MinBarsRequired int `yaml:"min_bars_required"`
}

// ── Regime ────────────────────────────────────────────────────────────────────

// RegimeConfig holds universal market guardrails applied before family scoring.
type RegimeConfig struct {
	HardBlocks RegimeHardBlocks `yaml:"hard_blocks"`
}

// RegimeHardBlocks are absolute gates. Failing either blocks all families.
type RegimeHardBlocks struct {
	VIXMax      float64 `yaml:"vix_max"`
	BTCRoc20Min float64 `yaml:"btc_roc20_min"`
}

// ── Penalties ─────────────────────────────────────────────────────────────────

// PenaltiesConfig holds score deductions applied after weighted family scoring.
// Values are on the 0-100 score scale.
type PenaltiesConfig struct {
	LateStageExtension     float64 `yaml:"late_stage_extension"`
	DistributionSevere     float64 `yaml:"distribution_severe"`
	RSIOverextendedBullish float64 `yaml:"rsi_overextended_bullish"`
	RSIOversoldBearish     float64 `yaml:"rsi_oversold_bearish"`
}

// ── Family config (one block per family in YAML) ──────────────────────────────

// FamilyConfig is the complete per-family policy block.
// All strategy parameters for a family live here — no split across sections.
type FamilyConfig struct {
	Direction   string `yaml:"direction"`   // "bullish" | "bearish"
	OptionType  string `yaml:"option_type"` // "call" | "put"
	Description string `yaml:"description"`

	// MaxScanStatus caps the lifecycle status the daily scan engine may assign.
	// When non-empty (e.g. "structural_candidate"), the engine will never promote
	// to entry_ready regardless of score. Used by bearish_exhaustion_reversal to
	// ensure entry_ready can only come from the opening confirmation activity.
	MaxScanStatus string `yaml:"max_scan_status"`

	Preconditions   FamilyPreconditions   `yaml:"preconditions"`
	Scoring         FamilyScoringConfig   `yaml:"scoring"`
	RSI             FamilyRSIBands        `yaml:"rsi"`
	ExtensionPct    FamilyExtensionBands  `yaml:"extension_pct"`
	Volume          FamilyVolumeBands     `yaml:"volume"`
	EMAGapPct       FamilyEMAGapBands     `yaml:"ema_gap_pct"`
	EntryConditions FamilyEntryConditions `yaml:"entry_conditions"`
	Options         FamilyOptionsBand     `yaml:"options"`
	HoldWindow      HoldWindow            `yaml:"hold_window"`
}

// FamilyPreconditions are binary gates. All flagged true must pass.
// One failure → family skipped entirely (not scored).
type FamilyPreconditions struct {
	// Bullish continuation
	EMA20AboveEMA50  bool `yaml:"ema20_above_ema50"`
	EMA20AboveEMA100 bool `yaml:"ema20_above_ema100"`
	CloseAboveEMA20  bool `yaml:"close_above_ema20"`
	MACDHistPositive bool `yaml:"macd_histogram_positive"`
	BTCNotNegative   bool `yaml:"btc_regime_not_negative"`
	// Bearish continuation
	EMA20BelowEMA50  bool `yaml:"ema20_below_ema50"`
	EMA20BelowEMA100 bool `yaml:"ema20_below_ema100"`
	CloseBelowEMA20  bool `yaml:"close_below_ema20"`
	MACDHistNegative bool `yaml:"macd_histogram_negative"`
	// Momentum families
	EMA20SlopePositive bool `yaml:"ema20_slope_positive"`
	EMA20SlopeNegative bool `yaml:"ema20_slope_negative"`
	// Exhaustion reversal: numeric threshold gates (zero = not enforced)
	RSIMinPrecondition float64 `yaml:"rsi_min_precondition"` // RSI must be >= this (e.g. 72)
	ATRExtensionMin    float64 `yaml:"atr_extension_min"`    // (close-EMA20)/ATR14 must be >= this (e.g. 1.8)
}

// FamilyScoringConfig holds the 5 dimension weights and promotion thresholds.
type FamilyScoringConfig struct {
	Weights    ScoringWeights    `yaml:"weights"`
	Thresholds ScoringThresholds `yaml:"thresholds"`
}

// ScoringWeights must sum to 100. Engine scores each dimension 0.0-1.0.
type ScoringWeights struct {
	TrendStructure      int `yaml:"trend_structure"`
	MomentumAlignment   int `yaml:"momentum_alignment"`
	VolumeParticipation int `yaml:"volume_participation"`
	EntryQuality        int `yaml:"entry_quality"`
	PatternStrength     int `yaml:"pattern_strength"`
}

// ScoringThresholds are minimum scores for each lifecycle status.
type ScoringThresholds struct {
	StructuralCandidate float64 `yaml:"structural_candidate"`
	EntryReady          float64 `yaml:"entry_ready"`
}

// FamilyRSIBands defines ideal and acceptable RSI ranges for scoring.
type FamilyRSIBands struct {
	IdealMin      float64 `yaml:"ideal_min"`
	IdealMax      float64 `yaml:"ideal_max"`
	AcceptableMin float64 `yaml:"acceptable_min"`
	AcceptableMax float64 `yaml:"acceptable_max"`
}

// FamilyExtensionBands defines how far price may be from EMA20.
// extension_pct = abs(close - EMA20) / EMA20 * 100
type FamilyExtensionBands struct {
	IdealMax      float64 `yaml:"ideal_max"`      // full entry_quality score
	AcceptableMax float64 `yaml:"acceptable_max"` // half score
	HardReject    float64 `yaml:"hard_reject"`    // zero entry_quality score
}

// FamilyVolumeBands defines relative volume thresholds for scoring.
type FamilyVolumeBands struct {
	StrongMin   float64 `yaml:"strong_min"`   // full volume score
	AdequateMin float64 `yaml:"adequate_min"` // partial score
}

// FamilyEMAGapBands defines EMA20-EMA50 separation quality thresholds.
type FamilyEMAGapBands struct {
	StrongMin   float64 `yaml:"strong_min"`
	AdequateMin float64 `yaml:"adequate_min"`
}

// FamilyEntryConditions are hard binary checks evaluated AFTER scoring.
// Even if score >= entry_ready threshold, all must pass for entry_ready status.
// Failing any → structural_candidate (never rejected by entry conditions alone).
type FamilyEntryConditions struct {
	VolumeMin       float64 `yaml:"volume_min"`
	RSIMin          float64 `yaml:"rsi_min"`
	RSIMax          float64 `yaml:"rsi_max"`
	ExtensionMaxPct float64 `yaml:"extension_max_pct"`
}

// FamilyOptionsBand holds DTE and delta target bands for contract selection.
type FamilyOptionsBand struct {
	DTEMin   int     `yaml:"dte_min"`
	DTEMax   int     `yaml:"dte_max"`
	DeltaMin float64 `yaml:"delta_min"`
	DeltaMax float64 `yaml:"delta_max"`
}

// HoldWindow is the trade duration range in calendar days.
type HoldWindow struct {
	Min  int `yaml:"min"`
	Base int `yaml:"base"`
	Max  int `yaml:"max"`
}

// ── Pattern scores ─────────────────────────────────────────────────────────────

// PatternScoreConfig maps pattern names → integer point values.
type PatternScoreConfig struct {
	Bullish map[string]int `yaml:"bullish"`
	Bearish map[string]int `yaml:"bearish"`
}

// ── Anti-patterns ─────────────────────────────────────────────────────────────

// AntiPatternConfig lists names that trigger penalties from PenaltiesConfig.
type AntiPatternConfig struct {
	BullishReject []string `yaml:"bullish_reject"`
	BearishReject []string `yaml:"bearish_reject"`
}

// ── Event blocks ──────────────────────────────────────────────────────────────

// EventBlocksConfig defines earnings/binary-event blackout windows.
type EventBlocksConfig struct {
	EarningsBlackoutDays    int    `yaml:"earnings_blackout_days"`
	BinaryEventBlackoutDays int    `yaml:"binary_event_blackout_days"`
	IfBlockedStatus         string `yaml:"if_blocked_status"`
}

// ── State rules ───────────────────────────────────────────────────────────────

// StateRulesConfig contains lifecycle flags read by engine and handlers.
type StateRulesConfig struct {
	StructuralCandidateIsWatchlistOnly bool `yaml:"structural_candidate_is_watchlist_only"`
	EntryReadyCanSurfacePreopen        bool `yaml:"entry_ready_can_surface_preopen"`
	ConfirmedRequiredForTradeOutput    bool `yaml:"confirmed_required_for_trade_output"`
	BlockedByEventOverridesEntryReady  bool `yaml:"blocked_by_event_overrides_entry_ready"`
	BlockedByEventOverridesConfirmed   bool `yaml:"blocked_by_event_overrides_confirmed"`
}

// ── Options translation (handlers.go compat) ──────────────────────────────────

// OptionsTranslationConfig holds liquidity filters and status visibility rules.
// The top-level section is kept for handlers.go backward compatibility.
// Per-family DTE/delta bands now live in FamilyConfig.Options.
type OptionsTranslationConfig struct {
	LiquidityFilters       LiquidityFilters `yaml:"liquidity_filters"`
	HideOptionsForStatuses []string         `yaml:"hide_options_for_statuses"`
}

// LiquidityFilters are the app-side chain quality thresholds applied before
// contract selection. Contracts failing these are stripped before Claude sees them.
type LiquidityFilters struct {
	MinOpenInterest         int     `yaml:"min_open_interest"`
	MinOptionVolume         int     `yaml:"min_option_volume"`
	MaxBidAskSpreadPctOfMid float64 `yaml:"max_bid_ask_spread_pct_of_mid"`
}

// ── Open confirmation ─────────────────────────────────────────────────────────

// OpenConfirmationConfig maps to open_confirmation in strategy_rules.yaml.
type OpenConfirmationConfig struct {
	Enabled                   bool               `yaml:"enabled"`
	ConfirmationWindowMinutes int                `yaml:"confirmation_window_minutes"`
	MinTrueSignalsToConfirm   int                `yaml:"min_true_signals_to_confirm"`
	Checks                    ConfirmationChecks `yaml:"checks"`
	AutoReject                AutoRejectChecks   `yaml:"auto_reject"`
}

// ConfirmationChecks mirrors the checks sub-block.
type ConfirmationChecks struct {
	BreakoutOrReclaimHolds                 bool `yaml:"breakout_or_reclaim_holds"`
	OpeningRangeCloseAboveMidpointForCalls bool `yaml:"opening_range_close_above_midpoint_for_calls"`
	OpeningRangeCloseBelowMidpointForPuts  bool `yaml:"opening_range_close_below_midpoint_for_puts"`
	NoRejectionWickForCalls                bool `yaml:"no_rejection_wick_for_calls"`
	NoReversalTailForPuts                  bool `yaml:"no_reversal_tail_for_puts"`
	OpeningVolumeSupport                   bool `yaml:"opening_volume_support"`
	MarketOpenAlignment                    bool `yaml:"market_open_alignment"`
}

// AutoRejectChecks mirrors the auto_reject sub-block.
type AutoRejectChecks struct {
	DecisiveLevelLoss       bool `yaml:"decisive_level_loss"`
	WeakFirst10mClose       bool `yaml:"weak_first_10m_close"`
	HardOpenReversal        bool `yaml:"hard_open_reversal"`
	BroadMarketRiskoffShock bool `yaml:"broad_market_riskoff_shock"`
	DownsideRejectionVolume bool `yaml:"downside_rejection_volume"`
}

// ── Scoring visibility ────────────────────────────────────────────────────────

// ScoringConfig controls which lifecycle statuses expose a numeric score in the UI.
type ScoringConfig struct {
	ShowScoreOnlyFor []string `yaml:"show_score_only_for"`
	HideScoreFor     []string `yaml:"hide_score_for"`
}

// ── Daily output ──────────────────────────────────────────────────────────────

// DailyOutputConfig maps to daily_output in strategy_rules.yaml.
type DailyOutputConfig struct {
	AllowZeroTradeDay         bool   `yaml:"allow_zero_trade_day"`
	NoTradeDayWhenNoConfirmed bool   `yaml:"no_trade_day_when_no_confirmed"`
	NoTradeDayMessage         string `yaml:"no_trade_day_message"`
}

// ── Trade frequency ───────────────────────────────────────────────────────────

// TradeFrequencyConfig controls how many setups advance to Claude per day.
// mode: "active_paper" | "conservative" | "aggressive"
type TradeFrequencyConfig struct {
	Mode                   string  `yaml:"mode"`
	MaxEntryReadyToConfirm int     `yaml:"max_entry_ready_to_confirm"`
	MaxNewPositionsPerDay  int     `yaml:"max_new_positions_per_day"`
	MinEntryReadyScore     float64 `yaml:"min_entry_ready_score"`
	MinClaudeConfidence    float64 `yaml:"min_claude_confidence"`
	AllowZeroTradeDay      bool    `yaml:"allow_zero_trade_day"`
}

// ── Claude confirmation ───────────────────────────────────────────────────────

// ClaudeConfirmationConfig controls Claude's role as final authority at open.
// When enabled, deterministic signals are evidence; Claude makes the call.
type ClaudeConfirmationConfig struct {
	Enabled                            bool    `yaml:"enabled"`
	MinConfidence                      float64 `yaml:"min_confidence"`
	MaxCandidatesPerRun                int     `yaml:"max_candidates_per_run"`
	UseDeterministicOpeningEvidence    bool    `yaml:"use_deterministic_opening_evidence"`
	DeterministicSignalsSoftMin        int     `yaml:"deterministic_signals_soft_min"`
	DeterministicAutoRejectIsHardBlock bool    `yaml:"deterministic_auto_reject_is_hard_block"`
}

// ScheduleConfig holds the autonomous trading-day timeline.
// Times are strings in "HH:MM" 24-hour PT local format.
// The worker reads these at startup and converts to Temporal cron expressions
// using TimeZoneName="America/Los_Angeles" so DST is handled automatically.
type ScheduleConfig struct {
	Timezone                  string `yaml:"timezone"`                    // IANA name, e.g. "America/Los_Angeles"
	DailyScanTime             string `yaml:"daily_scan_time"`             // "06:25"
	OpeningConfirmationTime   string `yaml:"opening_confirmation_time"`   // "06:42"
	OpeningConfirmationCutoff string `yaml:"opening_confirmation_cutoff"` // "06:55"
	FirstPositionReviewTime   string `yaml:"first_position_review_time"`  // "07:15"
	ContinuationReviewTime    string `yaml:"continuation_review_time"`    // "07:45"
	EndOfDayReviewTime        string `yaml:"end_of_day_review_time"`      // "12:45"
	WeeklyReviewTime          string `yaml:"weekly_review_time"`          // "07:00"
}

// ── Loader ────────────────────────────────────────────────────────────────────

// LoadRules parses strategy_rules.yaml from the given path.
func LoadRules(path string) (*Rules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load rules %s: %w", path, err)
	}
	var r Rules
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse rules: %w", err)
	}
	return &r, nil
}

// ── Accessor helpers ──────────────────────────────────────────────────────────

// FamilyFor returns the config for the named setup family.
func (r *Rules) FamilyFor(family string) (FamilyConfig, bool) {
	cfg, ok := r.Families[family]
	return cfg, ok
}

// BlockedByEventStatus returns the status for event-blackout candidates.
func (r *Rules) BlockedByEventStatus() string {
	if r.EventBlocks.IfBlockedStatus != "" {
		return r.EventBlocks.IfBlockedStatus
	}
	return "blocked_by_event"
}

// ShouldHideScore returns true if the given status must not show a score in the UI.
func (r *Rules) ShouldHideScore(status string) bool {
	for _, s := range r.Scoring.HideScoreFor {
		if s == status {
			return true
		}
	}
	return false
}

// ShouldHideOptions returns true if option output must be suppressed for the status.
func (r *Rules) ShouldHideOptions(status string) bool {
	for _, s := range r.OptionsTranslation.HideOptionsForStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// NoTradeDayMessage returns the configured no-trade message.
func (r *Rules) NoTradeDayMessage() string {
	if r.DailyOutput.NoTradeDayMessage != "" {
		return r.DailyOutput.NoTradeDayMessage
	}
	return "No symbols reached confirmed status. Watchlist candidates may still exist."
}

// MinBarsRequired returns the data quality threshold (with safe default).
func (r *Rules) MinBarsRequired() int {
	if r.Global.DataQuality.MinBarsRequired > 0 {
		return r.Global.DataQuality.MinBarsRequired
	}
	return 35
}

// ── DefaultRules ─────────────────────────────────────────────────────────────

// DefaultRules returns safe fallback values used when YAML cannot be loaded.
// Thresholds match strategy_rules.yaml v6.
func DefaultRules() *Rules {
	bullContFamily := FamilyConfig{
		Direction: "bullish", OptionType: "call",
		Preconditions: FamilyPreconditions{
			EMA20AboveEMA50: true, EMA20AboveEMA100: true,
			CloseAboveEMA20: true, MACDHistPositive: true, BTCNotNegative: true,
		},
		Scoring: FamilyScoringConfig{
			Weights: ScoringWeights{
				TrendStructure: 30, MomentumAlignment: 25,
				VolumeParticipation: 20, EntryQuality: 15, PatternStrength: 10,
			},
			Thresholds: ScoringThresholds{StructuralCandidate: 45, EntryReady: 65},
		},
		RSI:          FamilyRSIBands{IdealMin: 55, IdealMax: 68, AcceptableMin: 50, AcceptableMax: 74},
		ExtensionPct: FamilyExtensionBands{IdealMax: 5.0, AcceptableMax: 10.0, HardReject: 15.0},
		Volume:       FamilyVolumeBands{StrongMin: 1.5, AdequateMin: 1.2},
		EMAGapPct:    FamilyEMAGapBands{StrongMin: 3.0, AdequateMin: 0.5},
		EntryConditions: FamilyEntryConditions{
			VolumeMin: 1.2, RSIMin: 50, RSIMax: 74, ExtensionMaxPct: 10.0,
		},
		Options:    FamilyOptionsBand{DTEMin: 10, DTEMax: 21, DeltaMin: 0.45, DeltaMax: 0.65},
		HoldWindow: HoldWindow{Min: 5, Base: 12, Max: 20},
	}
	bullMomFamily := FamilyConfig{
		Direction: "bullish", OptionType: "call",
		Preconditions: FamilyPreconditions{
			CloseAboveEMA20: true, EMA20SlopePositive: true, BTCNotNegative: true,
		},
		Scoring: FamilyScoringConfig{
			Weights: ScoringWeights{
				TrendStructure: 20, MomentumAlignment: 30,
				VolumeParticipation: 25, EntryQuality: 15, PatternStrength: 10,
			},
			Thresholds: ScoringThresholds{StructuralCandidate: 45, EntryReady: 68},
		},
		RSI:          FamilyRSIBands{IdealMin: 55, IdealMax: 72, AcceptableMin: 45, AcceptableMax: 80},
		ExtensionPct: FamilyExtensionBands{IdealMax: 5.0, AcceptableMax: 12.0, HardReject: 18.0},
		Volume:       FamilyVolumeBands{StrongMin: 2.0, AdequateMin: 1.5},
		EMAGapPct:    FamilyEMAGapBands{StrongMin: 0.0, AdequateMin: 0.0},
		EntryConditions: FamilyEntryConditions{
			VolumeMin: 1.5, RSIMin: 45, RSIMax: 80, ExtensionMaxPct: 12.0,
		},
		Options:    FamilyOptionsBand{DTEMin: 7, DTEMax: 14, DeltaMin: 0.45, DeltaMax: 0.70},
		HoldWindow: HoldWindow{Min: 2, Base: 7, Max: 10},
	}
	bearContFamily := FamilyConfig{
		Direction: "bearish", OptionType: "put",
		Preconditions: FamilyPreconditions{
			EMA20BelowEMA50: true, EMA20BelowEMA100: true,
			CloseBelowEMA20: true, MACDHistNegative: true,
		},
		Scoring: FamilyScoringConfig{
			Weights: ScoringWeights{
				TrendStructure: 30, MomentumAlignment: 25,
				VolumeParticipation: 20, EntryQuality: 15, PatternStrength: 10,
			},
			Thresholds: ScoringThresholds{StructuralCandidate: 45, EntryReady: 65},
		},
		RSI:          FamilyRSIBands{IdealMin: 32, IdealMax: 45, AcceptableMin: 26, AcceptableMax: 50},
		ExtensionPct: FamilyExtensionBands{IdealMax: 5.0, AcceptableMax: 10.0, HardReject: 15.0},
		Volume:       FamilyVolumeBands{StrongMin: 1.5, AdequateMin: 1.2},
		EMAGapPct:    FamilyEMAGapBands{StrongMin: 3.0, AdequateMin: 0.5},
		EntryConditions: FamilyEntryConditions{
			VolumeMin: 1.2, RSIMin: 26, RSIMax: 50, ExtensionMaxPct: 10.0,
		},
		Options:    FamilyOptionsBand{DTEMin: 10, DTEMax: 21, DeltaMin: 0.45, DeltaMax: 0.65},
		HoldWindow: HoldWindow{Min: 5, Base: 12, Max: 20},
	}
	bearMomFamily := FamilyConfig{
		Direction: "bearish", OptionType: "put",
		Preconditions: FamilyPreconditions{
			CloseBelowEMA20: true, EMA20SlopeNegative: true,
		},
		Scoring: FamilyScoringConfig{
			Weights: ScoringWeights{
				TrendStructure: 20, MomentumAlignment: 30,
				VolumeParticipation: 25, EntryQuality: 15, PatternStrength: 10,
			},
			Thresholds: ScoringThresholds{StructuralCandidate: 45, EntryReady: 68},
		},
		RSI:          FamilyRSIBands{IdealMin: 28, IdealMax: 45, AcceptableMin: 20, AcceptableMax: 55},
		ExtensionPct: FamilyExtensionBands{IdealMax: 5.0, AcceptableMax: 12.0, HardReject: 18.0},
		Volume:       FamilyVolumeBands{StrongMin: 2.0, AdequateMin: 1.5},
		EMAGapPct:    FamilyEMAGapBands{StrongMin: 0.0, AdequateMin: 0.0},
		EntryConditions: FamilyEntryConditions{
			VolumeMin: 1.5, RSIMin: 20, RSIMax: 55, ExtensionMaxPct: 12.0,
		},
		Options:    FamilyOptionsBand{DTEMin: 7, DTEMax: 14, DeltaMin: 0.45, DeltaMax: 0.70},
		HoldWindow: HoldWindow{Min: 2, Base: 7, Max: 10},
	}
	bearishExhaustionFamily := FamilyConfig{
		Direction: "bearish", OptionType: "put",
		MaxScanStatus: "structural_candidate",
		Preconditions: FamilyPreconditions{
			CloseAboveEMA20:    true,
			RSIMinPrecondition: 72,
			ATRExtensionMin:    1.8,
		},
		Scoring: FamilyScoringConfig{
			Weights: ScoringWeights{
				TrendStructure: 30, MomentumAlignment: 25,
				VolumeParticipation: 15, EntryQuality: 20, PatternStrength: 10,
			},
			Thresholds: ScoringThresholds{StructuralCandidate: 40, EntryReady: 999},
		},
		RSI:          FamilyRSIBands{IdealMin: 75, IdealMax: 85, AcceptableMin: 72, AcceptableMax: 90},
		ExtensionPct: FamilyExtensionBands{IdealMax: 999.0, AcceptableMax: 999.0, HardReject: 999.0},
		Volume:       FamilyVolumeBands{StrongMin: 1.5, AdequateMin: 1.0},
		EMAGapPct:    FamilyEMAGapBands{StrongMin: 0.0, AdequateMin: 0.0},
		EntryConditions: FamilyEntryConditions{
			VolumeMin: 1.3, RSIMin: 72, RSIMax: 95, ExtensionMaxPct: 999.0,
		},
		Options:    FamilyOptionsBand{DTEMin: 5, DTEMax: 14, DeltaMin: 0.35, DeltaMax: 0.55},
		HoldWindow: HoldWindow{Min: 1, Base: 3, Max: 7},
	}
	return &Rules{
		Version: 7,
		Global: GlobalConfig{
			FeatureWindows: FeatureWindowsConfig{
				EMAShort: 20, EMAMedium: 50, EMALong: 100,
				RSIPeriod: 14, MACDFast: 12, MACDSlow: 26, MACDSignal: 9,
				ATRPeriod: 14, VolumeAvgPeriod: 20, EMASlopeLookback: 5,
				RealizedVolShort: 20, RealizedVolLong: 40,
				MomentumShort: 63, MomentumLong: 126,
				Entropy: 30, Bollinger: 20,
			},
			DataQuality: DataQualityConfig{MinBarsRequired: 35},
		},
		Regime: RegimeConfig{
			HardBlocks: RegimeHardBlocks{VIXMax: 24.0, BTCRoc20Min: 0.0},
		},
		Penalties: PenaltiesConfig{
			LateStageExtension: 15, DistributionSevere: 20,
			RSIOverextendedBullish: 10, RSIOversoldBearish: 10,
		},
		Families: map[string]FamilyConfig{
			"bullish_continuation":        bullContFamily,
			"bullish_momentum_breakout":   bullMomFamily,
			"bearish_continuation":        bearContFamily,
			"bearish_momentum_breakdown":  bearMomFamily,
			"bearish_exhaustion_reversal": bearishExhaustionFamily,
		},
		PatternScoreConfig: PatternScoreConfig{
			Bullish: map[string]int{
				"bull_flag": 3, "tight_base": 3, "flat_base": 2,
				"higher_low_continuation": 2, "volatility_contraction_breakout": 3,
				"relative_strength_bullish": 2,
			},
			Bearish: map[string]int{
				"bear_flag": 3, "lower_high_breakdown": 2,
				"volatility_contraction_breakdown": 3, "relative_weakness_bearish": 2,
				"overextension_exhaustion": 3, "rejection_wick_reversal": 3,
			},
		},
		AntiPatternConfig: AntiPatternConfig{
			BullishReject: []string{"late_stage_extension", "distribution_severe"},
		},
		EventBlocks: EventBlocksConfig{
			EarningsBlackoutDays: 5, BinaryEventBlackoutDays: 3,
			IfBlockedStatus: "blocked_by_event",
		},
		StateRules: StateRulesConfig{
			StructuralCandidateIsWatchlistOnly: true,
			EntryReadyCanSurfacePreopen:        true,
			ConfirmedRequiredForTradeOutput:    true,
			BlockedByEventOverridesEntryReady:  true,
			BlockedByEventOverridesConfirmed:   true,
		},
		OptionsTranslation: OptionsTranslationConfig{
			LiquidityFilters: LiquidityFilters{
				MinOpenInterest: 100, MinOptionVolume: 10, MaxBidAskSpreadPctOfMid: 10.0,
			},
			HideOptionsForStatuses: []string{
				"rejected", "structural_candidate", "entry_ready", "watch_only", "blocked_by_event",
			},
		},
		OpenConfirmation: OpenConfirmationConfig{
			Enabled: true, ConfirmationWindowMinutes: 10, MinTrueSignalsToConfirm: 3,
			Checks: ConfirmationChecks{
				BreakoutOrReclaimHolds: true, OpeningRangeCloseAboveMidpointForCalls: true,
				OpeningRangeCloseBelowMidpointForPuts: true, NoRejectionWickForCalls: true,
				NoReversalTailForPuts: true, OpeningVolumeSupport: true, MarketOpenAlignment: true,
			},
			AutoReject: AutoRejectChecks{
				DecisiveLevelLoss: true, WeakFirst10mClose: true, HardOpenReversal: true,
				BroadMarketRiskoffShock: true, DownsideRejectionVolume: true,
			},
		},
		Scoring: ScoringConfig{
			ShowScoreOnlyFor: []string{"entry_ready", "confirmed"},
			HideScoreFor:     []string{"rejected", "structural_candidate", "watch_only", "blocked_by_event"},
		},
		DailyOutput: DailyOutputConfig{
			AllowZeroTradeDay: true, NoTradeDayWhenNoConfirmed: true,
			NoTradeDayMessage: "No symbols reached confirmed status. Watchlist candidates may still exist.",
		},
		TradeFrequency: TradeFrequencyConfig{
			Mode:                   "active_paper",
			MaxEntryReadyToConfirm: 5,
			MaxNewPositionsPerDay:  3,
			MinEntryReadyScore:     68,
			MinClaudeConfidence:    0.65,
			AllowZeroTradeDay:      true,
		},
		ClaudeConfirmation: ClaudeConfirmationConfig{
			Enabled:                            true,
			MinConfidence:                      0.65,
			MaxCandidatesPerRun:                5,
			UseDeterministicOpeningEvidence:    true,
			DeterministicSignalsSoftMin:        2,
			DeterministicAutoRejectIsHardBlock: true,
		},
		Risk: RiskConfig{
			OptionLifecycle: OptionLifecycleConfig{
				DTEMin: 7, DTEMax: 14, TargetDTE: 10,
				AvoidDTEBelow: 4, ContractsPerTrade: 1,
			},
			MechanicalExits: MechanicalExitsConfig{
				Enabled:                         true,
				PremiumStopLossPct:              30,
				PremiumTakeProfitPct:            50,
				PremiumTrailingStartPct:         35,
				PremiumTrailingGivebackPct:      20,
				ForceEODExitUnlessHoldConfirmed: true,
				MaxHoldDaysWithoutReconfirm:     1,
			},
		},
	}
}
