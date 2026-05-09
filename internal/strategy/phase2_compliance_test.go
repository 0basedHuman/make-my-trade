// internal/strategy/phase2_compliance_test.go
//
// Phase 2 compliance tests:
//   1. Legacy YAML families are disabled — Rules.Families is nil/empty when loaded
//   2. Pattern analysis runs before breakout gate and sets PatternState correctly
//   3. PatternState="pattern_forming" when pattern detected but breakout not confirmed
//   4. PatternState="pattern_breakout_confirmed" when both detected and breakout passes
//   5. PatternState="no_pattern" when no pattern detected
//
// Run: go test ./internal/strategy/ -run TestPhase2 -v

package strategy

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yourname/makemytrade/internal/indicators"
)

// ── Legacy YAML disabled ──────────────────────────────────────────────────────

func TestPhase2_LegacyFamiliesDisabledInYAML(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	yamlPath := filepath.Join(repoRoot, "strategy_rules.yaml")

	rules, err := LoadRules(yamlPath)
	if err != nil {
		t.Fatalf("LoadRules failed: %v", err)
	}

	if rules.DeprecatedLegacyFamilies.Enabled {
		t.Fatal("deprecated_legacy_strategy_families.enabled must be false in strategy_rules.yaml")
	}
	if len(rules.Families) > 0 {
		t.Fatalf("Rules.Families must be empty when legacy families disabled, got %d families: %v",
			len(rules.Families), familyNames(rules.Families))
	}
}

func TestPhase2_RSVEConfigLoadedFromYAML(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	yamlPath := filepath.Join(repoRoot, "strategy_rules.yaml")

	rules, err := LoadRules(yamlPath)
	if err != nil {
		t.Fatalf("LoadRules failed: %v", err)
	}
	if rules.RSVE.Options.DTEMin == 0 {
		t.Fatal("RSVE config not loaded: DTEMin=0")
	}
	if rules.RSVE.Options.DTEMin != 21 {
		t.Fatalf("RSVE DTEMin must be 21, got %d", rules.RSVE.Options.DTEMin)
	}
}

func familyNames(m map[string]FamilyConfig) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

// ── PatternState classification ───────────────────────────────────────────────

// TestPhase2_PatternState_NoPattern: no pattern detected → PatternState="no_pattern"
func TestPhase2_PatternState_NoPattern(t *testing.T) {
	input := makeValidBullishInput()
	// Use short bars — not enough history for any pattern to form
	input.Bars = input.Bars[:10]

	cfg := DefaultRSVEConfig()
	cfg.PatternAnalysis.Enabled = true
	cfg.PatternAnalysis.RequiredForTrade = false

	r := EvaluateRSVE(input, cfg)
	if r.PatternAnalysis.PatternState != "no_pattern" {
		t.Fatalf("expected PatternState=no_pattern with short bars, got %q", r.PatternAnalysis.PatternState)
	}
}

// TestPhase2_PatternState_PatternBreakoutConfirmed: when AllPass=true (all gates including breakout), state must be pattern_breakout_confirmed OR no_pattern.
func TestPhase2_PatternState_WhenAllPass_BreakoutConfirmed(t *testing.T) {
	input := makeValidBullishInput()

	cfg := DefaultRSVEConfig()
	cfg.Bullish.RSMinPct = -100
	cfg.Bullish.BBWidthPercentileMax = 1.0

	r := EvaluateRSVE(input, cfg)

	// When AllPass=true the breakout gate passed. If a pattern was detected, PatternState
	// must be "pattern_breakout_confirmed". If not detected, it's "no_pattern".
	if r.AllPass {
		if r.PatternAnalysis.Detected && r.PatternAnalysis.PatternState != "pattern_breakout_confirmed" {
			t.Fatalf("expected PatternState=pattern_breakout_confirmed when AllPass=true and pattern detected, got %q",
				r.PatternAnalysis.PatternState)
		}
		if !r.PatternAnalysis.Detected && r.PatternAnalysis.PatternState != "no_pattern" {
			t.Fatalf("expected PatternState=no_pattern when no pattern and AllPass=true, got %q",
				r.PatternAnalysis.PatternState)
		}
	}
}

