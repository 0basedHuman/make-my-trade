package risk

import (
	"testing"
	"time"

	"github.com/yourname/makemytrade/internal/strategy"
)

// testRules returns MechanicalExitsConfig with canonical test values:
// stop=30%, TP=50%, trail_start=35%, trail_giveback=20%.
func testRules() strategy.MechanicalExitsConfig {
	return strategy.MechanicalExitsConfig{
		Enabled:                     true,
		PremiumStopLossPct:          30,
		PremiumTakeProfitPct:        50,
		PremiumTrailingStartPct:     35,
		PremiumTrailingGivebackPct:  20,
		MaxHoldDaysWithoutReconfirm: 1,
	}
}

// midday returns a time well inside trading hours (e.g. 10:00 PT).
func midday() time.Time {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	return time.Date(2026, 4, 24, 10, 0, 0, 0, loc)
}

// eodTime returns a time at 12:45 PT.
func eodTime() time.Time {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	return time.Date(2026, 4, 24, 12, 45, 0, 0, loc)
}

func basePos(entry float64) PositionRiskState {
	return PositionRiskState{
		PositionID:   "pos-1",
		Ticker:       "SPY",
		OptionSymbol: "SPY260424C00500000",
		EntryPremium: entry,
	}
}

// ── Stop loss ─────────────────────────────────────────────────────────────────

func TestStopLoss_DownExactly30Pct(t *testing.T) {
	pos := basePos(5.00)
	// 5.00 * 0.70 = 3.50 — exactly at floor
	dec := EvaluateMechanicalExit(pos, 3.50, testRules(), midday())
	if !dec.ShouldExit {
		t.Fatalf("want ShouldExit=true at exactly -30%%, got false")
	}
	if dec.Reason != ExitReasonPremiumStopLoss {
		t.Fatalf("want reason %s got %s", ExitReasonPremiumStopLoss, dec.Reason)
	}
}

func TestStopLoss_DownBelow30Pct(t *testing.T) {
	pos := basePos(5.00)
	dec := EvaluateMechanicalExit(pos, 3.00, testRules(), midday()) // -40%
	if !dec.ShouldExit || dec.Reason != ExitReasonPremiumStopLoss {
		t.Fatalf("want stop loss exit, got shouldExit=%v reason=%s", dec.ShouldExit, dec.Reason)
	}
}

func TestStopLoss_NoFireAboveFloor(t *testing.T) {
	pos := basePos(5.00)
	dec := EvaluateMechanicalExit(pos, 3.51, testRules(), midday()) // just above floor
	if dec.ShouldExit {
		t.Fatalf("stop should not fire at -29.8%%, got exit reason=%s", dec.Reason)
	}
}

// ── Take profit ───────────────────────────────────────────────────────────────

func TestTakeProfit_UpExactly50Pct(t *testing.T) {
	pos := basePos(5.00)
	// 5.00 * 1.50 = 7.50
	dec := EvaluateMechanicalExit(pos, 7.50, testRules(), midday())
	if !dec.ShouldExit {
		t.Fatalf("want ShouldExit=true at +50%%, got false")
	}
	if dec.Reason != ExitReasonPremiumTakeProfit {
		t.Fatalf("want reason %s got %s", ExitReasonPremiumTakeProfit, dec.Reason)
	}
}

func TestTakeProfit_UpAbove50Pct(t *testing.T) {
	pos := basePos(5.00)
	dec := EvaluateMechanicalExit(pos, 8.00, testRules(), midday()) // +60%
	if !dec.ShouldExit || dec.Reason != ExitReasonPremiumTakeProfit {
		t.Fatalf("want TP exit at +60%%, got shouldExit=%v reason=%s", dec.ShouldExit, dec.Reason)
	}
}

func TestTakeProfit_NoFireBeforeCeiling(t *testing.T) {
	pos := basePos(5.00)
	dec := EvaluateMechanicalExit(pos, 7.49, testRules(), midday()) // just under ceiling
	if dec.ShouldExit {
		t.Fatalf("TP should not fire at +49.8%%, got exit reason=%s", dec.Reason)
	}
}

// ── Trailing activation ───────────────────────────────────────────────────────

func TestTrailingActivates_At35Pct(t *testing.T) {
	pos := basePos(5.00)
	// +35% = 5.00 * 1.35 = 6.75
	dec := EvaluateMechanicalExit(pos, 6.75, testRules(), midday())
	if !dec.TrailingActive {
		t.Fatalf("trailing should activate at +35%%")
	}
	if dec.ShouldExit {
		t.Fatalf("should not exit just because trail activated; current is at exact start")
	}
}

