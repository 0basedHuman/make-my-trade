// internal/strategy/engine.go
//
// WHAT: App-side preprocessing for the options decision engine.
//       Classifies each symbol against the 4 setup families defined in
//       strategy_rules.yaml, applies all hard gates from the YAML, scores
//       patterns using integer YAML values, and produces structure-based
//       price targets (ATR + swing high/low) rather than arbitrary percents.
//
// WHY:  The application must handle all deterministic preprocessing before
//       Claude. This layer:
//         1. Enforces hard regime gates (VIX, BTC ROC) from YAML
//         2. Enforces hard qualifiers (volume, earnings blackout) from YAML
//         3. Classifies each symbol into one of the 4 setup families
//         4. Computes integer pattern scores from YAML pattern_scores keys
//         5. Checks anti-patterns from YAML anti_patterns lists
//         6. Computes structure-based targets (ATR range + nearest swing levels)
//         7. Sets CandidateStatus, ReasonCodes, SetupFamily on SymbolAnalysis
//
// HOW:  Engine.Analyze() is called once per symbol per scan.
//       It returns SymbolAnalysis with Eligible=true only when a symbol
//       passes all hard gates AND matches at least one setup family with a
//       sufficient pattern score. Only eligible symbols are sent to Claude.
//
// WHAT BREAKS: If Rules is nil (no YAML loaded), DefaultRules() is used.
//              If bars < 35, all indicators fail and symbol is ineligible.
//              If VIX >= 24 (YAML hard limit), ALL symbols are ineligible —
//              the scan still runs but nothing is sent to Claude.
//
// VERIFY: For a bullish symbol on a normal VIX day, Analyze() should return
//         Eligible=true, SetupFamily="bullish_continuation" or
//         "bullish_momentum_breakout", PatternScoreInt >= 2.

package strategy

import (
	"fmt"
	"strings"

	"github.com/yourname/makemytrade/internal/indicators"
	"github.com/yourname/makemytrade/internal/market"
)

// Regime holds market-wide macro context passed in from the handler.
type Regime struct {
	VIX      float64
	BTCROC20 float64
	VIXDate  string
}

// GateResult holds a single indicator check result (kept for DB + UI compat).
type GateResult struct {
	Passed bool
	Reason string
	Value  float64
}

// SymbolAnalysis is the complete preprocessing output for one symbol.
// It feeds directly into the RuntimePayload sent to Claude.
type SymbolAnalysis struct {
	Ticker string
	Date   string

	// Eligibility — only eligible symbols are sent to Claude.
	// Eligible = sufficient data + regime gates pass + at least one family matches.
	Eligible     bool
	ScreenReason string

	// ── Computed indicators ────────────────────────────────────────────────────
	ClosePrice  float64
	EMA20       float64
	EMA50       float64
	EMA100      float64 // computed for context; NOT a hard gate for momentum families
	RSI14       float64
	MACDHist    float64
	VolumeRatio float64

	// ── Trend classification ───────────────────────────────────────────────────
	TrendBias   string // "bullish" | "bearish" | "mixed"
	TrendReason string

	// ── Price levels ───────────────────────────────────────────────────────────
	PriorDayHigh float64
	PriorDayLow  float64

	// ── Setup family classification (NEW) ─────────────────────────────────────
	// SetupFamily is the best-matching family for this symbol.
	// MatchedFamilies lists all families that passed structural + entry rules.
	SetupFamily     string
	MatchedFamilies []string

	// CandidateStatus is set by the engine from YAML family rules:
	//   "rejected"             — no family matched or hard gate failed
	//   "structural_candidate" — family matched, pattern score >= min
	//   "entry_ready"          — family matched + entry rules pass
	// "watch_only" and "confirmed" may be assigned by Claude.
	CandidateStatus string

	// ReasonCodes are the YAML reason_codes explaining the classification.
	ReasonCodes []string

	// ── Pattern scoring (NEW: integer from YAML pattern_scores) ───────────────
	PatternName    string
	PatternScoreInt int    // integer score from YAML pattern_scores (primary)
	PatternScore   float64 // float64 kept for DB column compat = float64(PatternScoreInt)

	// ── Anti-pattern flags ────────────────────────────────────────────────────
	AntiPatterns   []string
	RejectedByAnti bool // still set when anti-patterns present (UI display)

	// ── Structure-based targets (NEW: from ATR + swing levels) ────────────────
	// Replaces arbitrary percent targets. Target1 = base, Target2 = stretch.
	BaseTarget   float64
	StretchTarget float64
	EntryLow     float64
	EntryHigh    float64
	StopLoss     float64
	Target1      float64 // = BaseTarget (DB compat)
	Target2      float64 // = StretchTarget (DB compat)
	RRRatio      float64

	// ── Hold windows from YAML target_model ───────────────────────────────────
	HoldDaysMin  int
	HoldDaysBase int
	HoldDaysMax  int

	// ── Gate fields (informational context, kept for DB + UI) ─────────────────
	GateTrend    GateResult
	GateMomentum GateResult
	GateVolume   GateResult
	GateVIX      GateResult
	GateBTC      GateResult
	GateRSI      GateResult

	// AllGatesPassed = Eligible (DB compat alias)
	AllGatesPassed bool
	RejectReason   string

	// ── News / sentiment ──────────────────────────────────────────────────────
	EarningsRisk   bool
	SentimentScore float64
	NewsSentiment  float64
	Sentiment      string

	// ── Relative strength vs SPY ──────────────────────────────────────────────
	RelativeStrength float64
}

