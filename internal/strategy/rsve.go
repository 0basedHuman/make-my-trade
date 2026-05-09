// internal/strategy/rsve.go
//
// WHAT: RSVE-O (Relative Strength Volatility Expansion Options) binary gate evaluator.
//
// WHY:  Binary gates enforce a hard quality floor: every gate must pass or the ticker
//       is rejected with a precise diagnostic for the failure. Score is a ranking tool
//       only — it never decides tradability.
//
// Gates (8 logical groups, 10 binary checks):
//   Market regime:       vix_regime + market_direction
//   No earnings:         no_earnings
//   Relative strength:   relative_strength / relative_weakness
//   EMA structure:       ema_structure
//   Volatility compress: vol_squeeze
//   Breakout/breakdown:  breakout_trigger / breakdown_trigger
//   Volume expansion:    volume_expansion
//   Options liquidity:   iv_rank_ok + spread_quality + oi_minimum + option_volume
//
// WHAT BREAKS: When any option field is -1 (unavailable), the result is
//              "stock_signal_passed" — no paper trade is created until option
//              quality is verified. Daily dryscan always passes -1 for options.
//
// VERIFY: go test ./internal/strategy/ -run TestRSVE

package strategy

import (
	"fmt"
	"math"

	"github.com/yourname/makemytrade/internal/indicators"
)

// ── Public types ──────────────────────────────────────────────────────────────

// GateDiagnostic describes one binary gate evaluation result.
type GateDiagnostic struct {
	Name        string `json:"name"`
	Passed      bool   `json:"passed"`
	Blocking    bool   `json:"blocking"`     // true = this gate caused the rejection
	ActualValue string `json:"actual_value"` // human-readable computed value
	Threshold   string `json:"threshold"`    // human-readable rule boundary
	DataSource  string `json:"data_source"`
}

// RSVEInput is everything EvaluateRSVE needs for one ticker.
type RSVEInput struct {
	Ticker string
	Date   string

	// Price and volume bars, sorted oldest-first. Need 100+ bars for all gates.
	Bars []indicators.Bar

	// SPY bars for relative-strength and market-direction gates.
	SPYBars []indicators.Bar

	// Macro context.
	VIX float64

	// Earnings proximity. -1 = unknown (gate passes). 0 = today. N = N days away.
	EarningsDaysAway int

	// Options chain quality. -1 = unavailable (gate passes silently).
	IVRank          float64 // 0-100
	BidAskSpreadPct float64 // % of mid price
	OpenInterest    int
	OptionVolume    int // daily option volume for target strike; -1 = unavailable
}

// RSVEResult is the full evaluation output for one ticker.
type RSVEResult struct {
	Ticker          string          `json:"ticker"`
	Date            string          `json:"date"`
	Direction       string          `json:"direction"`              // "bullish" | "bearish" | "none"
	AllPass         bool            `json:"all_pass"`
	Gates           []GateDiagnostic `json:"gates"`
	Score           float64          `json:"score"`                  // ranking score, 0-100; only valid when AllPass
	Status          string           `json:"status"`                 // "paper_trade_created" | "stock_signal_passed" | "option_blocked" | "rejected"
	RejectGate      string           `json:"reject_gate,omitempty"` // first blocking gate name
	PatternAnalysis PatternAnalysis  `json:"pattern_analysis"`       // structural pattern result (ranking context)
}

// ── Evaluator ─────────────────────────────────────────────────────────────────

// EvaluateRSVE runs the RSVE-O gate battery for one ticker.
// Both branches are tried. If exactly one passes, it wins. If both pass (edge case),
// higher score wins. If neither passes, the branch with fewer failures is returned
// with AllPass=false for richer diagnostics.
func EvaluateRSVE(input RSVEInput, cfg RSVEConfig) RSVEResult {
	bullResult := evaluateBranch(input, cfg, "bullish")
	bearResult := evaluateBranch(input, cfg, "bearish")

	statusRank := func(r RSVEResult) int {
		switch r.Status {
		case "paper_trade_created":
			return 3
		case "stock_signal_passed":
			return 2
		case "option_blocked":
			return 1
		default:
			return 0
		}
	}
	br := statusRank(bullResult)
	ar := statusRank(bearResult)
	switch {
	case br > ar:
		return bullResult
	case ar > br:
		return bearResult
	case bullResult.AllPass && bearResult.AllPass:
		if bullResult.Score >= bearResult.Score {
			return bullResult
		}
		return bearResult
	default:
		if countFailures(bullResult.Gates) <= countFailures(bearResult.Gates) {
			return bullResult
		}
		return bearResult
	}
}

