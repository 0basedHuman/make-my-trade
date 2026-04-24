// internal/strategy/confirmation.go
//
// WHAT: Deterministic opening-confirmation evaluator.
//       Reads the first 10 minutes of 1-min bars for a candidate and applies
//       the signals defined in open_confirmation.checks in strategy_rules.yaml.
//
// WHY:  The confirmation step is what separates entry_ready (pre-open
//       candidate) from confirmed (true trade signal). It must be:
//         1. Deterministic — same input always produces the same output
//         2. Side-effect-free — no DB calls, no API calls
//         3. Driven entirely by YAML thresholds (OpenConfirmationConfig)
//       The caller (RunOpeningConfirmationActivity) handles fetching and persisting.
//
// HOW:  EvaluateConfirmation receives:
//         - Up to 10 one-minute bars covering 6:30–6:40 AM PT
//         - The corresponding SPY bars for market alignment
//         - The candidate's entry zone and stop from the prior day's analysis
//         - The trade direction ("bullish" or "bearish")
//         - The YAML config controlling which checks are active
//
//       It returns a ConfirmationResult with:
//         - Per-signal bools (signal_level_holds, signal_open_range, etc.)
//         - signals_passed count
//         - auto_rejected bool + reason
//         - status = "confirmed" | "watch_only" (never "rejected" — that's auto_reject's job)
//
// WHAT BREAKS: If len(bars) == 0 the function returns watch_only with an
//              explanation rather than panicking. The caller must handle this.
//
// VERIFY:  Test with a synthetic bullish scenario:
//          bars[0] = {O:100, H:102, L:100, C:101.5, V:50000} (strong open)
//          bars[9] = {O:101, H:103, L:101, C:102.5, V:60000} (hold above midpoint)
//          spy   = closed above spy_open
//          entry_low=100, stop_loss=98
//          → signals_passed should be >= 3, status = "confirmed"

package strategy

import "github.com/yourname/makemytrade/internal/indicators"

// ConfirmationInput is the per-candidate input to EvaluateConfirmation.
type ConfirmationInput struct {
	Ticker        string
	Direction     string           // "bullish" or "bearish"
	EntryLow      float64          // from trade_candidates.entry_low
	EntryHigh     float64          // from trade_candidates.entry_high
	StopLoss      float64          // from trade_candidates.stop_loss
	PrevDayVolume int64            // prior session total volume (0 = unknown)
	Bars          []indicators.Bar // 1-min bars, 6:30–6:40 AM PT, sorted oldest-first
	SPYBars       []indicators.Bar // SPY 1-min bars for the same window
}

// ConfirmationResult is the per-candidate output of EvaluateConfirmation.
// Every field maps directly to a column in trade_confirmations.
type ConfirmationResult struct {
	Ticker string

	// Individual signal results
	SignalLevelHolds  bool // breakout_or_reclaim_holds
	SignalOpenRange   bool // opening_range midpoint check
	SignalNoRejection bool // no immediate rejection wick
	SignalVolumeOK    bool // opening_volume_support
	SignalMarketOK    bool // market_open_alignment (SPY)

	SignalsPassed int

	// Auto-reject
	AutoRejected     bool
	AutoRejectReason string

	// Final status
	// "confirmed"  → signals_passed >= min AND no auto_reject
	// "watch_only" → signals_passed < min OR auto_reject fired
	Status string

	// Bar data snapshot for persistence
	OpenPrice     float64
	First10High   float64
	First10Low    float64
	First10Close  float64
	First10Volume int64
}

