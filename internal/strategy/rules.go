// internal/strategy/rules.go
//
// WHAT: Go struct hierarchy that mirrors strategy_rules.yaml, plus a loader.
//
// WHY:  All strategy thresholds, family definitions, pattern scores, and
//       liquidity filters live in strategy_rules.yaml as the single source of
//       truth. This file parses that YAML at startup so the engine and handler
//       can reference named fields rather than magic constants.
//
// HOW:  LoadRules("strategy_rules.yaml") is called once at startup and the
//       resulting *Rules is passed to strategy.NewEngine() and used in the
//       handler to build filterChainQuality thresholds.
//
// WHAT BREAKS: If strategy_rules.yaml has a key that doesn't map to a struct
//              field, yaml.v3 silently ignores it (no error). If a required
//              numeric field is absent, it defaults to zero — which would
//              disable the check. Always verify thresholds after editing YAML.
//
// VERIFY:  fmt.Printf("%+v\n", rules.MarketRegime.HardRules)
//          should print {VIXMax:24 BTCRoc20Min:0}

package strategy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Rules is the decoded form of strategy_rules.yaml.
// All mutable strategy parameters come from here — never from hardcoded constants.
type Rules struct {
	Version     int    `yaml:"version"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	MarketRegime        MarketRegimeConfig        `yaml:"market_regime"`
	HardQualifiers      HardQualifiersConfig      `yaml:"hard_qualifiers"`
	SetupFamilies       map[string]SetupFamilyConfig `yaml:"setup_families"`
	PatternScoreConfig  PatternScoreConfig        `yaml:"pattern_scores"`
	AntiPatternConfig   AntiPatternConfig         `yaml:"anti_patterns"`
	TargetModelConfig   TargetModelConfig         `yaml:"target_model"`
	OptionsTranslation  OptionsTranslationConfig  `yaml:"options_translation"`
	ReasonCodes         []string                  `yaml:"reason_codes"`

	// v3: lifecycle + classification + UI config
	StateRules          StateRulesConfig          `yaml:"state_rules"`
	EventBlocks         EventBlocksConfig         `yaml:"event_blocks"`
	ClassificationLogic ClassificationLogicConfig `yaml:"classification_logic"`
	TargetOutput        TargetOutputConfig        `yaml:"target_output"`
	Scoring             ScoringConfig             `yaml:"scoring"`
	UIRules             UIRulesConfig             `yaml:"ui_rules"`
	DailyOutput         DailyOutputConfig         `yaml:"daily_output"`
	OpenConfirmation    OpenConfirmationConfig    `yaml:"open_confirmation"`
}

// ── Market regime ─────────────────────────────────────────────────────────────

type MarketRegimeConfig struct {
	HardRules MarketRegimeHardRules `yaml:"hard_rules"`
}

// MarketRegimeHardRules contains the absolute regime gates.
// VIXMax: if VIX >= this, all new entries are blocked.
// BTCRoc20Min: if BTC 20d ROC < this, bullish setups are blocked.
type MarketRegimeHardRules struct {
	VIXMax      float64 `yaml:"vix_max"`
	BTCRoc20Min float64 `yaml:"btc_roc20_min"`
}

// ── Hard qualifiers ───────────────────────────────────────────────────────────

type HardQualifiersConfig struct {
	Common HardQualifiersCommon `yaml:"common"`
}

type HardQualifiersCommon struct {
	VolumeRatioMin          float64 `yaml:"volume_ratio_min"`
	RewardRiskMin           float64 `yaml:"reward_risk_min"`
	EntryExtensionMaxPct    float64 `yaml:"entry_extension_max_pct"`
	EarningsBlackoutDays    int     `yaml:"earnings_blackout_days"`
	BinaryEventBlackoutDays int     `yaml:"binary_event_blackout_days"`
}

// ── Setup families ────────────────────────────────────────────────────────────

// SetupFamilyConfig mirrors one entry under setup_families in the YAML.
// StructuralRules uses map[string]interface{} because the YAML values are
// heterogeneous (bool, float, etc.) and names vary per family.
type SetupFamilyConfig struct {
	Direction            string                 `yaml:"direction"`
	OptionType           string                 `yaml:"option_type"`
	Description          string                 `yaml:"description"`
	StructuralRules      map[string]interface{} `yaml:"structural_rules"`
	EntryRules           FamilyEntryRules       `yaml:"entry_rules"`
	PatternScoreMin      int                    `yaml:"pattern_score_min"`
	PreferredOptionStyle string                 `yaml:"preferred_option_style"`
}

// FamilyEntryRules holds the per-family entry filter thresholds.
type FamilyEntryRules struct {
	VolumeRatioMin        float64 `yaml:"volume_ratio_min"`
	StrongVolumeExpansion bool    `yaml:"strong_volume_expansion"`
	RSIMin                float64 `yaml:"rsi_min"`
	RSIMax                float64 `yaml:"rsi_max"`
	RewardRiskMin         float64 `yaml:"reward_risk_min"`
	EntryExtensionMaxPct  float64 `yaml:"entry_extension_max_pct"`
}

// ── Pattern scores ────────────────────────────────────────────────────────────

// PatternScoreConfig maps pattern names → integer point values.
// Used by the engine to sum a candidate's integer pattern score.
type PatternScoreConfig struct {
	Bullish map[string]int `yaml:"bullish"`
	Bearish map[string]int `yaml:"bearish"`
}

// ── Anti-patterns ─────────────────────────────────────────────────────────────

type AntiPatternConfig struct {
	BullishReject []string `yaml:"bullish_reject"`
	BearishReject []string `yaml:"bearish_reject"`
}

// ── Target model ──────────────────────────────────────────────────────────────

type TargetModelConfig struct {
	UseArbitraryPercentTargets bool              `yaml:"use_arbitrary_percent_targets"`
	BullishContinuation        FamilyTargetModel `yaml:"bullish_continuation"`
	BullishMomentumBreakout    FamilyTargetModel `yaml:"bullish_momentum_breakout"`
	BearishContinuation        FamilyTargetModel `yaml:"bearish_continuation"`
	BearishMomentumBreakdown   FamilyTargetModel `yaml:"bearish_momentum_breakdown"`
}

type FamilyTargetModel struct {
	BaseTargetSources    []string   `yaml:"base_target_sources"`
	StretchTargetSources []string   `yaml:"stretch_target_sources"`
	HoldWindowDays       HoldWindow `yaml:"hold_window_days"`
}

// HoldWindow holds the hold duration range for a family (in calendar days).
type HoldWindow struct {
	Min  int `yaml:"min"`
	Base int `yaml:"base"`
	Max  int `yaml:"max"`
}

// ── Options translation ───────────────────────────────────────────────────────

type OptionsTranslationConfig struct {
	Enabled               bool                         `yaml:"enabled"`
	BySetupFamily         map[string]OptionsFamilySpec `yaml:"by_setup_family"`
	LiquidityFilters      LiquidityFilters             `yaml:"liquidity_filters"`
	HideOptionsForStatuses []string                    `yaml:"hide_options_for_statuses"`
}

// OptionsFamilySpec holds preferred DTE and delta for one setup family.
type OptionsFamilySpec struct {
	PreferredDTEMin     int      `yaml:"preferred_dte_min"`
	PreferredDTEMax     int      `yaml:"preferred_dte_max"`
	PreferredDeltaMin   float64  `yaml:"preferred_delta_min"`
	PreferredDeltaMax   float64  `yaml:"preferred_delta_max"`
	PreferredStructures []string `yaml:"preferred_structures"`
}

// LiquidityFilters are the app-side chain quality thresholds.
// Contracts that fail these are stripped before being sent to Claude.
type LiquidityFilters struct {
	MinOpenInterest         int     `yaml:"min_open_interest"`
	MinOptionVolume         int     `yaml:"min_option_volume"`
	MaxBidAskSpreadPctOfMid float64 `yaml:"max_bid_ask_spread_pct_of_mid"`
}

// ── v3: Lifecycle state, event blocks, classification, scoring, UI ────────────

// StateRulesConfig maps to state_rules in strategy_rules.yaml.
// These flags drive the lifecycle state machine — status promotion and demotion
// rules are read from here, not hardcoded in the engine.
type StateRulesConfig struct {
	StructuralCandidateIsWatchlistOnly         bool `yaml:"structural_candidate_is_watchlist_only"`
	EntryReadyCanSurfacePreopen                bool `yaml:"entry_ready_can_surface_preopen"`
	ConfirmedRequiredForTradeOutput            bool `yaml:"confirmed_required_for_trade_output"`
	ConfirmedRequiredForOptionsEvaluation      bool `yaml:"confirmed_required_for_options_evaluation"`
	HideOptionOutputUntilUnderlyingConfirmed   bool `yaml:"hide_option_output_until_underlying_confirmed"`
	BlockedByEventOverridesEntryReady          bool `yaml:"blocked_by_event_overrides_entry_ready"`
	BlockedByEventOverridesConfirmed           bool `yaml:"blocked_by_event_overrides_confirmed"`
}

// EventBlocksConfig maps to event_blocks in strategy_rules.yaml.
// IfBlockedStatus is the status to assign when a symbol is blocked by an event.
type EventBlocksConfig struct {
	EarningsBlackoutDays    int    `yaml:"earnings_blackout_days"`
	BinaryEventBlackoutDays int    `yaml:"binary_event_blackout_days"`
	IfBlockedStatus         string `yaml:"if_blocked_status"` // "blocked_by_event"
}

// ClassificationLogicConfig maps to classification_logic in strategy_rules.yaml.
// These lists are informational — the engine still drives classification but
// they document the criteria so Claude and the UI match.
type ClassificationLogicConfig struct {
	RejectedIfAny             []string `yaml:"rejected_if_any"`
	StructuralCandidateIfAll  []string `yaml:"structural_candidate_if_all"`
	RemainStructuralIfAny     []string `yaml:"remain_structural_candidate_if_any"`
	EntryReadyIfAll           []string `yaml:"entry_ready_if_all"`
	ConfirmedIfAll            []string `yaml:"confirmed_if_all"`
}

// TargetOutputConfig maps to target_output in strategy_rules.yaml.
// Controls which statuses receive price targets in the output.
type TargetOutputConfig struct {
	EmitTargetsFor                      []string `yaml:"emit_targets_for"`
	MarkStructuralTargetsAsWatchlistOnly bool     `yaml:"mark_structural_targets_as_watchlist_only"`
	LabelTargets                        struct {
		Base    string `yaml:"base"`
		Stretch string `yaml:"stretch"`
	} `yaml:"label_targets"`
	DoNotTreatStretchAsBaseCase bool `yaml:"do_not_treat_stretch_as_base_case"`
}

// ScoringConfig maps to scoring in strategy_rules.yaml.
// Controls which statuses expose a numeric score in the UI.
type ScoringConfig struct {
	ShowScoreOnlyFor []string `yaml:"show_score_only_for"`
	HideScoreFor     []string `yaml:"hide_score_for"`
}

// UIStatusRule is one status-section entry under ui_rules.
type UIStatusRule struct {
	Label                   string `yaml:"label"`
	ShowTradeButton         bool   `yaml:"show_trade_button"`
	ShowOptionOutput        bool   `yaml:"show_option_output"`
	ShowReasonWhatIsMissing bool   `yaml:"show_reason_what_is_missing"`
}

// UIRulesConfig maps to ui_rules in strategy_rules.yaml.
type UIRulesConfig struct {
	Sections            []string     `yaml:"sections"`
	StructuralCandidate UIStatusRule `yaml:"structural_candidate"`
	BlockedByEvent      UIStatusRule `yaml:"blocked_by_event"`
	EntryReady          UIStatusRule `yaml:"entry_ready"`
	Confirmed           UIStatusRule `yaml:"confirmed"`
}

// DailyOutputConfig maps to daily_output in strategy_rules.yaml.
type DailyOutputConfig struct {
	AllowZeroTradeDay        bool     `yaml:"allow_zero_trade_day"`
	RequireReasonCodes       bool     `yaml:"require_reason_codes"`
	Sections                 []string `yaml:"sections"`
	NoTradeDayWhenNoConfirmed bool    `yaml:"no_trade_day_when_no_confirmed"`
	NoTradeDayMessage        string   `yaml:"no_trade_day_message"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// FamilyTargetFor returns the target model for the named setup family.
func (r *Rules) FamilyTargetFor(family string) (FamilyTargetModel, bool) {
	switch family {
	case "bullish_continuation":
		return r.TargetModelConfig.BullishContinuation, true
	case "bullish_momentum_breakout":
		return r.TargetModelConfig.BullishMomentumBreakout, true
	case "bearish_continuation":
		return r.TargetModelConfig.BearishContinuation, true
	case "bearish_momentum_breakdown":
		return r.TargetModelConfig.BearishMomentumBreakdown, true
	}
	return FamilyTargetModel{}, false
}

// OptionsFamilySpecFor returns the options translation spec for the named family.
func (r *Rules) OptionsFamilySpecFor(family string) (OptionsFamilySpec, bool) {
	spec, ok := r.OptionsTranslation.BySetupFamily[family]
	return spec, ok
}

// ShouldHideScore returns true if a candidate with the given status must not show a score
// in the UI, per scoring.hide_score_for in strategy_rules.yaml.
func (r *Rules) ShouldHideScore(status string) bool {
	for _, s := range r.Scoring.HideScoreFor {
		if s == status {
			return true
		}
	}
	return false
}

// ShouldHideOptions returns true if options contract output must be suppressed for the
// given candidate status, per options_translation.hide_options_for_statuses.
func (r *Rules) ShouldHideOptions(status string) bool {
	for _, s := range r.OptionsTranslation.HideOptionsForStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// BlockedByEventStatus returns the status to assign when an event blackout fires.
// Defaults to "blocked_by_event" if the YAML field is unset.
func (r *Rules) BlockedByEventStatus() string {
	if r.EventBlocks.IfBlockedStatus != "" {
		return r.EventBlocks.IfBlockedStatus
	}
	return "blocked_by_event"
}

// NoTradeDayMessage returns the configured no-trade message, falling back to a default.
func (r *Rules) NoTradeDayMessage() string {
	if r.DailyOutput.NoTradeDayMessage != "" {
		return r.DailyOutput.NoTradeDayMessage
	}
	return "No symbols reached confirmed status. Watchlist candidates may still exist."
}

// ── Open confirmation ─────────────────────────────────────────────────────────

// OpenConfirmationConfig maps to open_confirmation in strategy_rules.yaml.
// Controls how the first-10-minute opening window promotes entry_ready → confirmed.
type OpenConfirmationConfig struct {
	Enabled                   bool                 `yaml:"enabled"`
	ConfirmationWindowMinutes int                  `yaml:"confirmation_window_minutes"`
	MinTrueSignalsToConfirm   int                  `yaml:"min_true_signals_to_confirm"`
	Checks                    ConfirmationChecks   `yaml:"checks"`
	AutoReject                AutoRejectChecks     `yaml:"auto_reject"`
}

// ConfirmationChecks mirrors the checks sub-block.
// Each bool records whether that check is active in the YAML.
// The confirmation evaluator reads these to decide which signals count.
type ConfirmationChecks struct {
	BreakoutOrReclaimHolds                  bool `yaml:"breakout_or_reclaim_holds"`
	OpeningRangeCloseAboveMidpointForCalls  bool `yaml:"opening_range_close_above_midpoint_for_calls"`
	OpeningRangeCloseBelowMidpointForPuts   bool `yaml:"opening_range_close_below_midpoint_for_puts"`
	NoRejectionWickForCalls                 bool `yaml:"no_rejection_wick_for_calls"`
	NoReversalTailForPuts                   bool `yaml:"no_reversal_tail_for_puts"`
	OpeningVolumeSupport                    bool `yaml:"opening_volume_support"`
	MarketOpenAlignment                     bool `yaml:"market_open_alignment"`
}

// AutoRejectChecks mirrors the auto_reject sub-block.
type AutoRejectChecks struct {
	DecisiveLevelLoss        bool `yaml:"decisive_level_loss"`
	WeakFirst10mClose        bool `yaml:"weak_first_10m_close"`
	HardOpenReversal         bool `yaml:"hard_open_reversal"`
	BroadMarketRiskoffShock  bool `yaml:"broad_market_riskoff_shock"`
	DownsideRejectionVolume  bool `yaml:"downside_rejection_volume"`
}

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

// DefaultRules returns safe fallback values used when YAML cannot be loaded.
// All thresholds match the hard values in strategy_rules.yaml v3.
func DefaultRules() *Rules {
	return &Rules{
		MarketRegime: MarketRegimeConfig{
			HardRules: MarketRegimeHardRules{VIXMax: 24.0, BTCRoc20Min: 0.0},
		},
		HardQualifiers: HardQualifiersConfig{
			Common: HardQualifiersCommon{
				VolumeRatioMin:          1.2,
				RewardRiskMin:           2.0,
				EntryExtensionMaxPct:    6.0,
				EarningsBlackoutDays:    5,
				BinaryEventBlackoutDays: 3,
			},
		},
		OptionsTranslation: OptionsTranslationConfig{
			LiquidityFilters: LiquidityFilters{
				MinOpenInterest:         500,
				MinOptionVolume:         100,
				MaxBidAskSpreadPctOfMid: 5.0,
			},
			HideOptionsForStatuses: []string{
				"rejected", "structural_candidate", "entry_ready", "watch_only", "blocked_by_event",
			},
		},
		StateRules: StateRulesConfig{
			StructuralCandidateIsWatchlistOnly:       true,
			EntryReadyCanSurfacePreopen:              true,
			ConfirmedRequiredForTradeOutput:          true,
			ConfirmedRequiredForOptionsEvaluation:    true,
			HideOptionOutputUntilUnderlyingConfirmed: true,
			BlockedByEventOverridesEntryReady:        true,
			BlockedByEventOverridesConfirmed:         true,
		},
		EventBlocks: EventBlocksConfig{
			EarningsBlackoutDays:    5,
			BinaryEventBlackoutDays: 3,
			IfBlockedStatus:         "blocked_by_event",
		},
		Scoring: ScoringConfig{
			ShowScoreOnlyFor: []string{"entry_ready", "confirmed", "options_ready", "options_confirmed"},
			HideScoreFor:     []string{"rejected", "structural_candidate", "watch_only", "blocked_by_event"},
		},
		UIRules: UIRulesConfig{
			Sections: []string{
				"confirmed", "entry_ready", "structural_candidate",
				"blocked_by_event", "watch_only", "no_trade_today_summary",
			},
			StructuralCandidate: UIStatusRule{Label: "WATCHLIST", ShowReasonWhatIsMissing: true},
			BlockedByEvent:      UIStatusRule{Label: "BLOCKED BY EVENT", ShowReasonWhatIsMissing: true},
			EntryReady:          UIStatusRule{Label: "PRE-OPEN CANDIDATE", ShowReasonWhatIsMissing: true},
			Confirmed:           UIStatusRule{Label: "CONFIRMED", ShowTradeButton: true, ShowOptionOutput: true},
		},
		DailyOutput: DailyOutputConfig{
			AllowZeroTradeDay:         true,
			NoTradeDayWhenNoConfirmed: true,
			NoTradeDayMessage:         "No symbols reached confirmed status. Watchlist candidates may still exist.",
		},
		OpenConfirmation: OpenConfirmationConfig{
			Enabled:                   true,
			ConfirmationWindowMinutes: 10,
			MinTrueSignalsToConfirm:   3,
			Checks: ConfirmationChecks{
				BreakoutOrReclaimHolds:                 true,
				OpeningRangeCloseAboveMidpointForCalls: true,
				OpeningRangeCloseBelowMidpointForPuts:  true,
				NoRejectionWickForCalls:                true,
				NoReversalTailForPuts:                  true,
				OpeningVolumeSupport:                   true,
				MarketOpenAlignment:                    true,
			},
			AutoReject: AutoRejectChecks{
				DecisiveLevelLoss:       true,
				WeakFirst10mClose:       true,
				HardOpenReversal:        true,
				BroadMarketRiskoffShock: true,
				DownsideRejectionVolume: true,
			},
		},
	}
}
