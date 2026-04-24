package risk

import (
	"testing"
	"time"

	"github.com/yourname/makemytrade/internal/strategy"
)

// testRules returns MechanicalExitsConfig with canonical test values:
// stop=30%, TP=50%, trail_start=35%, trail_giveback=20%, EOD=true.
func testRules() strategy.MechanicalExitsConfig {
	return strategy.MechanicalExitsConfig{
		Enabled:                         true,
		PremiumStopLossPct:              30,
		PremiumTakeProfitPct:            50,
		PremiumTrailingStartPct:         35,
		PremiumTrailingGivebackPct:      20,
		ForceEODExitUnlessHoldConfirmed: true,
		MaxHoldDaysWithoutReconfirm:     1,
	}
}

// midday returns a time well inside trading hours (e.g. 10:00 PT) so EOD checks don't fire.
func midday() time.Time {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	return time.Date(2026, 4, 24, 10, 0, 0, 0, loc)
}

// eodTime returns a time at 12:45 PT to trigger EOD checks.
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

// ── EOD exit ──────────────────────────────────────────────────────────────────

func TestEODExit_FiresAt1245_NotApproved(t *testing.T) {
	pos := basePos(5.00)
	pos.HoldOvernightApproved = false
	dec := EvaluateMechanicalExit(pos, 5.50, testRules(), eodTime())
	if !dec.ShouldExit {
		t.Fatalf("EOD exit should fire at 12:45 without hold approval")
	}
	if dec.Reason != ExitReasonEODNoHoldApproval {
		t.Fatalf("want reason %s got %s", ExitReasonEODNoHoldApproval, dec.Reason)
	}
}

func TestEODExit_NoFireWhenApproved(t *testing.T) {
	pos := basePos(5.00)
	pos.HoldOvernightApproved = true
	dec := EvaluateMechanicalExit(pos, 5.50, testRules(), eodTime())
	if dec.ShouldExit {
		t.Fatalf("EOD exit should NOT fire when hold_overnight_approved=true")
	}
}

func TestEODExit_NoFireBeforeCutoff(t *testing.T) {
	pos := basePos(5.00)
	pos.HoldOvernightApproved = false
	// 12:44 PT — just before EOD cutoff
	loc, _ := time.LoadLocation("America/Los_Angeles")
	beforeEOD := time.Date(2026, 4, 24, 12, 44, 59, 0, loc)
	dec := EvaluateMechanicalExit(pos, 5.50, testRules(), beforeEOD)
	if dec.ShouldExit {
		t.Fatalf("EOD exit should not fire before 12:45 PT")
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