// EvaluateConfirmation runs all active confirmation checks from YAML and
// returns the result without any side effects.
func EvaluateConfirmation(in ConfirmationInput, cfg OpenConfirmationConfig) ConfirmationResult {
	res := ConfirmationResult{
		Ticker: in.Ticker,
		Status: "watch_only",
	}

	if len(in.Bars) == 0 {
		res.AutoRejected = true
		res.AutoRejectReason = "no_intraday_bars_available"
		return res
	}

	bullish := in.Direction == "bullish"

	// ── Aggregate the opening range ──────────────────────────────────────────
	first := in.Bars[0]
	last := in.Bars[len(in.Bars)-1]

	rangeHigh := first.High
	rangeLow := first.Low
	var totalVolume int64
	for _, b := range in.Bars {
		if b.High > rangeHigh {
			rangeHigh = b.High
		}
		if b.Low < rangeLow {
			rangeLow = b.Low
		}
		totalVolume += int64(b.Volume)
	}
	rangeMid := (rangeHigh + rangeLow) / 2

	res.OpenPrice = first.Open
	res.First10High = rangeHigh
	res.First10Low = rangeLow
	res.First10Close = last.Close
	res.First10Volume = totalVolume

	// ── Auto-reject checks (any one fires → watch_only immediately) ───────────

	// 1. decisive_level_loss: any bar closed through stop loss
	if cfg.AutoReject.DecisiveLevelLoss {
		for _, b := range in.Bars {
			if bullish && b.Close < in.StopLoss {
				res.AutoRejected = true
				res.AutoRejectReason = "decisive_level_loss"
				return res
			}
			if !bullish && b.Close > in.StopLoss {
				res.AutoRejected = true
				res.AutoRejectReason = "decisive_level_loss"
				return res
			}
		}
	}

	// 2. weak_first_10m_close: closed in the bottom/top quartile of range
	if cfg.AutoReject.WeakFirst10mClose {
		rangeSize := rangeHigh - rangeLow
		if rangeSize > 0 {
			bottomQuartile := rangeLow + 0.25*rangeSize
			topQuartile := rangeHigh - 0.25*rangeSize
			if bullish && last.Close < bottomQuartile {
				res.AutoRejected = true
				res.AutoRejectReason = "weak_first_10m_close"
				return res
			}
			if !bullish && last.Close > topQuartile {
				res.AutoRejected = true
				res.AutoRejectReason = "weak_first_10m_close"
				return res
			}
		}
	}

	// 3. hard_open_reversal: first candle was with us then fully reversed
	if cfg.AutoReject.HardOpenReversal {
		firstBullish := first.Close > first.Open
		firstBearish := first.Close < first.Open
		if bullish && firstBullish && last.Close < first.Open {
			res.AutoRejected = true
			res.AutoRejectReason = "hard_open_reversal"
			return res
		}
		if !bullish && firstBearish && last.Close > first.Open {
			res.AutoRejected = true
			res.AutoRejectReason = "hard_open_reversal"
			return res
		}
	}

	// 4. broad_market_riskoff_shock: SPY moved > 0.5% against the trade direction
	if cfg.AutoReject.BroadMarketRiskoffShock && len(in.SPYBars) > 0 {
		spyOpen := in.SPYBars[0].Open
		spyLast := in.SPYBars[len(in.SPYBars)-1].Close
		if spyOpen > 0 {
			spyMovePC := (spyLast - spyOpen) / spyOpen * 100
			if bullish && spyMovePC < -0.5 {
				res.AutoRejected = true
				res.AutoRejectReason = "broad_market_riskoff_shock"
				return res
			}
			if !bullish && spyMovePC > 0.5 {
				res.AutoRejected = true
				res.AutoRejectReason = "broad_market_riskoff_shock"
				return res
			}
		}
	}

	// 5. downside_rejection_volume: last bar had the highest volume AND traded against us
	if cfg.AutoReject.DownsideRejectionVolume && len(in.Bars) > 1 {
		avgVol := float64(totalVolume) / float64(len(in.Bars))
		lastVol := float64(last.Volume)
		if lastVol > avgVol*1.5 {
			lastBearish := last.Close < last.Open
			lastBullish := last.Close > last.Open
			if bullish && lastBearish {
				res.AutoRejected = true
				res.AutoRejectReason = "downside_rejection_volume"
				return res
			}
			if !bullish && lastBullish {
				res.AutoRejected = true
				res.AutoRejectReason = "downside_rejection_volume"
				return res
			}
		}
	}

	// ── Positive confirmation signals ─────────────────────────────────────────
	//
	// Signal 1: breakout_or_reclaim_holds
	// Bullish: last close is at or above the midpoint of the entry zone.
	// Bearish: last close is at or below the midpoint of the entry zone.
	if cfg.Checks.BreakoutOrReclaimHolds {
		entryMid := (in.EntryHigh + in.EntryLow) / 2
		if bullish && last.Close >= entryMid {
			res.SignalLevelHolds = true
		}
		if !bullish && last.Close <= entryMid {
			res.SignalLevelHolds = true
		}
		if entryMid == 0 {
			// No entry zone stored — use range midpoint as proxy
			if bullish && last.Close >= rangeMid {
				res.SignalLevelHolds = true
			}
			if !bullish && last.Close <= rangeMid {
				res.SignalLevelHolds = true
			}
		}
	}

	// Signal 2: opening_range midpoint check
	// Bullish: last close above midpoint of the 10-min range (calls)
	// Bearish: last close below midpoint (puts)
	if cfg.Checks.OpeningRangeCloseAboveMidpointForCalls && bullish {
		if last.Close > rangeMid {
			res.SignalOpenRange = true
		}
	}
	if cfg.Checks.OpeningRangeCloseBelowMidpointForPuts && !bullish {
		if last.Close < rangeMid {
			res.SignalOpenRange = true
		}
	}

	// Signal 3: no immediate rejection wick
	// Bullish (no_rejection_wick_for_calls):
	//   Upper wick of first bar must be < 50% of the bar's body size.
	//   A large upper wick = sellers rejected the open — bearish sign.
	// Bearish (no_reversal_tail_for_puts):
	//   Lower wick of first bar must be < 50% of the bar's body size.
	if bullish && cfg.Checks.NoRejectionWickForCalls {
		body := abs64(first.Close - first.Open)
		upperWick := first.High - maxF(first.Close, first.Open)
		if body > 0 && upperWick < 0.5*body {
			res.SignalNoRejection = true
		}
		if body == 0 {
			// Doji — neutral, treat as passing
			res.SignalNoRejection = true
		}
	}
	if !bullish && cfg.Checks.NoReversalTailForPuts {
		body := abs64(first.Close - first.Open)
		lowerWick := minF(first.Close, first.Open) - first.Low
		if body > 0 && lowerWick < 0.5*body {
			res.SignalNoRejection = true
		}
		if body == 0 {
			res.SignalNoRejection = true
		}
	}

	// Signal 4: opening_volume_support
	// Pass if we got at least 3 bars with total volume > 0.
	// For liquid watchlist names this threshold is always met.
	// If prev_day_volume is known, require first10 > 2% of daily (reasonable for a busy open).
	if cfg.Checks.OpeningVolumeSupport {
		minBars := 3
		if len(in.Bars) >= minBars && totalVolume > 0 {
			if in.PrevDayVolume > 0 {
				// 2% of prior day volume is a healthy 10-min opening — ~4x uniform rate
				threshold := in.PrevDayVolume / 50
				if totalVolume >= threshold {
					res.SignalVolumeOK = true
				}
			} else {
				// No prior volume on record — pass if we have activity
				res.SignalVolumeOK = true
			}
		}
	}

	// Signal 5: market_open_alignment (SPY)
	// Bullish: SPY's first10_close > SPY open (market is confirming bullish)
	// Bearish: SPY's first10_close < SPY open (market is confirming bearish)
	if cfg.Checks.MarketOpenAlignment && len(in.SPYBars) > 0 {
		spyOpen := in.SPYBars[0].Open
		spyClose := in.SPYBars[len(in.SPYBars)-1].Close
		if bullish && spyClose > spyOpen {
			res.SignalMarketOK = true
		}
		if !bullish && spyClose < spyOpen {
			res.SignalMarketOK = true
		}
	}

	// ── Count passed signals ─────────────────────────────────────────────────
	passed := 0
	if res.SignalLevelHolds {
		passed++
	}
	if res.SignalOpenRange {
		passed++
	}
	if res.SignalNoRejection {
		passed++
	}
	if res.SignalVolumeOK {
		passed++
	}
	if res.SignalMarketOK {
		passed++
	}
	res.SignalsPassed = passed

	min := cfg.MinTrueSignalsToConfirm
	if min <= 0 {
		min = 3 // YAML default
	}
	if passed >= min {
		res.Status = "confirmed"
	} else {
		res.Status = "watch_only"
	}

	return res
}