// ── Branch evaluation ─────────────────────────────────────────────────────────

func evaluateBranch(input RSVEInput, cfg RSVEConfig, direction string) RSVEResult {
	result := RSVEResult{
		Ticker:    input.Ticker,
		Date:      input.Date,
		Direction: direction,
		Status:    "rejected",
	}

	var bcfg RSVEBranchConfig
	if direction == "bullish" {
		bcfg = cfg.Bullish
	} else {
		bcfg = cfg.Bearish
	}

	f := computeRSVEFeatures(input)

	gates := make([]GateDiagnostic, 0, 13)
	blocked := false

	// ── Gate 1: VIX regime ────────────────────────────────────────────────────
	g1 := gateVIXRegime(input.VIX, bcfg.VIXMax)
	gates = appendGate(gates, &blocked, g1)

	// ── Gate 2: Market direction ──────────────────────────────────────────────
	g2 := gateMarketDirection(f, direction)
	gates = appendGate(gates, &blocked, g2)

	// ── Gate 3: Earnings blackout ─────────────────────────────────────────────
	g3 := gateNoEarnings(input.EarningsDaysAway, bcfg.EarningsBlackoutDays)
	gates = appendGate(gates, &blocked, g3)

	// ── Gate 4: Relative strength vs SPY ──────────────────────────────────────
	g4 := gateRelativeStrength(f, direction, bcfg.RSMinPct)
	gates = appendGate(gates, &blocked, g4)

	// ── Gate 5: EMA structure (EMA20 vs EMA50) ────────────────────────────────
	g5 := gateEMAStructure(f, direction)
	gates = appendGate(gates, &blocked, g5)

	// ── Gate 6: Volume expansion ──────────────────────────────────────────────
	g6 := gateVolumeExpansion(f, bcfg.VolumeRatioMin)
	gates = appendGate(gates, &blocked, g6)

	// ── Gate 7: Volatility compression (squeeze before expansion) ────────────
	g7 := gateVolSqueeze(f, bcfg.BBWidthPercentileMax)
	gates = appendGate(gates, &blocked, g7)

	// ── Structural pattern analysis (before breakout gate) ────────────────────
	// Runs after compression gates so we know the squeeze context. PatternState is
	// updated below after gate 8 result is known (forming vs breakout_confirmed).
	pa := AnalyzePatterns(input.Bars, direction, cfg.PatternAnalysis)

	// ── Gate 8: Breakout / breakdown trigger ──────────────────────────────────
	g8 := gateBreakoutTrigger(f, input.Bars, direction, bcfg.BreakoutLookback)
	gates = appendGate(gates, &blocked, g8)

	// ── Classify pattern_state using gate 8 result ────────────────────────────
	switch {
	case !pa.Detected:
		pa.PatternState = "no_pattern"
	case g8.Passed:
		pa.PatternState = "pattern_breakout_confirmed"
	default:
		pa.PatternState = "pattern_forming"
	}
	result.PatternAnalysis = pa

	if cfg.PatternAnalysis.Enabled && cfg.PatternAnalysis.RequiredForTrade {
		gp := gatePatternRequired(pa)
		gates = appendGate(gates, &blocked, gp)
	}

	// ── Stock gates blocked — return rejected ────────────────────────────────
	if blocked {
		result.Gates = gates
		for _, g := range gates {
			if g.Blocking {
				result.RejectGate = g.Name
				break
			}
		}
		return result
	}

	// ── Option data availability check ────────────────────────────────────────
	// When any option field is -1 (unavailable), the stock signal is confirmed but
	// no paper trade is created — option quality has not been verified.
	optDataMissing := input.IVRank < 0 || input.BidAskSpreadPct < 0 ||
		input.OpenInterest < 0 || input.OptionVolume < 0
	if optDataMissing {
		result.Gates = gates
		result.Status = "stock_signal_passed"
		result.Score = computeRSVERankScore(f, direction, pa, cfg.PatternAnalysis, input, cfg)
		return result
	}

	// ── Gate 9: IV rank ───────────────────────────────────────────────────────
	optBlocked := false
	g9 := gateIVRank(input.IVRank, cfg.Options.MaxIVRank)
	gates = appendGate(gates, &optBlocked, g9)

	// ── Gate 10: Bid-ask spread ───────────────────────────────────────────────
	g10 := gateBidAskSpread(input.BidAskSpreadPct, cfg.Options.MaxSpreadPct)
	gates = appendGate(gates, &optBlocked, g10)

	// ── Gate 11: Open interest minimum ───────────────────────────────────────
	g11 := gateOpenInterest(input.OpenInterest, cfg.Options.MinOpenInterest)
	gates = appendGate(gates, &optBlocked, g11)

	// ── Gate 12: Option volume minimum ───────────────────────────────────────
	g12 := gateOptionVolume(input.OptionVolume, cfg.Options.MinOptionVolume)
	gates = appendGate(gates, &optBlocked, g12)

	result.Gates = gates

	if optBlocked {
		for _, g := range gates {
			if g.Blocking {
				result.RejectGate = g.Name
				break
			}
		}
		result.Status = "option_blocked"
		return result
	}

	result.AllPass = true
	result.Status = "paper_trade_created"
	result.Score = computeRSVERankScore(f, direction, pa, cfg.PatternAnalysis, input, cfg)
	return result
}