// Config holds data-quality thresholds (not strategy rules — those are in Rules).
type Config struct {
	MinBarsRequired int
}

// DefaultConfig returns the data-quality config for the options engine.
func DefaultConfig() Config {
	return Config{
		MinBarsRequired: 35, // minimum bars for MACD(12,26,9) + RSI(14) + ATR(14)
	}
}

// Engine runs app-side preprocessing for options candidates.
type Engine struct {
	cfg   Config
	rules *Rules
}

// NewEngine creates an Engine. If rules is nil, DefaultRules() is used.
func NewEngine(cfg Config, rules *Rules) *Engine {
	if rules == nil {
		rules = DefaultRules()
	}
	return &Engine{cfg: cfg, rules: rules}
}

// Analyze runs preprocessing for one symbol and returns a SymbolAnalysis.
// bars must be sorted oldest-first. spyBars may be nil.
func (e *Engine) Analyze(
	ticker string,
	date string,
	bars []indicators.Bar,
	regime Regime,
	spyBars []indicators.Bar,
	earningsRisk bool,
	sentimentData market.SentimentData,
) SymbolAnalysis {

	a := SymbolAnalysis{
		Ticker:       ticker,
		Date:         date,
		EarningsRisk: earningsRisk,
	}

	// ── 1. Data quality gate ─────────────────────────────────────────────────
	if len(bars) < e.cfg.MinBarsRequired {
		a.Eligible = false
		a.ScreenReason = fmt.Sprintf("insufficient_data: only %d bars (need %d+)", len(bars), e.cfg.MinBarsRequired)
		a.CandidateStatus = "rejected"
		a.AllGatesPassed = false
		a.RejectReason = a.ScreenReason
		return a
	}

	closes := indicators.Closes(bars)
	volumes := indicators.Volumes(bars)
	n := len(bars)
	a.ClosePrice = closes[n-1]

	if n >= 2 {
		a.PriorDayHigh = bars[n-2].High
		a.PriorDayLow = bars[n-2].Low
	}

	// ── 2. Compute indicators ────────────────────────────────────────────────
	ema20, ok20 := indicators.EMALast(closes, 20)
	a.EMA20 = ema20

	ema50, ok50 := indicators.EMALast(closes, 50)
	a.EMA50 = ema50

	ema100, _ := indicators.EMALast(closes, 100)
	a.EMA100 = ema100

	macd, macdOK := indicators.MACDLast(closes)
	if macdOK {
		a.MACDHist = macd.Histogram
	}

	rsi, rsiOK := indicators.RSILast(closes, 14)
	if rsiOK {
		a.RSI14 = rsi
	}

	volRatio, volOK := indicators.VolumeRatioLast(volumes, 20)
	if volOK {
		a.VolumeRatio = volRatio
	}

	// EMA slope for momentum families (5-bar lookback)
	ema20Slope, _ := indicators.EMASlope(closes, 20, 5)

	// ── 3. Trend classification ──────────────────────────────────────────────
	if ok20 && ok50 {
		if ema20 > ema50 && closes[n-1] > ema20 {
			a.TrendBias = "bullish"
			a.TrendReason = fmt.Sprintf("EMA20 (%.2f) > EMA50 (%.2f), close above EMA20", ema20, ema50)
		} else if ema20 < ema50 && closes[n-1] < ema20 {
			a.TrendBias = "bearish"
			a.TrendReason = fmt.Sprintf("EMA20 (%.2f) < EMA50 (%.2f), close below EMA20", ema20, ema50)
		} else {
			a.TrendBias = "mixed"
			a.TrendReason = fmt.Sprintf("EMA20 (%.2f) vs EMA50 (%.2f) — no clear alignment", ema20, ema50)
		}
	} else {
		a.TrendBias = "mixed"
		a.TrendReason = "insufficient data for EMA20/EMA50"
	}

	// ── 4. Relative strength vs SPY ──────────────────────────────────────────
	if len(spyBars) >= 21 {
		spyCloses := indicators.Closes(spyBars)
		rs, rsOK := indicators.RelativeStrength(closes, spyCloses)
		if rsOK {
			a.RelativeStrength = rs
		}
	}

	// ── 5. Sentiment ─────────────────────────────────────────────────────────
	a.SentimentScore = sentimentData.Score
	a.NewsSentiment = sentimentData.Score
	switch {
	case sentimentData.Score > 0.2:
		a.Sentiment = "positive"
	case sentimentData.Score < -0.2:
		a.Sentiment = "negative"
	default:
		a.Sentiment = "neutral"
	}

	// ── 6. Informational gate fields (for DB + UI) ────────────────────────────
	r := e.rules

	if ok20 && ok50 {
		tp := ema20 > ema50
		tr := ""
		if !tp {
			tr = fmt.Sprintf("EMA20 (%.2f) < EMA50 (%.2f)", ema20, ema50)
		}
		a.GateTrend = GateResult{Passed: tp, Reason: tr, Value: ema20}
	} else {
		a.GateTrend = GateResult{Passed: false, Reason: "insufficient data"}
	}

	if macdOK {
		mp := macd.Histogram > 0
		mr := ""
		if !mp {
			mr = fmt.Sprintf("MACD hist (%.4f) <= 0", macd.Histogram)
		}
		a.GateMomentum = GateResult{Passed: mp, Reason: mr, Value: macd.Histogram}
	} else {
		a.GateMomentum = GateResult{Passed: false, Reason: "insufficient data"}
	}

	if volOK {
		vp := volRatio >= r.HardQualifiers.Common.VolumeRatioMin
		vr := ""
		if !vp {
			vr = fmt.Sprintf("volume ratio (%.2fx) < %.1f", volRatio, r.HardQualifiers.Common.VolumeRatioMin)
		}
		a.GateVolume = GateResult{Passed: vp, Reason: vr, Value: volRatio}
	} else {
		a.GateVolume = GateResult{Passed: false, Reason: "insufficient data"}
	}

	vixMax := r.MarketRegime.HardRules.VIXMax
	vixPass := regime.VIX < vixMax
	vixReason := ""
	if !vixPass {
		vixReason = fmt.Sprintf("VIX (%.1f) >= %.0f — regime hard gate", regime.VIX, vixMax)
	}
	a.GateVIX = GateResult{Passed: vixPass, Reason: vixReason, Value: regime.VIX}

	btcPass := regime.BTCROC20 >= r.MarketRegime.HardRules.BTCRoc20Min
	btcReason := ""
	if !btcPass {
		btcReason = fmt.Sprintf("BTC 20d ROC (%.1f%%) < 0 — risk-off proxy", regime.BTCROC20)
	}
	a.GateBTC = GateResult{Passed: btcPass, Reason: btcReason, Value: regime.BTCROC20}

	if rsiOK {
		rp := rsi >= 30 && rsi <= 80
		rr := ""
		if rsi < 30 {
			rr = fmt.Sprintf("RSI (%.1f) < 30 — oversold", rsi)
		} else if rsi > 80 {
			rr = fmt.Sprintf("RSI (%.1f) > 80 — overbought", rsi)
		}
		a.GateRSI = GateResult{Passed: rp, Reason: rr, Value: rsi}
	} else {
		a.GateRSI = GateResult{Passed: false, Reason: "insufficient data"}
	}

	// ── 7. Regime hard gates ──────────────────────────────────────────────────
	// VIX >= vix_max blocks ALL families — no new entries possible.
	if !vixPass {
		a.Eligible = false
		a.ScreenReason = vixReason
		a.CandidateStatus = "rejected"
		a.ReasonCodes = append(a.ReasonCodes, "vix_too_high")
		a.AllGatesPassed = false
		a.RejectReason = a.ScreenReason
		return a
	}

	// ── 8. Hard qualifiers — volume ───────────────────────────────────────────
	// volume_ratio_min from hard_qualifiers.common applies to all families.
	volMin := r.HardQualifiers.Common.VolumeRatioMin
	if volOK && volRatio < volMin {
		a.Eligible = false
		a.ScreenReason = fmt.Sprintf("volume_ratio (%.2fx) < YAML min (%.1fx) — option chain likely illiquid", volRatio, volMin)
		a.CandidateStatus = "rejected"
		a.ReasonCodes = append(a.ReasonCodes, "volume_weak")
		a.AllGatesPassed = false
		a.RejectReason = a.ScreenReason
		return a
	}

	// ── 9. Earnings blackout ─────────────────────────────────────────────────
	// earningsRisk is set by the caller (handler/activity) via market.HasEarningsWithin.
	// We do NOT hard-reject here — family matching still runs so the symbol
	// appears in the structural_candidate bucket. The blocked_by_event override
	// is applied after family matching and status assignment (see below).
	// This ensures the reason code is only added when a family matched.

	// ── 10. Pattern scoring ───────────────────────────────────────────────────
	patternsByName := scorePatterns(bars, a.RelativeStrength, a.TrendBias)
	a.AntiPatterns = detectAntiPatterns(bars, r)

	if len(a.AntiPatterns) > 0 {
		a.RejectedByAnti = true
		a.ReasonCodes = append(a.ReasonCodes, "anti_pattern_detected")
	}

	// ── 11. Try each setup family ─────────────────────────────────────────────
	type familyResult struct {
		name             string
		patternScore     int
		entryReady       bool
		entryFailReasons []string // populated when entryReady == false
		baseTarget       float64
		stretchTarget    float64
		holdMin, holdBase, holdMax int
	}

	var matches []familyResult

	// bullish_continuation
	if ok20 && ok50 && ema20 > ema100 && a.ClosePrice > ema20 && macdOK && macd.Histogram > 0 && btcPass {
		er := r.SetupFamilies["bullish_continuation"].EntryRules
		extPct := (a.ClosePrice-ema20)/ema20*100
		score := sumPatternScore(patternsByName, r.PatternScoreConfig.Bullish)
		scoreMin := r.SetupFamilies["bullish_continuation"].PatternScoreMin
		if score >= scoreMin {
			entryReady := volRatio >= er.VolumeRatioMin &&
				rsiOK && rsi >= er.RSIMin && rsi <= er.RSIMax &&
				extPct <= er.EntryExtensionMaxPct
			failR := entryFailReasons(volRatio, er.VolumeRatioMin, rsiOK, rsi, er.RSIMin, er.RSIMax, extPct, er.EntryExtensionMaxPct)
			b, s, tok := indicators.ATRTargetRange(bars, 14, a.ClosePrice, "bullish")
			if !tok {
				b = a.ClosePrice * 1.04
				s = a.ClosePrice * 1.07
			}
			// Prefer nearest resistance over ATR if available
			if nr := indicators.NearestResistance(bars, a.ClosePrice, 30); nr > 0 {
				b = nr
			}
			hw := r.TargetModelConfig.BullishContinuation.HoldWindowDays
			matches = append(matches, familyResult{"bullish_continuation", score, entryReady, failR, b, s, hw.Min, hw.Base, hw.Max})
		}
	}

	// bullish_momentum_breakout
	if ok20 && a.ClosePrice > ema20 && ema20Slope > 0 && btcPass {
		er := r.SetupFamilies["bullish_momentum_breakout"].EntryRules
		extPct := (a.ClosePrice-ema20)/ema20*100
		score := sumPatternScore(patternsByName, r.PatternScoreConfig.Bullish)
		// Add breakout bonus if price > recent resistance
		if indicators.NearestResistance(bars, a.ClosePrice*0.995, 20) == 0 {
			// price is breaking above prior resistance range — add breakout score
			if _, ok := r.PatternScoreConfig.Bullish["volatility_contraction_breakout"]; ok {
				score += r.PatternScoreConfig.Bullish["volatility_contraction_breakout"]
			}
		}
		scoreMin := r.SetupFamilies["bullish_momentum_breakout"].PatternScoreMin
		if score >= scoreMin {
			const momentumVolMin = 1.5 // strong_volume_expansion
			entryReady := extPct <= er.EntryExtensionMaxPct &&
				rsiOK && rsi >= er.RSIMin && rsi <= er.RSIMax &&
				volRatio >= momentumVolMin
			failR := entryFailReasons(volRatio, momentumVolMin, rsiOK, rsi, er.RSIMin, er.RSIMax, extPct, er.EntryExtensionMaxPct)
			b, s, tok := indicators.ATRTargetRange(bars, 14, a.ClosePrice, "bullish")
			if !tok {
				b = a.ClosePrice * 1.05
				s = a.ClosePrice * 1.10
			}
			hw := r.TargetModelConfig.BullishMomentumBreakout.HoldWindowDays
			matches = append(matches, familyResult{"bullish_momentum_breakout", score, entryReady, failR, b, s, hw.Min, hw.Base, hw.Max})
		}
	}

	// bearish_continuation
	if ok20 && ok50 && ema20 < ema100 && a.ClosePrice < ema20 && macdOK && macd.Histogram < 0 {
		er := r.SetupFamilies["bearish_continuation"].EntryRules
		extPct := (ema20-a.ClosePrice)/ema20*100
		score := sumPatternScore(patternsByName, r.PatternScoreConfig.Bearish)
		scoreMin := r.SetupFamilies["bearish_continuation"].PatternScoreMin
		if score >= scoreMin {
			entryReady := volRatio >= er.VolumeRatioMin &&
				rsiOK && rsi >= er.RSIMin && rsi <= er.RSIMax &&
				extPct <= er.EntryExtensionMaxPct
			failR := entryFailReasons(volRatio, er.VolumeRatioMin, rsiOK, rsi, er.RSIMin, er.RSIMax, extPct, er.EntryExtensionMaxPct)
			b, s, tok := indicators.ATRTargetRange(bars, 14, a.ClosePrice, "bearish")
			if !tok {
				b = a.ClosePrice * 0.96
				s = a.ClosePrice * 0.93
			}
			if ns := indicators.NearestSupport(bars, a.ClosePrice, 30); ns > 0 {
				b = ns
			}
			hw := r.TargetModelConfig.BearishContinuation.HoldWindowDays
			matches = append(matches, familyResult{"bearish_continuation", score, entryReady, failR, b, s, hw.Min, hw.Base, hw.Max})
		}
	}

	// bearish_momentum_breakdown
	if ok20 && a.ClosePrice < ema20 && ema20Slope < 0 {
		er := r.SetupFamilies["bearish_momentum_breakdown"].EntryRules
		extPct := (ema20-a.ClosePrice)/ema20*100
		score := sumPatternScore(patternsByName, r.PatternScoreConfig.Bearish)
		// Add breakdown bonus if price < recent support
		if indicators.NearestSupport(bars, a.ClosePrice*1.005, 20) == 0 {
			if _, ok := r.PatternScoreConfig.Bearish["volatility_contraction_breakdown"]; ok {
				score += r.PatternScoreConfig.Bearish["volatility_contraction_breakdown"]
			}
		}
		scoreMin := r.SetupFamilies["bearish_momentum_breakdown"].PatternScoreMin
		if score >= scoreMin {
			const momentumVolMin = 1.5 // strong_volume_expansion
			entryReady := extPct <= er.EntryExtensionMaxPct &&
				rsiOK && rsi >= er.RSIMin && rsi <= er.RSIMax &&
				volRatio >= momentumVolMin
			failR := entryFailReasons(volRatio, momentumVolMin, rsiOK, rsi, er.RSIMin, er.RSIMax, extPct, er.EntryExtensionMaxPct)
			b, s, tok := indicators.ATRTargetRange(bars, 14, a.ClosePrice, "bearish")
			if !tok {
				b = a.ClosePrice * 0.95
				s = a.ClosePrice * 0.90
			}
			hw := r.TargetModelConfig.BearishMomentumBreakdown.HoldWindowDays
			matches = append(matches, familyResult{"bearish_momentum_breakdown", score, entryReady, failR, b, s, hw.Min, hw.Base, hw.Max})
		}
	}

	// ── 12. No family matched ─────────────────────────────────────────────────
	if len(matches) == 0 {
		a.Eligible = false
		a.CandidateStatus = "rejected"
		a.AllGatesPassed = false
		// Explain why no family matched
		if a.TrendBias == "mixed" {
			a.ReasonCodes = appendIfMissing(a.ReasonCodes, "trend_down")
		}
		if macdOK && macd.Histogram <= 0 {
			a.ReasonCodes = appendIfMissing(a.ReasonCodes, "macd_negative")
		}
		if rsiOK {
			if rsi > 72 {
				a.ReasonCodes = appendIfMissing(a.ReasonCodes, "rsi_extended")
			} else if rsi < 28 {
				a.ReasonCodes = appendIfMissing(a.ReasonCodes, "rsi_too_weak")
			}
		}
		if len(a.ReasonCodes) == 0 {
			a.ReasonCodes = append(a.ReasonCodes, "trend_down")
		}
		a.ScreenReason = strings.Join(a.ReasonCodes, ", ")
		a.RejectReason = a.ScreenReason
		a.PatternScore = 0
		return a
	}

	// ── 13. Pick the best matching family ────────────────────────────────────
	// Best = highest pattern score. Prefer continuation over momentum on tie.
	best := matches[0]
	for _, m := range matches[1:] {
		if m.patternScore > best.patternScore {
			best = m
		}
	}
	for _, m := range matches {
		a.MatchedFamilies = append(a.MatchedFamilies, m.name)
	}
	a.SetupFamily = best.name
	a.PatternScoreInt = best.patternScore
	a.PatternScore = float64(best.patternScore)
	a.BaseTarget = best.baseTarget
	a.StretchTarget = best.stretchTarget
	a.Target1 = best.baseTarget
	a.Target2 = best.stretchTarget
	a.HoldDaysMin = best.holdMin
	a.HoldDaysBase = best.holdBase
	a.HoldDaysMax = best.holdMax

	// Set candidate status.
	// entry_ready  = all pre-open entry rules passed → only waiting for the 6:30 AM bar.
	// structural_candidate = structure matches but at least one entry condition is not yet met.
	if best.entryReady {
		a.CandidateStatus = "entry_ready"
		a.ReasonCodes = appendIfMissing(a.ReasonCodes, "volume_strong")
	} else {
		a.CandidateStatus = "structural_candidate"
		// Add specific codes for what's blocking entry so the UI can explain it.
		for _, fr := range best.entryFailReasons {
			a.ReasonCodes = appendIfMissing(a.ReasonCodes, fr)
		}
	}

	// ── Event block override (YAML state_rules) ───────────────────────────────
	// blocked_by_event overrides BOTH entry_ready AND structural_candidate when
	// an earnings or binary-event blackout is active.  The symbol is marked
	// ineligible so it is never sent to Claude for options evaluation.
	if earningsRisk {
		blockedStatus := r.BlockedByEventStatus()
		if a.CandidateStatus == "entry_ready" && r.StateRules.BlockedByEventOverridesEntryReady {
			a.CandidateStatus = blockedStatus
			a.Eligible = false
			a.AllGatesPassed = false
			a.ReasonCodes = appendIfMissing(a.ReasonCodes, "event_blackout_earnings")
		}
		if a.CandidateStatus == "structural_candidate" {
			// structural_candidate + event blackout → blocked_by_event (not watchlist).
			a.CandidateStatus = blockedStatus
			a.Eligible = false
			a.AllGatesPassed = false
			a.ReasonCodes = appendIfMissing(a.ReasonCodes, "event_blackout_earnings")
		}
	}

	// Set pattern name from detected patterns (take highest scoring)
	a.PatternName = bestPatternName(patternsByName,
		isBullishFamily(best.name), r)

	// Populate entry zone and stop from ATR
	atr14, atrOK := indicators.ATRLast(bars, 14)
	if atrOK && atr14 > 0 {
		a.EntryLow = a.ClosePrice - 0.25*atr14
		a.EntryHigh = a.ClosePrice + 0.25*atr14
		if isBullishFamily(best.name) {
			a.StopLoss = a.ClosePrice - 1.5*atr14
		} else {
			a.StopLoss = a.ClosePrice + 1.5*atr14
		}
		// RR ratio
		if a.BaseTarget > 0 && a.StopLoss > 0 && a.ClosePrice > 0 {
			reward := absDiff(a.BaseTarget, a.ClosePrice)
			risk := absDiff(a.StopLoss, a.ClosePrice)
			if risk > 0 {
				a.RRRatio = reward / risk
			}
		}
	}

	// Add trend reason code
	if isBullishFamily(best.name) {
		a.ReasonCodes = appendIfMissing(a.ReasonCodes, "trend_up")
		a.ReasonCodes = appendIfMissing(a.ReasonCodes, "above_ema20")
	} else {
		a.ReasonCodes = appendIfMissing(a.ReasonCodes, "trend_down")
		a.ReasonCodes = appendIfMissing(a.ReasonCodes, "below_ema20")
	}
	if macdOK && macd.Histogram > 0 {
		a.ReasonCodes = appendIfMissing(a.ReasonCodes, "macd_positive")
	}

	// Eligible is true only if status is not blocked_by_event (which was set above).
	if a.CandidateStatus != "blocked_by_event" {
		a.Eligible = true
		a.AllGatesPassed = true
	}
	return a
}