func TestTrailingNotActivated_Below35Pct(t *testing.T) {
	pos := basePos(5.00)
	dec := EvaluateMechanicalExit(pos, 6.74, testRules(), midday()) // just under +35%
	if dec.TrailingActive {
		t.Fatalf("trailing should not activate below +35%%")
	}
}

// ── Trailing giveback ─────────────────────────────────────────────────────────

func TestTrailingGiveback_20PctFromPeak(t *testing.T) {
	// Trail already active, peak at 8.00. Current = 8.00 * 0.80 = 6.40 (exactly -20%).
	pos := basePos(5.00)
	pos.TrailingActive = true
	pos.PeakOptionPrice = 8.00
	dec := EvaluateMechanicalExit(pos, 6.40, testRules(), midday())
	if !dec.ShouldExit {
		t.Fatalf("want trailing giveback exit at exactly -20%% from peak, got false")
	}
	if dec.Reason != ExitReasonPremiumTrailingGiveback {
		t.Fatalf("want reason %s got %s", ExitReasonPremiumTrailingGiveback, dec.Reason)
	}
}

func TestTrailingGiveback_NoBothPeakUpdated(t *testing.T) {
	// Trail active but current still above trail floor — no exit.
	pos := basePos(5.00)
	pos.TrailingActive = true
	pos.PeakOptionPrice = 8.00
	dec := EvaluateMechanicalExit(pos, 6.41, testRules(), midday()) // just above floor
	if dec.ShouldExit {
		t.Fatalf("trail giveback should not fire at -19.9%% from peak")
	}
}

func TestTrailingGiveback_PeakUpdated(t *testing.T) {
	// Current exceeds previous peak — peak should be updated to current.
	pos := basePos(5.00)
	pos.TrailingActive = true
	pos.PeakOptionPrice = 7.00
	dec := EvaluateMechanicalExit(pos, 7.50, testRules(), midday())
	if dec.PeakPremium != 7.50 {
		t.Fatalf("want peak updated to 7.50, got %.2f", dec.PeakPremium)
	}
}

// ── No forced EOD exit — 21-45 DTE swings hold overnight ────────────────────

func TestNoEODExit_HoldsOvernightByDefault(t *testing.T) {
	// Position at +10%: within all rule limits, no exit should fire at EOD.
	pos := basePos(5.00)
	dec := EvaluateMechanicalExit(pos, 5.50, testRules(), eodTime())
	if dec.ShouldExit {
		t.Fatalf("21-45 DTE swings must hold overnight by default; got exit reason=%s", dec.Reason)
	}
}

// ── Same-day hard invalidation fires immediately ──────────────────────────────

func TestSameDayStop_FiresImmediately(t *testing.T) {
	// DaysHeld=0 (entered today): stop loss must fire when premium drops.
	// No min-hold protection — same-day hard invalidation is valid.
	pos := basePos(5.00)
	pos.DaysHeld = 0
	dec := EvaluateMechanicalExit(pos, 3.50, testRules(), midday()) // exactly -30%
	if !dec.ShouldExit {
		t.Fatalf("stop loss must fire on day 0 when premium is at stop floor")
	}
	if dec.Reason != ExitReasonPremiumStopLoss {
		t.Fatalf("want PREMIUM_STOP_LOSS got %s", dec.Reason)
	}
}

func TestSameDayTrailing_StillFires(t *testing.T) {
	// Trail was already active (e.g. entry was yesterday's close, day 0 today).
	// Trailing giveback must fire regardless of DaysHeld.
	pos := basePos(5.00)
	pos.DaysHeld = 0
	pos.TrailingActive = true
	pos.PeakOptionPrice = 8.00
	dec := EvaluateMechanicalExit(pos, 6.40, testRules(), midday()) // -20% from peak
	if !dec.ShouldExit {
		t.Fatalf("trailing giveback must fire on day 0; got ShouldExit=false")
	}
	if dec.Reason != ExitReasonPremiumTrailingGiveback {
		t.Fatalf("want PREMIUM_TRAILING_GIVEBACK got %s", dec.Reason)
	}
}

func TestSameDayValidPosition_NoExit(t *testing.T) {
	// Position is healthy on day 0: no exit should fire.
	pos := basePos(5.00)
	pos.DaysHeld = 0
	rules := testRules()
	rules.TimeStopDays = 2 // time stop only fires after 2 days
	rules.MaxHoldDaysWithoutReconfirm = 5
	dec := EvaluateMechanicalExit(pos, 5.50, rules, midday()) // +10%
	if dec.ShouldExit {
		t.Fatalf("healthy position on day 0 should not exit; got reason=%s", dec.Reason)
	}
}

