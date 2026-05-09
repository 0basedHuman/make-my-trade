// internal/strategy/confirmation_test.go
//
// Tests for the tightened opening confirmation logic:
//   - level_holds (SignalLevelHolds) = REQUIRED
//   - market_aligned (SignalMarketOK) = REQUIRED
//   - at least 1 of: volume, no_rejection_wick, opening_range_midpoint = OPTIONAL
//
// Run: go test ./internal/strategy/ -run TestConfirmation -v

package strategy

import (
	"testing"

	"github.com/yourname/makemytrade/internal/indicators"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// confirmCfg returns an OpenConfirmationConfig with required+optional mode enabled.
func confirmCfg() OpenConfirmationConfig {
	return OpenConfirmationConfig{
		Enabled:            true,
		RequiredChecks:     ConfirmationRequiredChecks{LevelHolds: true, MarketAligned: true},
		MinOptionalSignals: 1,
		Checks: ConfirmationChecks{
			BreakoutOrReclaimHolds:                 true,
			OpeningRangeCloseAboveMidpointForCalls: true,
			OpeningRangeCloseBelowMidpointForPuts:  true,
			NoRejectionWickForCalls:                true,
			NoReversalTailForPuts:                  true,
			OpeningVolumeSupport:                   true,
			MarketOpenAlignment:                    true,
		},
		AutoReject: AutoRejectChecks{},
	}
}

// strongBullishInput returns a ConfirmationInput where all signals pass.
// Bars: price holding well above entry zone, volume elevated, clean open.
// SPY: positive on the day.
func strongBullishInput() ConfirmationInput {
	bars := []indicators.Bar{
		{Open: 100, High: 103, Low: 99.5, Close: 102, Volume: 80000}, // strong first bar, no upper wick relative to body
		{Open: 102, High: 104, Low: 101, Close: 103, Volume: 70000},
		{Open: 103, High: 105, Low: 102, Close: 104, Volume: 75000},
	}
	spyBars := []indicators.Bar{
		{Open: 500, High: 502, Low: 499, Close: 501, Volume: 1000000}, // SPY up
	}
	return ConfirmationInput{
		Ticker:        "TEST",
		Direction:     "bullish",
		EntryLow:      98.0,
		EntryHigh:     100.0,
		StopLoss:      95.0,
		PrevDayVolume: 1000000,
		Bars:          bars,
		SPYBars:       spyBars,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestConfirmation_AllRequired_OneOptional_Confirmed: both required pass + 1 optional → confirmed
func TestConfirmation_AllRequired_OneOptional_Confirmed(t *testing.T) {
	in := strongBullishInput()
	cfg := confirmCfg()
	res := EvaluateConfirmation(in, cfg)

	if res.AutoRejected {
		t.Fatalf("unexpected auto-reject: %s", res.AutoRejectReason)
	}
	if !res.SignalLevelHolds {
		t.Fatalf("SignalLevelHolds should be true for strong bullish input")
	}
	if !res.SignalMarketOK {
		t.Fatalf("SignalMarketOK should be true when SPY is up")
	}
	if !res.Diagnostics.RequiredChecksPassed {
		t.Fatalf("Diagnostics.RequiredChecksPassed should be true, got false (failed: %v)", res.Diagnostics.FailedRequired)
	}
	if res.Diagnostics.OptionalChecksPassed < 1 {
		t.Fatalf("expected >= 1 optional signals, got %d", res.Diagnostics.OptionalChecksPassed)
	}
	if res.Status != "confirmed" {
		t.Fatalf("expected status=confirmed, got %q", res.Status)
	}
}

// TestConfirmation_RequiredFails_LevelHolds_WatchOnly: level_holds fails → watch_only
func TestConfirmation_RequiredFails_LevelHolds_WatchOnly(t *testing.T) {
	// Price closes well below entry zone midpoint → level_holds fails
	bars := []indicators.Bar{
		{Open: 100, High: 100.5, Low: 94, Close: 94.5, Volume: 60000}, // collapses through entry
		{Open: 94.5, High: 95, Low: 93, Close: 93.5, Volume: 55000},
	}
	spyBars := []indicators.Bar{
		{Open: 500, High: 502, Low: 499, Close: 501, Volume: 1000000}, // SPY still up
	}
	in := ConfirmationInput{
		Ticker:        "FAIL",
		Direction:     "bullish",
		EntryLow:      98.0,
		EntryHigh:     102.0, // midpoint = 100; close at 93.5 → far below
		StopLoss:      95.0,
		PrevDayVolume: 0,
		Bars:          bars,
		SPYBars:       spyBars,
	}
	cfg := confirmCfg()
	res := EvaluateConfirmation(in, cfg)

	if res.Status != "watch_only" {
		t.Fatalf("expected watch_only when level_holds fails, got %q", res.Status)
	}
	if res.Diagnostics.RequiredChecksPassed {
		t.Fatalf("RequiredChecksPassed should be false when level_holds fails")
	}
	found := false
	for _, f := range res.Diagnostics.FailedRequired {
		if f == "level_holds" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'level_holds' in FailedRequired, got %v", res.Diagnostics.FailedRequired)
	}
}

// TestConfirmation_RequiredFails_MarketAligned_WatchOnly: market_aligned fails → watch_only
func TestConfirmation_RequiredFails_MarketAligned_WatchOnly(t *testing.T) {
	in := strongBullishInput()
	// Override SPY to be down — market_aligned will fail
	in.SPYBars = []indicators.Bar{
		{Open: 500, High: 500, Low: 495, Close: 496, Volume: 1000000}, // SPY down
	}
	cfg := confirmCfg()
	res := EvaluateConfirmation(in, cfg)

	if res.Status != "watch_only" {
		t.Fatalf("expected watch_only when market_aligned fails, got %q", res.Status)
	}
	found := false
	for _, f := range res.Diagnostics.FailedRequired {
		if f == "market_aligned" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'market_aligned' in FailedRequired, got %v", res.Diagnostics.FailedRequired)
	}
}

// TestConfirmation_BothRequired_NoOptionals_WatchOnly: both required pass but 0 optionals → watch_only
func TestConfirmation_BothRequired_NoOptionals_WatchOnly(t *testing.T) {
	// Build a scenario where level_holds and market_aligned pass but no optional signal passes:
	//   - No volume support (small bars, PrevDayVolume set to make threshold huge)
	//   - Upper wick exactly = body size (SignalNoRejection fails)
	//   - Close exactly at range midpoint (not > midpoint for SignalOpenRange calls check)
	bars := []indicators.Bar{
		// body = |100-100.5| = 0.5, upper wick = 103 - 100.5 = 2.5 (> 0.5*0.5 body = 0.25) → wick fails
		{Open: 100, High: 103, Low: 99, Close: 100.5, Volume: 100}, // tiny volume
	}
	spyBars := []indicators.Bar{
		{Open: 500, High: 502, Low: 499, Close: 501, Volume: 1000000}, // SPY up → market_aligned passes
	}
	in := ConfirmationInput{
		Ticker:        "NOOPT",
		Direction:     "bullish",
		EntryLow:      99.0,
		EntryHigh:     101.0, // entryMid=100; last close=100.5 >= 100 → level_holds passes
		StopLoss:      95.0,
		PrevDayVolume: 10000000, // huge → threshold=200000, volume=100 << threshold → SignalVolumeOK=false
		Bars:          bars,
		SPYBars:       spyBars,
	}
	cfg := confirmCfg()
	res := EvaluateConfirmation(in, cfg)

	if !res.SignalLevelHolds {
		t.Fatalf("SignalLevelHolds should pass (close=100.5 >= entryMid=100)")
	}
	if !res.SignalMarketOK {
		t.Fatalf("SignalMarketOK should pass (SPY up)")
	}
	if res.Diagnostics.OptionalChecksPassed >= 1 {
		t.Fatalf("expected 0 optional signals to pass, got %d", res.Diagnostics.OptionalChecksPassed)
	}
	if res.Status != "watch_only" {
		t.Fatalf("expected watch_only when both required pass but no optionals, got %q", res.Status)
	}
}