// appendGate adds a gate to the slice, setting Blocking=true on the first failure.
func appendGate(gates []GateDiagnostic, blocked *bool, g GateDiagnostic) []GateDiagnostic {
	if !*blocked && !g.Passed {
		g.Blocking = true
		*blocked = true
	}
	return append(gates, g)
}

// ── Individual gate implementations ──────────────────────────────────────────

func gateVIXRegime(vix, maxVIX float64) GateDiagnostic {
	return GateDiagnostic{
		Name:        "vix_regime",
		Passed:      vix < maxVIX,
		ActualValue: fmt.Sprintf("%.1f", vix),
		Threshold:   fmt.Sprintf("< %.0f", maxVIX),
		DataSource:  "FRED VIXCLS",
	}
}

func gateMarketDirection(f rsveFeatures, direction string) GateDiagnostic {
	var passed bool
	var actual, threshold, name string
	if direction == "bullish" {
		passed = f.spyHasEMA50 && f.spyClose > f.spyEMA50
		actual = fmt.Sprintf("SPY %.2f vs EMA50 %.2f", f.spyClose, f.spyEMA50)
		threshold = "SPY close > EMA50"
		name = "market_uptrend"
	} else {
		passed = f.spyHasEMA20 && f.spyClose < f.spyEMA20
		actual = fmt.Sprintf("SPY %.2f vs EMA20 %.2f", f.spyClose, f.spyEMA20)
		threshold = "SPY close < EMA20"
		name = "market_downtrend"
	}
	return GateDiagnostic{Name: name, Passed: passed, ActualValue: actual, Threshold: threshold, DataSource: "Alpaca daily (SPY)"}
}

func gateNoEarnings(earningsDays, blackout int) GateDiagnostic {
	passed := earningsDays < 0 || earningsDays > blackout
	actual := "unknown"
	if earningsDays >= 0 {
		actual = fmt.Sprintf("%d days", earningsDays)
	}
	return GateDiagnostic{
		Name:        "no_earnings",
		Passed:      passed,
		ActualValue: actual,
		Threshold:   fmt.Sprintf("> %d days", blackout),
		DataSource:  "earnings calendar",
	}
}

func gateRelativeStrength(f rsveFeatures, direction string, minPct float64) GateDiagnostic {
	rs := f.relStrength20d
	var passed bool
	var actual, name, threshold string
	if !f.hasRelStrength {
		actual = "unavailable"
	} else {
		actual = fmt.Sprintf("RS=%.1f%%", rs)
	}
	if direction == "bullish" {
		passed = f.hasRelStrength && rs >= minPct
		name = "relative_strength"
		threshold = fmt.Sprintf(">= +%.1f%% vs SPY", minPct)
	} else {
		passed = f.hasRelStrength && rs <= -minPct
		name = "relative_weakness"
		threshold = fmt.Sprintf("<= -%.1f%% vs SPY", minPct)
	}
	return GateDiagnostic{Name: name, Passed: passed, ActualValue: actual, Threshold: threshold, DataSource: "Alpaca 20d returns"}
}