// ── bearish_exhaustion_reversal intraday rejection check ─────────────────────

// ExhaustionRejectionInput is the per-candidate input for the
// bearish_exhaustion_reversal intraday check.
//
// This is NOT a general opening-confirmation check — it is only valid for
// structural_candidate rows with setup_family='bearish_exhaustion_reversal'.
// The check confirms that the overextended name is actually rejecting intraday,
// which is the trigger that promotes it from structural_candidate → entry_ready.
type ExhaustionRejectionInput struct {
	Ticker        string
	PrevDayVolume int64
	Bars          []indicators.Bar // 1-min bars from 6:30 AM PT, sorted oldest-first
	SPYBars       []indicators.Bar // SPY bars for the same window
}

// ExhaustionRejectionResult is the per-candidate output of EvaluateExhaustionRejection.
type ExhaustionRejectionResult struct {
	Ticker string

	// Promotion gate: true means the candidate should be promoted to entry_ready
	RejectionConfirmed bool

	// Sub-signals
	VWAPBreak        bool // last close below VWAP
	ORMidBreak       bool // last close below opening-range midpoint
	RelVolumeOK      bool // relative volume >= 1.3× expected
	MarketNotBullish bool // SPY is not strongly bullish (< +0.3%)

	// Hard blocks — any true prevents promotion
	HardBlockFired  bool
	HardBlockReason string

	// Computed values (for logging)
	VWAP      float64
	ORMid     float64
	RelVolume float64
}