// ── No exit in normal range ───────────────────────────────────────────────────

func TestNoExit_NormalRange(t *testing.T) {
	pos := basePos(5.00)
	// Current at 5.50 (+10%): above stop floor (3.50), below TP (7.50), no trail
	dec := EvaluateMechanicalExit(pos, 5.50, testRules(), midday())
	if dec.ShouldExit {
		t.Fatalf("no exit should fire at +10%% in normal range, got reason=%s", dec.Reason)
	}
	if dec.TrailingActive {
		t.Fatalf("trailing should not be active at +10%%")
	}
}

func TestNoExit_ZeroEntrySkipped(t *testing.T) {
	pos := basePos(0) // entry = 0 → skip all checks
	dec := EvaluateMechanicalExit(pos, 5.00, testRules(), midday())
	if dec.ShouldExit {
		t.Fatalf("no exit should fire when entry premium is 0")
	}
}

func TestNoExit_RulesDisabled(t *testing.T) {
	pos := basePos(5.00)
	rules := testRules()
	rules.Enabled = false
	dec := EvaluateMechanicalExit(pos, 1.00, rules, midday()) // would be -80% stop
	if dec.ShouldExit {
		t.Fatalf("no exit should fire when rules are disabled")
	}
}

// ── Structure invalidation ────────────────────────────────────────────────────

func TestStructureInvalidation_Bullish_UnderlyingBelowBreakout(t *testing.T) {
	pos := basePos(5.00)
	pos.Direction = "bullish"
	pos.StructureInvalidationLevel = 100.0
	pos.UnderlyingClose = 98.0 // closed below breakout
	dec := EvaluateMechanicalExit(pos, 5.10, testRules(), midday()) // option still fine
	if !dec.ShouldExit {
		t.Fatal("structure_invalidation must fire when bullish underlying closes below breakout level")
	}
	if dec.Reason != ExitReasonStructureInvalidation {
		t.Fatalf("want STRUCTURE_INVALIDATION got %s", dec.Reason)
	}
}

func TestStructureInvalidation_Bearish_UnderlyingAboveBreakdown(t *testing.T) {
	pos := basePos(5.00)
	pos.Direction = "bearish"
	pos.StructureInvalidationLevel = 100.0
	pos.UnderlyingClose = 102.0 // closed above breakdown level
	dec := EvaluateMechanicalExit(pos, 5.10, testRules(), midday())
	if !dec.ShouldExit {
		t.Fatal("structure_invalidation must fire when bearish underlying closes above breakdown level")
	}
	if dec.Reason != ExitReasonStructureInvalidation {
		t.Fatalf("want STRUCTURE_INVALIDATION got %s", dec.Reason)
	}
}

func TestStructureInvalidation_Bullish_HoldsAboveBreakout(t *testing.T) {
	pos := basePos(5.00)
	pos.Direction = "bullish"
	pos.StructureInvalidationLevel = 100.0
	pos.UnderlyingClose = 101.5 // still above breakout
	dec := EvaluateMechanicalExit(pos, 5.10, testRules(), midday())
	if dec.ShouldExit {
		t.Fatalf("no structure invalidation when underlying holds above breakout; got %s", dec.Reason)
	}
}

func TestStructureInvalidation_Skipped_WhenUnderlyingZero(t *testing.T) {
	// UnderlyingClose = 0 → caller didn't populate it → rule skipped
	pos := basePos(5.00)
	pos.Direction = "bullish"
	pos.StructureInvalidationLevel = 100.0
	pos.UnderlyingClose = 0 // not populated
	dec := EvaluateMechanicalExit(pos, 5.10, testRules(), midday())
	if dec.ShouldExit {
		t.Fatal("structure_invalidation must be skipped when UnderlyingClose is 0")
	}
}

func TestStructureInvalidation_PremiumStopHasPriority(t *testing.T) {
	// Both stop loss AND structure invalidation triggered — stop loss fires first.
	pos := basePos(5.00)
	pos.Direction = "bullish"
	pos.StructureInvalidationLevel = 100.0
	pos.UnderlyingClose = 98.0 // below breakout
	// Premium also at stop floor: 5.00 * 0.70 = 3.50
	dec := EvaluateMechanicalExit(pos, 3.50, testRules(), midday())
	if !dec.ShouldExit {
		t.Fatal("exit must fire")
	}
	if dec.Reason != ExitReasonPremiumStopLoss {
		t.Fatalf("premium stop loss must take priority over structure invalidation; got %s", dec.Reason)
	}
}