// ── Pattern scoring helpers ────────────────────────────────────────────────────

// scorePatterns runs the available pattern detectors and returns a map of
// pattern_name → detected (true/false). The engine then sums integer scores
// from the YAML pattern_scores table.
func scorePatterns(bars []indicators.Bar, relStrength float64, trendBias string) map[string]bool {
	out := make(map[string]bool)

	// Patterns detectable with existing indicator functions
	if bf, _ := indicators.IsBullFlag(bars); bf {
		out["bull_flag"] = true
	}
	if tb, _ := indicators.IsTightBase(bars, 12); tb {
		out["tight_base"] = true
	}
	if vcb, _ := indicators.IsVCB(bars); vcb {
		out["volatility_contraction_breakout"] = true
		out["volatility_contraction_breakdown"] = true
	}
	if indicators.IsBearFlag(bars) {
		out["bear_flag"] = true
	}
	if indicators.IsHigherLowContinuation(bars, 20) {
		out["higher_low_continuation"] = true
	}
	if indicators.IsLowerHighBreakdown(bars, 20) {
		out["lower_high_breakdown"] = true
	}
	// Relative strength as a pattern signal
	if relStrength > 5.0 {
		out["relative_strength_bullish"] = true
	}
	if relStrength < -5.0 {
		out["relative_weakness_bearish"] = true
	}
	// flat_base = same as tight_base for scoring purposes
	if out["tight_base"] {
		out["flat_base"] = true
	}
	return out
}