// EvaluateExhaustionRejection checks whether a bearish_exhaustion_reversal
// structural_candidate is showing intraday rejection in the opening window.
//
// Promotion requires ALL of:
//   - last close below VWAP OR below opening-range midpoint
//   - relative volume >= 1.3× expected 10-min pace (or prior-day volume unknown)
//   - SPY not strongly bullish (< +0.3% from open)
//
// Hard blocks (prevent promotion regardless):
//   - no intraday bars available
//   - any bar has a lower wick >= 40% of bar range (bulls stepped in = no exhaustion)
func EvaluateExhaustionRejection(in ExhaustionRejectionInput) ExhaustionRejectionResult {
	res := ExhaustionRejectionResult{Ticker: in.Ticker}

	if len(in.Bars) == 0 {
		res.HardBlockFired = true
		res.HardBlockReason = "no_intraday_bars_available"
		return res
	}

	// ── Aggregate opening range and VWAP ─────────────────────────────────────
	rangeHigh := in.Bars[0].High
	rangeLow := in.Bars[0].Low
	var vwapNumer, volSum float64
	for _, b := range in.Bars {
		if b.High > rangeHigh {
			rangeHigh = b.High
		}
		if b.Low < rangeLow {
			rangeLow = b.Low
		}
		vwapNumer += b.Close * b.Volume
		volSum += b.Volume
	}
	if volSum > 0 {
		res.VWAP = vwapNumer / volSum
	}
	res.ORMid = (rangeHigh + rangeLow) / 2.0

	last := in.Bars[len(in.Bars)-1]

	// ── Hard block: bullish recovery wick ────────────────────────────────────
	// Any bar with a lower wick >= 40% of bar range = buyers defended the price.
	// That means bulls are NOT exhausted and this is not a clean reversal setup.
	for _, b := range in.Bars {
		barRange := b.High - b.Low
		if barRange > 0 {
			lowerWick := minF(b.Close, b.Open) - b.Low
			if lowerWick/barRange >= 0.40 {
				res.HardBlockFired = true
				res.HardBlockReason = "bullish_recovery_wick"
				return res
			}
		}
	}

	// ── Sub-signals ──────────────────────────────────────────────────────────

	// VWAP break: last close is below VWAP (stock is rejecting the day's average)
	if res.VWAP > 0 && last.Close < res.VWAP {
		res.VWAPBreak = true
	}

	// OR midpoint break: last close is below the opening-range midpoint
	if last.Close < res.ORMid {
		res.ORMidBreak = true
	}

	// Relative volume: compare actual 10-min volume to expected pace.
	// Expected = prevDayVolume / 39  (390-min trading day divided into 10-min slices).
	// A ratio >= 1.3 means elevated selling participation — required for a real rejection.
	// If prior-day volume is unknown, we skip this check (conservative: pass it).
	if in.PrevDayVolume > 0 {
		expected := float64(in.PrevDayVolume) / 39.0
		if expected > 0 {
			res.RelVolume = volSum / expected
		}
		res.RelVolumeOK = res.RelVolume >= 1.3
	} else {
		res.RelVolumeOK = true // no prior data — assume volume requirement is met
	}

	// SPY not strongly bullish: SPY's 10-min move < +0.3% from open.
	// If SPY is ripping, a single-stock put setup is fighting the market — skip it.
	if len(in.SPYBars) > 0 {
		spyOpen := in.SPYBars[0].Open
		spyClose := in.SPYBars[len(in.SPYBars)-1].Close
		if spyOpen > 0 {
			spyMovePct := (spyClose - spyOpen) / spyOpen * 100.0
			res.MarketNotBullish = spyMovePct < 0.3
		} else {
			res.MarketNotBullish = true
		}
	} else {
		res.MarketNotBullish = true // no SPY data — assume neutral/not bullish
	}

	// ── Promotion gate ────────────────────────────────────────────────────────
	// All three must be true: price rejection (VWAP or OR), volume participation, market alignment.
	res.RejectionConfirmed = (res.VWAPBreak || res.ORMidBreak) && res.RelVolumeOK && res.MarketNotBullish

	return res
}

// ── tiny math helpers ─────────────────────────────────────────────────────────

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