func gateEMAStructure(f rsveFeatures, direction string) GateDiagnostic {
	var passed bool
	var actual, name, threshold string
	actual = fmt.Sprintf("EMA20=%.2f EMA50=%.2f", f.ema20, f.ema50)
	if direction == "bullish" {
		passed = f.hasEMA20 && f.hasEMA50 && f.ema20 > f.ema50
		name = "ema_structure_bullish"
		threshold = "EMA20 > EMA50"
	} else {
		passed = f.hasEMA20 && f.hasEMA50 && f.ema20 < f.ema50
		name = "ema_structure_bearish"
		threshold = "EMA20 < EMA50"
	}
	return GateDiagnostic{Name: name, Passed: passed, ActualValue: actual, Threshold: threshold, DataSource: "Alpaca daily bars"}
}

func gateVolumeExpansion(f rsveFeatures, minRVOL float64) GateDiagnostic {
	actual := "unavailable"
	if f.hasVolumeRatio {
		actual = fmt.Sprintf("%.2fx", f.volumeRatio)
	}
	return GateDiagnostic{
		Name:        "volume_expansion",
		Passed:      f.hasVolumeRatio && f.volumeRatio >= minRVOL,
		ActualValue: actual,
		Threshold:   fmt.Sprintf(">= %.1fx avg", minRVOL),
		DataSource:  "Alpaca daily bars (20d avg vol)",
	}
}

func gateVolSqueeze(f rsveFeatures, maxPercentile float64) GateDiagnostic {
	actual := "unavailable"
	if f.hasBBWidthPct {
		actual = fmt.Sprintf("%.0f%%ile", f.bbWidthPercentile*100)
	}
	return GateDiagnostic{
		Name:        "vol_squeeze",
		Passed:      f.hasBBWidthPct && f.bbWidthPercentile <= maxPercentile,
		ActualValue: actual,
		Threshold:   fmt.Sprintf("<= %.0f%%ile of 63d range", maxPercentile*100),
		DataSource:  "Bollinger width (20-bar, 2σ) vs 63d history",
	}
}

func gateBreakoutTrigger(f rsveFeatures, bars []indicators.Bar, direction string, lookback int) GateDiagnostic {
	name := breakoutGateName(direction)
	n := len(bars)
	if n < lookback+1 {
		return GateDiagnostic{Name: name, Passed: false, ActualValue: "insufficient bars",
			Threshold: fmt.Sprintf("%s prior %d-bar pivot", direction, lookback), DataSource: "Alpaca daily bars"}
	}
	priorBars := bars[:n-1]
	if direction == "bullish" {
		pivot := indicators.HighestHigh(priorBars, lookback)
		passed := f.close > pivot
		return GateDiagnostic{Name: name, Passed: passed,
			ActualValue: fmt.Sprintf("close=%.2f pivot=%.2f", f.close, pivot),
			Threshold:   fmt.Sprintf("close > %.2f (%d-bar high)", pivot, lookback),
			DataSource:  "Alpaca daily bars"}
	}
	pivot := indicators.LowestLow(priorBars, lookback)
	passed := f.close < pivot
	return GateDiagnostic{Name: name, Passed: passed,
		ActualValue: fmt.Sprintf("close=%.2f pivot=%.2f", f.close, pivot),
		Threshold:   fmt.Sprintf("close < %.2f (%d-bar low)", pivot, lookback),
		DataSource:  "Alpaca daily bars"}
}

func breakoutGateName(direction string) string {
	if direction == "bullish" {
		return "breakout_trigger"
	}
	return "breakdown_trigger"
}

// gatePatternRequired is added when required_for_trade=true and no qualifying
// pattern was detected. It is never emitted when required_for_trade=false.
func gatePatternRequired(pa PatternAnalysis) GateDiagnostic {
	if pa.Detected {
		name := "no pattern"
		if pa.BestPattern != nil {
			name = pa.BestPattern.PatternName
		}
		return GateDiagnostic{
			Name:        "pattern_required",
			Passed:      true,
			ActualValue: fmt.Sprintf("%s q=%.2f", name, pa.BestPattern.QualityScore),
			Threshold:   "pattern detected",
			DataSource:  "structural pattern detector",
		}
	}
	return GateDiagnostic{
		Name:        "pattern_required",
		Passed:      false,
		ActualValue: "no_pattern_detected",
		Threshold:   "at least one structural pattern required",
		DataSource:  "structural pattern detector",
	}
}