// sumPatternScore sums YAML integer scores for detected patterns.
func sumPatternScore(detected map[string]bool, scores map[string]int) int {
	total := 0
	for name, isDetected := range detected {
		if isDetected {
			if pts, ok := scores[name]; ok {
				total += pts
			}
		}
	}
	return total
}

// bestPatternName returns the highest-scoring detected pattern name for display.
func bestPatternName(detected map[string]bool, bullish bool, r *Rules) string {
	scores := r.PatternScoreConfig.Bullish
	if !bullish {
		scores = r.PatternScoreConfig.Bearish
	}
	best := ""
	bestScore := -1
	for name, isDetected := range detected {
		if isDetected {
			if s, ok := scores[name]; ok && s > bestScore {
				bestScore = s
				best = name
			}
		}
	}
	if best == "" {
		if bullish {
			return "continuation"
		}
		return "breakdown"
	}
	return best
}

// detectAntiPatterns checks for anti-patterns listed in the YAML.
// Only checks patterns we can detect with available indicator functions.
func detectAntiPatterns(bars []indicators.Bar, r *Rules) []string {
	var found []string

	// Check for late_stage_extension (in bullish_reject list)
	if indicators.IsLateStageExtension(bars) {
		for _, ap := range r.AntiPatternConfig.BullishReject {
			if ap == "late_stage_extension" {
				found = append(found, "late_stage_extension")
				break
			}
		}
	}
	// Check for distribution_severe (HasDistributionDays → maps to distribution_severe)
	if indicators.HasDistributionDays(bars) {
		for _, ap := range r.AntiPatternConfig.BullishReject {
			if ap == "distribution_severe" {
				found = append(found, "distribution_severe")
				break
			}
		}
	}
	return found
}

