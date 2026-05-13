// internal/risk/options.go
//
// WHAT: Deterministic mechanical exit evaluator for paper option positions.
//
// WHY:  21-45 DTE options. All exits are mechanical — no Claude involvement.
//       This package provides a pure function that evaluates every mechanical
//       exit rule for a single position given its current premium.
//
// HOW:  EvaluateMechanicalExit is called by RunMechanicalRiskCheckActivity
//       every 10 minutes during market hours. It never touches the database
//       or network — it takes values and returns a decision.
//
// Exit rule hierarchy (evaluated in order; first match wins):
//   1. PREMIUM_STOP_LOSS         — down ≥ 25% from entry (entry×0.75)
//   2. PREMIUM_TAKE_PROFIT       — up ≥ 70% from entry
//   3. PREMIUM_TRAILING_GIVEBACK — trail active AND gives back ≥ 10% from peak
//   4. TIME_STOP                 — days_held ≥ time_stop_days AND no trailing activated
//   5. MAX_HOLD_DAYS             — days_held ≥ max_hold_days (hard time limit)
//   6. STRUCTURE_INVALIDATION    — underlying closes below breakout level (bullish) or above breakdown level (bearish)
//   7. no exit (position stays open; overnight hold is default for 21-45 DTE swings)
//
// Trailing state:
//   - arm trailing once premium up ≥ 35% from entry (trailing_start_pct)
//   - exit if premium gives back ≥ 10% from peak (trailing_giveback_pct)
//
// WHAT BREAKS: If entryPremium is 0, all percentage checks are skipped and no
//              exit fires. Callers must verify option_premium > 0 before calling.
//
// VERIFY:
//   entry=5.00 current=3.75 → (3.75-5.00)/5.00 = -25% → PREMIUM_STOP_LOSS
//   entry=5.00 current=8.50 → (8.50-5.00)/5.00 = +70% → PREMIUM_TAKE_PROFIT
//   entry=5.00 current=6.75 → +35%: trail activates. peak=6.75, current=6.08 → -10% from peak → PREMIUM_TRAILING_GIVEBACK

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
	ExitReasonTimeStop                = "TIME_STOP_NO_FOLLOWTHROUGH"
	ExitReasonMaxHoldDays             = "MAX_HOLD_DAYS_REACHED"
	ExitReasonStructureInvalidation   = "STRUCTURE_INVALIDATION"
)

// PositionRiskState carries the mutable per-position state needed by
// EvaluateMechanicalExit. Loaded from the paper_positions row.
type PositionRiskState struct {
	PositionID      string
	Ticker          string
	OptionSymbol    string
	EntryPremium    float64   // option_premium at entry
	PeakOptionPrice float64   // highest mid-price seen; 0 if not yet updated
	TrailingActive  bool      // true once +trailing_start_pct reached
	EntryDate       time.Time // entry_date from paper_positions
	DaysHeld        int       // caller computes (now - entry_date).Days()

	// Structure invalidation fields (rule 6).
	// Direction: "bullish" or "bearish" (derived from option_type).
	// StructureInvalidationLevel: pattern-derived level (bull_flag→flag low, triangle→swing low, etc.)
	// UnderlyingClose: current underlying mid-price (0 = skip rule).
	Direction                  string
	StructureInvalidationLevel float64
	UnderlyingClose            float64
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

	// 4. Time stop: no follow-through within time_stop_days — breakout failed.
	if rules.TimeStopDays > 0 && pos.DaysHeld >= rules.TimeStopDays && !dec.TrailingActive {
		dec.ShouldExit = true
		dec.Reason = ExitReasonTimeStop
		return dec
	}

	// 5. Max hold days: hard time limit regardless of trailing or follow-through.
	maxHold := rules.MaxHoldDaysWithoutReconfirm
	if maxHold > 0 && pos.DaysHeld >= maxHold {
		dec.ShouldExit = true
		dec.Reason = ExitReasonMaxHoldDays
		return dec
	}

	// 6. Structure invalidation: underlying closes back through the pattern-derived invalidation level.
	// Only fires when UnderlyingClose is populated by the caller (> 0) and level is set.
	if pos.UnderlyingClose > 0 && pos.StructureInvalidationLevel > 0 {
		switch pos.Direction {
		case "bullish":
			if pos.UnderlyingClose < pos.StructureInvalidationLevel {
				dec.ShouldExit = true
				dec.Reason = ExitReasonStructureInvalidation
				return dec
			}
		case "bearish":
			if pos.UnderlyingClose > pos.StructureInvalidationLevel {
				dec.ShouldExit = true
				dec.Reason = ExitReasonStructureInvalidation
				return dec
			}
		}
	}

	return dec
}