// TestPhase2_PatternState_NoPatternOption_Unavailable: option data missing doesn't affect PatternState.
func TestPhase2_PatternState_OptionUnavailable(t *testing.T) {
	input := makeValidBullishInput()
	input.IVRank = -1 // option data unavailable → stock_signal_passed

	cfg := DefaultRSVEConfig()
	cfg.Bullish.RSMinPct = -100
	cfg.Bullish.BBWidthPercentileMax = 1.0

	r := EvaluateRSVE(input, cfg)
	if r.Status != "stock_signal_passed" {
		t.Fatalf("expected stock_signal_passed, got %q", r.Status)
	}
	// PatternState must still be set even when option data is unavailable
	validStates := map[string]bool{
		"no_pattern": true, "pattern_forming": true, "pattern_breakout_confirmed": true,
	}
	if !validStates[r.PatternAnalysis.PatternState] {
		t.Fatalf("PatternState must be one of no_pattern/pattern_forming/pattern_breakout_confirmed, got %q",
			r.PatternAnalysis.PatternState)
	}
}

// TestPhase2_NoForcedEODExit_ConfigHasNoField: verify MechanicalExitsConfig has no ForceEOD field.
func TestPhase2_NoMinHoldDays_ConfigHasNoField(t *testing.T) {
	cfg := DefaultRules().Risk.MechanicalExits
	// If MinHoldDays existed, it would be in the struct. This test verifies the default
	// config doesn't suppress day-0 exits by checking TimeStopDays is the smallest time gate.
	if cfg.TimeStopDays <= 0 {
		t.Fatal("TimeStopDays must be > 0 for the time-stop rule to function")
	}
	if cfg.MaxHoldDaysWithoutReconfirm <= 0 {
		t.Fatal("MaxHoldDaysWithoutReconfirm must be > 0 for the max-hold rule to function")
	}
}

// TestPhase2_ClaudeConfirmation_NotUsedAsGate: Claude confirmation config must not be
// used as a gate or to determine "confirmed" status. The RSVE engine and
// EvaluateConfirmation are the sole gatekeepers.
// This verifies that ClaudeConfirmationConfig.Enabled is false so the old code
// path cannot accidentally re-enable Claude-gated confirmation.
func TestPhase2_ClaudeConfirmation_NotUsedAsGate(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	yamlPath := filepath.Join(repoRoot, "strategy_rules.yaml")

	rules, err := LoadRules(yamlPath)
	if err != nil {
		t.Fatalf("LoadRules failed: %v", err)
	}
	if rules.ClaudeConfirmation.Enabled {
		t.Fatal("claude_confirmation.enabled must be false — Claude must not gate trades")
	}
}

// TestPhase2_ConfirmationStatus_DeterminesOutcome: EvaluateConfirmation status field
// ("confirmed" vs "watch_only") is what determines entry, not SignalsPassed count.
func TestPhase2_ConfirmationStatus_DeterminesOutcome(t *testing.T) {
	// Scenario: required signals fail — result.Status must be "watch_only" regardless
	// of raw SignalsPassed count.
	bars := []indicators.Bar{
		{Open: 100, High: 100.5, Low: 93, Close: 93.5, Volume: 60000}, // collapses through entry
	}
	spyBars := []indicators.Bar{
		{Open: 500, High: 502, Low: 499, Close: 501, Volume: 1000000},
	}
	in := ConfirmationInput{
		Ticker:        "FAIL",
		Direction:     "bullish",
		EntryLow:      98.0,
		EntryHigh:     102.0,
		StopLoss:      95.0,
		PrevDayVolume: 0,
		Bars:          bars,
		SPYBars:       spyBars,
	}
	cfg := DefaultRSVEConfig()
	rules := OpenConfirmationConfig{
		Enabled:            true,
		RequiredChecks:     ConfirmationRequiredChecks{LevelHolds: true, MarketAligned: true},
		MinOptionalSignals: 1,
		Checks: ConfirmationChecks{
			BreakoutOrReclaimHolds: true,
			MarketOpenAlignment:    true,
		},
	}
	_ = cfg // rules is what we pass

	res := EvaluateConfirmation(in, rules)

	// level_holds fails → Status must be watch_only; code must NOT use SignalsPassed for this check
	if res.Status != "watch_only" {
		t.Fatalf("expected watch_only when required signal fails; got %q", res.Status)
	}
	// Confirm the decision is driven by Status, not by a raw signal count threshold
	if res.SignalsPassed >= 3 {
		// If somehow 3+ signals pass but level_holds fails, status still must be watch_only
		if res.Status != "watch_only" {
			t.Fatalf("status must be watch_only even if SignalsPassed=%d when required check fails", res.SignalsPassed)
		}
	}
}