// gateIVRank rejects when IV rank > maxIVRank. IV rank <= maxIVRank passes.
// -1 = unavailable → pass silently.
func gateIVRank(ivRank, maxIVRank float64) GateDiagnostic {
	if ivRank < 0 {
		return GateDiagnostic{Name: "iv_rank_ok", Passed: true, ActualValue: "unavailable (skipped)",
			Threshold: fmt.Sprintf("<= %.0f", maxIVRank), DataSource: "options chain IV rank"}
	}
	return GateDiagnostic{
		Name:        "iv_rank_ok",
		Passed:      ivRank <= maxIVRank,
		ActualValue: fmt.Sprintf("%.0f", ivRank),
		Threshold:   fmt.Sprintf("<= %.0f", maxIVRank),
		DataSource:  "options chain IV rank",
	}
}

func gateBidAskSpread(spreadPct, maxSpreadPct float64) GateDiagnostic {
	if spreadPct < 0 {
		return GateDiagnostic{Name: "spread_quality", Passed: true, ActualValue: "unavailable (skipped)",
			Threshold: fmt.Sprintf("<= %.0f%% of mid", maxSpreadPct), DataSource: "options chain bid/ask"}
	}
	return GateDiagnostic{
		Name:        "spread_quality",
		Passed:      spreadPct <= maxSpreadPct,
		ActualValue: fmt.Sprintf("%.1f%% of mid", spreadPct),
		Threshold:   fmt.Sprintf("<= %.0f%% of mid", maxSpreadPct),
		DataSource:  "options chain bid/ask",
	}
}

func gateOpenInterest(oi, minOI int) GateDiagnostic {
	if oi < 0 {
		return GateDiagnostic{Name: "oi_minimum", Passed: true, ActualValue: "unavailable (skipped)",
			Threshold: fmt.Sprintf(">= %d", minOI), DataSource: "options chain OI"}
	}
	return GateDiagnostic{
		Name:        "oi_minimum",
		Passed:      oi >= minOI,
		ActualValue: fmt.Sprintf("%d", oi),
		Threshold:   fmt.Sprintf(">= %d", minOI),
		DataSource:  "options chain OI",
	}
}

// gateOptionVolume rejects when daily option volume < minVolume.
// -1 = unavailable → pass silently.
func gateOptionVolume(vol, minVol int) GateDiagnostic {
	if vol < 0 {
		return GateDiagnostic{Name: "option_volume", Passed: true, ActualValue: "unavailable (skipped)",
			Threshold: fmt.Sprintf(">= %d contracts/day", minVol), DataSource: "options chain daily volume"}
	}
	return GateDiagnostic{
		Name:        "option_volume",
		Passed:      vol >= minVol,
		ActualValue: fmt.Sprintf("%d", vol),
		Threshold:   fmt.Sprintf(">= %d contracts/day", minVol),
		DataSource:  "options chain daily volume",
	}
}

// ── Ranking score (only computed when AllPass=true) ───────────────────────────

