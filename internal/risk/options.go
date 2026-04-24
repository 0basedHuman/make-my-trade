// internal/risk/options.go
//
// WHAT: Deterministic mechanical exit evaluator for paper option positions.
//
// WHY:  7–14 DTE options decay quickly. Relying solely on scheduled Claude
//       review (07:15, 07:45, 12:45 PT) is too slow for hard stops and take
//       profits. This package provides a pure function that evaluates every
//       mechanical exit rule for a single position given its current premium.
//
//       Claude is the final authority for ENTRY and can APPROVE hold overnight.
//       Claude CANNOT override mechanical stop, take-profit, or trailing exits.
//
// HOW:  EvaluateMechanicalExit is called by RunMechanicalRiskCheckActivity
//       every 10 minutes during market hours. It never touches the database
//       or network — it takes values and returns a decision.
//
// Exit rule hierarchy (evaluated in order; first match wins):
//   1. PREMIUM_STOP_LOSS       — down ≥ stop_pct from entry
//   2. PREMIUM_TAKE_PROFIT     — up ≥ take_profit_pct from entry
//   3. PREMIUM_TRAILING_GIVEBACK — trail active AND gives back ≥ giveback_pct from peak
//   4. EOD_EXIT_NO_HOLD_APPROVAL — near EOD, hold_overnight_approved=false
//   5. no exit (position stays open)
//
// Trailing state update (side-effect-free; caller persists these):
//   - if currentPremium >= entryPremium * (1 + trailing_start_pct/100):
//       TrailingActive = true
//   - if currentPremium > previousPeak:
//       NewPeakPremium = currentPremium
//
// WHAT BREAKS: If entryPremium is 0, all percentage checks are skipped and no
//              exit fires. Callers must verify option_premium > 0 before calling.
//
// VERIFY:
//   entry=5.00 current=3.50 → (3.50-5.00)/5.00 = -30% → PREMIUM_STOP_LOSS
//   entry=5.00 current=7.50 → (7.50-5.00)/5.00 = +50% → PREMIUM_TAKE_PROFIT
//   entry=5.00 current=6.80 → +36%: trail activates. peak=6.80, current=5.44 → -20% from peak → PREMIUM_TRAILING_GIVEBACK

package risk

import (
	"time"

	"github.com/yourname/makemytrade/internal/strategy"
)

// ExitReason is the machine-readable cause for a mechanical exit.
const (
	ExitReasonPremiumStopLoss         = "PREMIUM_STOP_LOSS"
	ExitReasonPremiumTakeProfit       = "PREMIUM_TAKE_PROFIT"
	ExitReasonPremiumTrailingGiveback = "PREMIUM_TRAILING_GIVEBACK"
	ExitReasonEODNoHoldApproval       = "EOD_EXIT_NO_HOLD_APPROVAL"
)

// PositionRiskState carries the mutable per-position state needed by
// EvaluateMechanicalExit. Loaded from the paper_positions row.
type PositionRiskState struct {
	PositionID            string
	Ticker                string
	OptionSymbol          string
	EntryPremium          float64   // option_premium at entry
	PeakOptionPrice       float64   // highest mid-price seen; 0 if not yet updated
	TrailingActive        bool      // true once +trailing_start_pct reached
	HoldOvernightApproved bool      // Claude approved hold overnight for current day
	EntryDate             time.Time // entry_date from paper_positions
	DaysHeld              int       // caller computes (now - entry_date).Days()
}

// MechanicalExitDecision is the output of EvaluateMechanicalExit.
type MechanicalExitDecision struct {
	ShouldExit     bool
	Reason         string // one of the ExitReason* constants, or "" if no exit
	CurrentPremium float64
	EntryPremium   float64
	PnLPct         float64 // (current - entry) / entry * 100
	PeakPremium    float64 // updated peak (for caller to persist)
	TrailingActive bool    // updated trailing state (for caller to persist)
}

// EvaluateMechanicalExit applies all mechanical exit rules for one position.
//
// Parameters:
//
//	pos            — current risk state loaded from DB
//	currentPremium — current option mid-price fetched from Alpaca
//	rules          — from strategy_rules.yaml
//	nowPT          — current time in America/Los_Angeles (for EOD check)
//
// Returns a MechanicalExitDecision. The caller must:
//  1. Persist PeakPremium and TrailingActive back to the DB (always).
//  2. If ShouldExit: call execution.SellOptionPosition with Reason.
func EvaluateMechanicalExit(pos PositionRiskState, currentPremium float64, rules strategy.MechanicalExitsConfig, nowPT time.Time) MechanicalExitDecision {
	entry := pos.EntryPremium
	dec := MechanicalExitDecision{
		CurrentPremium: currentPremium,
		EntryPremium:   entry,
		TrailingActive: pos.TrailingActive,
		PeakPremium:    pos.PeakOptionPrice,
	}

	// If entry is zero or rules are disabled, nothing to check.
	if !rules.Enabled || entry <= 0 || currentPremium <= 0 {
		return dec
	}

	// PnL % from entry
	dec.PnLPct = (currentPremium - entry) / entry * 100.0

	// ── Update peak (always, before exit check) ───────────────────────────────
	if currentPremium > dec.PeakPremium {
		dec.PeakPremium = currentPremium
	}

	// ── Activate trailing ─────────────────────────────────────────────────────
	if !dec.TrailingActive && dec.PnLPct >= rules.PremiumTrailingStartPct {
		dec.TrailingActive = true
	}

	// ── Exit rules (evaluated in priority order) ──────────────────────────────

	// 1. Stop loss: down ≥ stop_pct from entry
	stopFloor := entry * (1.0 - rules.PremiumStopLossPct/100.0)
	if currentPremium <= stopFloor {
		dec.ShouldExit = true
		dec.Reason = ExitReasonPremiumStopLoss
		return dec
	}

	// 2. Take profit: up ≥ take_profit_pct from entry
	tpCeiling := entry * (1.0 + rules.PremiumTakeProfitPct/100.0)
	if currentPremium >= tpCeiling {
		dec.ShouldExit = true
		dec.Reason = ExitReasonPremiumTakeProfit
		return dec
	}

	// 3. Trailing giveback: trail active AND current ≤ peak * (1 - giveback_pct/100)
	if dec.TrailingActive && dec.PeakPremium > 0 {
		trailFloor := dec.PeakPremium * (1.0 - rules.PremiumTrailingGivebackPct/100.0)
		if currentPremium <= trailFloor {
			dec.ShouldExit = true
			dec.Reason = ExitReasonPremiumTrailingGiveback
			return dec
		}
	}

	// 4. EOD exit: within 30 minutes of 13:00 PT (market close 13:00) and hold not approved.
	//    Market closes at 13:00 PT. We force exit at 12:45 PT or later unless approved.
	if rules.ForceEODExitUnlessHoldConfirmed && !pos.HoldOvernightApproved {
		eodCutoff := time.Date(nowPT.Year(), nowPT.Month(), nowPT.Day(), 12, 45, 0, 0, nowPT.Location())
		if nowPT.After(eodCutoff) || nowPT.Equal(eodCutoff) {
			dec.ShouldExit = true
			dec.Reason = ExitReasonEODNoHoldApproval
			return dec
		}
	}

	return dec
}