// isBullishFamily returns true if the family direction is bullish.
func isBullishFamily(family string) bool {
	return family == "bullish_continuation" || family == "bullish_momentum_breakout"
}

// absDiff computes the absolute difference between two prices.
func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// entryFailReasons returns reason codes for each entry condition that failed.
// Called when best.entryReady == false so the UI can explain what's missing.
// volMin is the family-specific volume threshold (er.VolumeRatioMin or 1.5 for momentum).
func entryFailReasons(volRatio, volMin float64, rsiOK bool, rsi, rsiMin, rsiMax, extPct, extMax float64) []string {
	var reasons []string
	if volRatio < volMin {
		reasons = append(reasons, "volume_weak")
	}
	if rsiOK {
		if rsi > rsiMax {
			reasons = append(reasons, "rsi_extended")
		} else if rsi < rsiMin {
			reasons = append(reasons, "rsi_too_weak")
		}
	}
	if extPct > extMax {
		reasons = append(reasons, "entry_too_extended")
	}
	return reasons
}

// appendIfMissing adds s to slice only if not already present.
func appendIfMissing(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

// ── RegimeLabel ───────────────────────────────────────────────────────────────

// RegimeLabel returns a human-readable market regime label for display and DB.
func RegimeLabel(vix float64, btcROC float64) string {
	if vix >= 30 {
		return "risk_off_high_vol"
	}
	if vix >= 24 {
		return "elevated_volatility"
	}
	if vix >= 20 {
		if btcROC < 0 {
			return "caution_zone"
		}
		return "risk_aware"
	}
	if btcROC > 5 {
		return "risk_on"
	}
	return "neutral"
}

// ScanResult holds the aggregate result of scanning all symbols.
type ScanResult struct {
	Date           string
	Regime         Regime
	RegimeLabel    string
	SymbolsScanned int
	Eligible       []SymbolAnalysis
	Ineligible     []SymbolAnalysis
}