// computeRSVERankScore returns a 0-100 score for ranking candidates.
// It is NOT a gate — it never decides tradability.
//
// Components (PRO weights):
//   - Relative strength magnitude:  0-25 pts
//   - Volatility compression (BB):  0-20 pts
//   - Breakout strength:            0-20 pts
//   - Volume expansion:             0-15 pts
//   - Pattern quality:              0-15 pts
//   - Options liquidity:            0-5  pts (only when all option data present)
func computeRSVERankScore(f rsveFeatures, direction string, pa PatternAnalysis, pcfg PatternAnalysisConfig, input RSVEInput, cfg RSVEConfig) float64 {
	score := 0.0

	// Relative strength (0-25)
	if f.hasRelStrength {
		rs := f.relStrength20d
		if direction == "bearish" {
			rs = -rs
		}
		switch {
		case rs >= 10.0:
			score += 25
		case rs >= 6.0:
			score += 15 + (rs-6.0)/4.0*10
		case rs >= 2.0:
			score += (rs - 2.0) / 4.0 * 15
		}
	}

	// Volatility compression (0-20): BB width percentile — lower = tighter = better
	if f.hasBBWidthPct {
		score += math.Max(0, (0.30-f.bbWidthPercentile)/0.30) * 20
	}

	// Breakout strength (0-20): how far close exceeds the breakout pivot
	{
		n := len(input.Bars)
		lb := cfg.Bullish.BreakoutLookback
		if direction == "bearish" {
			lb = cfg.Bearish.BreakoutLookback
		}
		if n > lb+1 {
			priorBars := input.Bars[:n-1]
			if direction == "bullish" {
				pivot := indicators.HighestHigh(priorBars, lb)
				if pivot > 0 {
					pct := (f.close - pivot) / pivot * 100
					switch {
					case pct >= 3.0:
						score += 20
					case pct >= 1.0:
						score += (pct - 1.0) / 2.0 * 20
					}
				}
			} else {
				pivot := indicators.LowestLow(priorBars, lb)
				if pivot > 0 {
					pct := (pivot - f.close) / pivot * 100
					switch {
					case pct >= 3.0:
						score += 20
					case pct >= 1.0:
						score += (pct - 1.0) / 2.0 * 20
					}
				}
			}
		}
	}

	// Volume expansion (0-15)
	if f.hasVolumeRatio {
		switch {
		case f.volumeRatio >= 3.0:
			score += 15
		case f.volumeRatio >= 2.0:
			score += 8 + (f.volumeRatio-2.0)/1.0*7
		case f.volumeRatio >= 1.2:
			score += (f.volumeRatio - 1.2) / 0.8 * 8
		}
	}

	// Pattern quality (0-15)
	if pcfg.Enabled && pa.Detected && pa.BestPattern != nil {
		weight := pcfg.PatternScoreWeight
		if weight <= 0 {
			weight = 15.0
		}
		score += pa.BestPattern.QualityScore * weight
	}

	// Options liquidity (0-5): only when all option data is present
	if input.IVRank >= 0 && input.BidAskSpreadPct >= 0 && input.OpenInterest >= 0 && input.OptionVolume >= 0 {
		liq := 0.0
		if input.IVRank <= cfg.Options.MaxIVRank && cfg.Options.MaxIVRank > 0 {
			liq += (cfg.Options.MaxIVRank - input.IVRank) / cfg.Options.MaxIVRank * 2
		}
		if input.BidAskSpreadPct <= cfg.Options.MaxSpreadPct && cfg.Options.MaxSpreadPct > 0 {
			liq += (cfg.Options.MaxSpreadPct - input.BidAskSpreadPct) / cfg.Options.MaxSpreadPct * 2
		}
		if cfg.Options.MinOptionVolume > 0 && input.OptionVolume >= cfg.Options.MinOptionVolume*2 {
			liq++
		}
		score += math.Min(liq, 5)
	}

	return math.Round(math.Min(score, 100)*10) / 10
}

// ── Internal feature struct ───────────────────────────────────────────────────

type rsveFeatures struct {
	close float64

	ema20    float64
	ema50    float64
	hasEMA20 bool
	hasEMA50 bool

	volumeRatio    float64
	hasVolumeRatio bool

	bbWidthPercentile float64
	hasBBWidthPct     bool

	relStrength20d float64
	hasRelStrength bool

	spyClose    float64
	spyEMA20    float64
	spyEMA50    float64
	spyHasEMA20 bool
	spyHasEMA50 bool
}

func computeRSVEFeatures(input RSVEInput) rsveFeatures {
	f := rsveFeatures{}
	bars := input.Bars
	n := len(bars)
	if n == 0 {
		return f
	}

	closes := indicators.Closes(bars)
	volumes := indicators.Volumes(bars)
	f.close = closes[n-1]

	if ema20, ok := indicators.EMALast(closes, 20); ok {
		f.ema20, f.hasEMA20 = ema20, true
	}
	if ema50, ok := indicators.EMALast(closes, 50); ok {
		f.ema50, f.hasEMA50 = ema50, true
	}
	if vr, ok := indicators.VolumeRatioLast(volumes, 20); ok {
		f.volumeRatio, f.hasVolumeRatio = vr, true
	}
	if bbPct, ok := indicators.BollingerWidthPercentile(closes, 20, 63, 2.0); ok {
		f.bbWidthPercentile, f.hasBBWidthPct = bbPct, true
	}
	if len(input.SPYBars) >= 21 {
		spyCloses := indicators.Closes(input.SPYBars)
		if rs, ok := indicators.RelativeStrength(closes, spyCloses); ok {
			f.relStrength20d, f.hasRelStrength = rs, true
		}
	}
	if len(input.SPYBars) > 0 {
		spyCloses := indicators.Closes(input.SPYBars)
		f.spyClose = spyCloses[len(spyCloses)-1]
		if ema, ok := indicators.EMALast(spyCloses, 20); ok {
			f.spyEMA20, f.spyHasEMA20 = ema, true
		}
		if ema, ok := indicators.EMALast(spyCloses, 50); ok {
			f.spyEMA50, f.spyHasEMA50 = ema, true
		}
	}
	return f
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func countFailures(gates []GateDiagnostic) int {
	n := 0
	for _, g := range gates {
		if !g.Passed {
			n++
		}
	}
	return n
}
