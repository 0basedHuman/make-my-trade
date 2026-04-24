// internal/strategy/engine.go
//
// WHAT: Deterministic strategy scoring engine for the options paper-trade system.
//
// WHY:  All preprocessing before Claude must be deterministic, explainable,
//       and driven by strategy_rules.yaml. This file implements the policy
//       defined in that YAML without hidden logic.
//
// HOW:  The engine runs in 6 explicit layers per symbol:
//
//       LAYER 0 — data quality gate (insufficient bars → reject immediately)
//       LAYER 1 — compute Features once (indicators, slopes, ratios)
//       LAYER 2 — regime hard blocks (VIX, BTC ROC → reject all families)
//       LAYER 3 — score each of the 4 families independently:
//                   a. check preconditions (binary gates from YAML)
//                   b. score 5 dimensions (0.0-1.0 each, weights from YAML)
//                   c. compute weighted score (0-100)
//                   d. apply penalties (anti-patterns, from YAML)
//                   e. check entry conditions (separate from score)
//                   f. assign recommended status from score thresholds
//       LAYER 4 — select best family (highest final score)
//       LAYER 5 — apply event block override (earnings → blocked_by_event)
//
// WHAT BREAKS:
//   - If strategy_rules.yaml is missing, DefaultRules() is used — safe but
//     threshold values may differ from tuned YAML values.
//   - If bars < min_bars_required (35), all indicators fail → rejected.
//   - If VIX >= vix_max, no family is scored — all are rejected.
//
// VERIFY: For a bullish symbol on a low-VIX day, Analyze() should return
//   Eligible=true, SetupFamily="bullish_continuation" or
//   "bullish_momentum_breakout", with ScoreBreakdown.FinalScore >= 45.

package strategy

import (
	"fmt"
	"math"
	"strings"

	"github.com/yourname/makemytrade/internal/indicators"
	"github.com/yourname/makemytrade/internal/market"
)

// ── Public types ──────────────────────────────────────────────────────────────

// Regime holds market-wide macro context passed in from the caller.
type Regime struct {
	VIX      float64
	BTCROC20 float64
	VIXDate  string
}

// GateResult holds a single gate check result (kept for DB + UI compat).
type GateResult struct {
	Passed bool
	Reason string
	Value  float64
}

// Features holds all computed indicators for one symbol.
// Computed once before any family evaluation so each family scorecard
// operates on the same values.
type Features struct {
	Close float64

	// EMAs and derived ratios
	EMA20  float64
	EMA50  float64
	EMA100 float64
	HasEMA20, HasEMA50, HasEMA100 bool

	// EMA20 slope: % change over ema_slope_lookback bars.
	// Positive = rising, negative = falling.
	EMA20Slope    float64
	HasEMA20Slope bool

	// Signed gap percentages
	EMA20vsEMA50Pct  float64 // (EMA20-EMA50)/EMA50*100; positive = EMA20 above
	EMA20vsEMA100Pct float64

	// Signed extension: (close-EMA20)/EMA20*100; positive = close above EMA20
	CloseVsEMA20Pct float64

	// Momentum
	MACDHist float64
	HasMACD  bool

	// RSI
	RSI    float64
	HasRSI bool

	// Volume
	VolumeRatio    float64
	HasVolumeRatio bool

	// Regime
	VIX      float64
	BTCROC20 float64

	// ATR and extension in ATR units
	// ATRExtension = (close - EMA20) / ATR14; positive = close above EMA20.
	// Used by bearish_exhaustion_reversal preconditions and scoring.
	ATR14          float64
	ATRExtension   float64
	HasATRExtension bool

	// Prior bar levels
	PriorHigh float64
	PriorLow  float64

	// Relative strength vs SPY (% outperformance over 20 days)
	RelStrength float64

	// ── v7: Extended indicator sleeves ────────────────────────────────────────

	// Realized volatility (annualized stddev of log returns)
	RealVol20    float64
	RealVol40    float64
	HasRealVol20 bool
	HasRealVol40 bool

	// Vol-scaled momentum: return / realized_vol — cross-ticker normalized
	VolScaledMom63  float64 // ~3-month horizon
	VolScaledMom126 float64 // ~6-month horizon
	HasVolScaledMom bool

	// Shannon entropy of daily returns (0=clean trend, 1=coin flip)
	Entropy30    float64
	HasEntropy30 bool

	// Bollinger Band width (upper−lower / SMA, 20-bar, 2σ)
	BollingerWidth20    float64
	HasBollingerWidth   bool

	// Squeeze ratio: Bollinger width / Keltner width (< 1.0 = squeeze active)
	SqueezeRatio20    float64
	HasSqueezeRatio   bool

	// Raw returns used in SMA/EMA divergence sleeve
	Return63d  float64 // 63-bar ROC %
	Return126d float64 // 126-bar ROC %
	HasReturn63d  bool
	HasReturn126d bool
}

// FamilyScore is the complete scored result for one setup family.
// Kept in SymbolAnalysis for debugging and API exposure.
type FamilyScore struct {
	Family        string `json:"family"`
	Eligible      bool   `json:"preconditions_met"`
	FailedPrecond string `json:"failed_precondition,omitempty"`

	// Dimension scores 0.0-1.0 each (for tracing score origin)
	TrendStructure      float64 `json:"trend_structure"`
	MomentumAlignment   float64 `json:"momentum_alignment"`
	VolumeParticipation float64 `json:"volume_participation"`
	EntryQuality        float64 `json:"entry_quality"`
	PatternStrength     float64 `json:"pattern_strength"`

	WeightedScore float64 `json:"weighted_score"` // before penalties, 0-100
	Penalties     float64 `json:"penalties"`      // total deduction
	FinalScore    float64 `json:"final_score"`    // after penalties, ≥ 0

	EntryConditionsMet bool     `json:"entry_conditions_met"`
	EntryFailReasons   []string `json:"entry_fail_reasons,omitempty"`

	// Recommended lifecycle status from this family's score.
	Status string `json:"status"`
}

// SymbolAnalysis is the complete preprocessing output for one symbol.
// All fields are kept for DB/API/UI backward compatibility.
type SymbolAnalysis struct {
	Ticker string
	Date   string

	// ── Eligibility ───────────────────────────────────────────────────────────
	Eligible     bool
	ScreenReason string

	// ── Computed indicators ───────────────────────────────────────────────────
	ClosePrice  float64
	EMA20       float64
	EMA50       float64
	EMA100      float64
	RSI14       float64
	MACDHist    float64
	VolumeRatio float64

	// ── Trend ─────────────────────────────────────────────────────────────────
	TrendBias   string
	TrendReason string

	// ── Price levels ──────────────────────────────────────────────────────────
	PriorDayHigh float64
	PriorDayLow  float64

	// ── Family selection ──────────────────────────────────────────────────────
	SetupFamily     string
	MatchedFamilies []string // all families that passed preconditions

	// ── Score breakdown (new in v6) ───────────────────────────────────────────
	// ScoreBreakdown is the winning family's full scored result.
	// AllFamilyScores contains scored results for all 4 families (for debugging).
	ScoreBreakdown   FamilyScore
	AllFamilyScores  []FamilyScore

	// ── Lifecycle status ──────────────────────────────────────────────────────
	// Set deterministically by the engine from YAML thresholds.
	// "watch_only" and "confirmed" are only assigned by Claude or the
	// confirmation evaluator — never by Analyze().
	CandidateStatus string
	ReasonCodes     []string

	// ── Pattern scoring (kept for DB compat) ─────────────────────────────────
	PatternName     string
	PatternScoreInt int
	PatternScore    float64 // = float64(PatternScoreInt) for DB column compat

	// ── Anti-patterns ─────────────────────────────────────────────────────────
	AntiPatterns   []string
	RejectedByAnti bool

	// ── Structure-based targets ───────────────────────────────────────────────
	BaseTarget    float64
	StretchTarget float64
	EntryLow      float64
	EntryHigh     float64
	StopLoss      float64
	Target1       float64 // = BaseTarget (DB compat)
	Target2       float64 // = StretchTarget (DB compat)
	RRRatio       float64

	// ── Hold window (from YAML family config) ─────────────────────────────────
	HoldDaysMin  int
	HoldDaysBase int
	HoldDaysMax  int

	// ── Gate fields (informational, kept for DB + UI) ─────────────────────────
	GateTrend    GateResult
	GateMomentum GateResult
	GateVolume   GateResult
	GateVIX      GateResult
	GateBTC      GateResult
	GateRSI      GateResult

	AllGatesPassed bool
	RejectReason   string

	// ── News / sentiment ──────────────────────────────────────────────────────
	EarningsRisk   bool
	SentimentScore float64
	NewsSentiment  float64
	Sentiment      string

	// ── Relative strength ─────────────────────────────────────────────────────
	RelativeStrength float64
}

// ── Engine ────────────────────────────────────────────────────────────────────

// Config holds data-quality thresholds separate from strategy rules.
type Config struct {
	MinBarsRequired int
}

// DefaultConfig returns the engine data-quality config.
func DefaultConfig() Config {
	return Config{MinBarsRequired: 35}
}

// Engine runs deterministic preprocessing for the options decision pipeline.
type Engine struct {
	cfg   Config
	rules *Rules
}

// NewEngine creates an Engine with the given config and rules.
// If rules is nil, DefaultRules() is used.
func NewEngine(cfg Config, rules *Rules) *Engine {
	if rules == nil {
		rules = DefaultRules()
	}
	return &Engine{cfg: cfg, rules: rules}
}

// Analyze runs all 6 layers of preprocessing for one symbol.
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

	// ── LAYER 0: Data quality gate ───────────────────────────────────────────
	minBars := e.rules.MinBarsRequired()
	if e.cfg.MinBarsRequired > minBars {
		minBars = e.cfg.MinBarsRequired
	}
	if len(bars) < minBars {
		return e.reject(&a, fmt.Sprintf("insufficient_data: %d bars (need %d+)", len(bars), minBars),
			"insufficient_data")
	}

	// ── LAYER 1: Compute Features ────────────────────────────────────────────
	f := e.computeFeatures(bars, spyBars, regime)
	a.ClosePrice = f.Close
	a.EMA20 = f.EMA20
	a.EMA50 = f.EMA50
	a.EMA100 = f.EMA100
	a.RSI14 = f.RSI
	a.MACDHist = f.MACDHist
	a.VolumeRatio = f.VolumeRatio
	a.PriorDayHigh = f.PriorHigh
	a.PriorDayLow = f.PriorLow
	a.RelativeStrength = f.RelStrength

	// Trend bias (informational — families have their own preconditions)
	a.TrendBias, a.TrendReason = classifyTrend(f)

	// Sentiment
	a.SentimentScore = sentimentData.Score
	a.NewsSentiment = sentimentData.Score
	a.Sentiment = sentimentLabel(sentimentData.Score)

	// Informational gate fields (UI / DB compat)
	e.fillGateFields(&a, f)

	// ── LAYER 2: Regime hard blocks ──────────────────────────────────────────
	r := e.rules
	if f.VIX >= r.Regime.HardBlocks.VIXMax {
		return e.reject(&a,
			fmt.Sprintf("VIX (%.1f) >= %.0f — regime hard gate", f.VIX, r.Regime.HardBlocks.VIXMax),
			"vix_too_high")
	}

	// ── LAYER 3: Detect patterns and anti-patterns ───────────────────────────
	detectedPatterns := detectPatterns(bars, f.RelStrength, f)
	a.AntiPatterns = detectAntiPatterns(bars, r)
	if len(a.AntiPatterns) > 0 {
		a.RejectedByAnti = true
		a.ReasonCodes = appendUniq(a.ReasonCodes, "anti_pattern_detected")
	}

	// ── LAYER 4: Score all 5 families ────────────────────────────────────────
	familyOrder := []string{
		"bullish_continuation",
		"bullish_momentum_breakout",
		"bearish_continuation",
		"bearish_momentum_breakdown",
		"bearish_exhaustion_reversal",
	}

	var allScores []FamilyScore
	for _, name := range familyOrder {
		cfg, ok := r.Families[name]
		if !ok {
			continue
		}
		fs := scoreFamily(name, cfg, f, detectedPatterns, r.PatternScoreConfig, r.Penalties, a.AntiPatterns)
		allScores = append(allScores, fs)
	}
	a.AllFamilyScores = allScores

	// ── LAYER 5: Select best family ──────────────────────────────────────────
	best, matched := selectBestFamily(allScores)
	a.MatchedFamilies = matched

	if best == nil || best.Status == "rejected" {
		// No family reached structural_candidate
		a.ReasonCodes = append(a.ReasonCodes, noMatchReasonCodes(f)...)
		if len(a.ReasonCodes) == 0 {
			a.ReasonCodes = append(a.ReasonCodes, "trend_down")
		}
		a.ScreenReason = strings.Join(a.ReasonCodes, ", ")
		a.RejectReason = a.ScreenReason
		a.CandidateStatus = "rejected"
		a.PatternScore = 0
		return a
	}

	// Found a scoreable family
	a.SetupFamily = best.Family
	a.ScoreBreakdown = *best
	a.PatternScoreInt = int(math.Round(best.PatternStrength * 8.0)) // reverse-normalize for DB compat
	a.PatternScore = float64(a.PatternScoreInt)
	a.PatternName = bestPatternName(detectedPatterns, isBullishFamily(best.Family), r)
	a.CandidateStatus = best.Status
	a.ReasonCodes = append(a.ReasonCodes, best.EntryFailReasons...)

	// Family-specific targets and hold window
	e.fillTargets(&a, bars, best.Family)
	familyCfg := r.Families[best.Family]
	hw := familyCfg.HoldWindow
	a.HoldDaysMin = hw.Min
	a.HoldDaysBase = hw.Base
	a.HoldDaysMax = hw.Max

	// Reason codes from direction
	if isBullishFamily(best.Family) {
		a.ReasonCodes = appendUniq(a.ReasonCodes, "trend_up", "above_ema20")
		if f.HasMACD && f.MACDHist > 0 {
			a.ReasonCodes = appendUniq(a.ReasonCodes, "macd_positive")
		}
		if best.EntryConditionsMet {
			a.ReasonCodes = appendUniq(a.ReasonCodes, "volume_strong")
		}
	} else if best.Family == "bearish_exhaustion_reversal" {
		// Price is ABOVE EMA20 — this is a reversal setup, not a breakdown.
		a.ReasonCodes = appendUniq(a.ReasonCodes, "rsi_extended", "above_ema20", "bearish_reversal_setup")
		if f.HasATRExtension {
			a.ReasonCodes = appendUniq(a.ReasonCodes, "overextended_above_ema20")
		}
	} else {
		a.ReasonCodes = appendUniq(a.ReasonCodes, "trend_down", "below_ema20")
		if f.HasMACD && f.MACDHist < 0 {
			a.ReasonCodes = appendUniq(a.ReasonCodes, "macd_negative")
		}
	}

	// ── LAYER 6: Event block override ────────────────────────────────────────
	if earningsRisk {
		blocked := r.BlockedByEventStatus()
		if r.StateRules.BlockedByEventOverridesEntryReady &&
			(a.CandidateStatus == "entry_ready" || a.CandidateStatus == "structural_candidate") {
			a.CandidateStatus = blocked
			a.Eligible = false
			a.AllGatesPassed = false
			a.ReasonCodes = appendUniq(a.ReasonCodes, "event_blackout_earnings")
			return a
		}
	}

	a.Eligible = true
	a.AllGatesPassed = true
	return a
}

// ── Feature computation ───────────────────────────────────────────────────────

func (e *Engine) computeFeatures(bars []indicators.Bar, spyBars []indicators.Bar, regime Regime) Features {
	closes := indicators.Closes(bars)
	volumes := indicators.Volumes(bars)
	n := len(bars)

	f := Features{
		Close:    closes[n-1],
		VIX:      regime.VIX,
		BTCROC20: regime.BTCROC20,
	}
	if n >= 2 {
		f.PriorHigh = bars[n-2].High
		f.PriorLow = bars[n-2].Low
	}

	lk := e.rules.Global.FeatureWindows.EMASlopeLookback
	if lk <= 0 {
		lk = 5
	}

	if ema20, ok := indicators.EMALast(closes, 20); ok {
		f.EMA20, f.HasEMA20 = ema20, true
	}
	if ema50, ok := indicators.EMALast(closes, 50); ok {
		f.EMA50, f.HasEMA50 = ema50, true
	}
	if ema100, ok := indicators.EMALast(closes, 100); ok {
		f.EMA100, f.HasEMA100 = ema100, true
	}

	if f.HasEMA20 && f.HasEMA50 && f.EMA50 != 0 {
		f.EMA20vsEMA50Pct = (f.EMA20 - f.EMA50) / f.EMA50 * 100
	}
	if f.HasEMA20 && f.HasEMA100 && f.EMA100 != 0 {
		f.EMA20vsEMA100Pct = (f.EMA20 - f.EMA100) / f.EMA100 * 100
	}
	if f.HasEMA20 && f.EMA20 != 0 {
		f.CloseVsEMA20Pct = (f.Close - f.EMA20) / f.EMA20 * 100
	}

	if slope, ok := indicators.EMASlope(closes, 20, lk); ok {
		f.EMA20Slope, f.HasEMA20Slope = slope, true
	}

	if macd, ok := indicators.MACDLast(closes); ok {
		f.MACDHist, f.HasMACD = macd.Histogram, true
	}
	if rsi, ok := indicators.RSILast(closes, 14); ok {
		f.RSI, f.HasRSI = rsi, true
	}
	if vr, ok := indicators.VolumeRatioLast(volumes, 20); ok {
		f.VolumeRatio, f.HasVolumeRatio = vr, true
	}

	if len(spyBars) >= 21 {
		spyCloses := indicators.Closes(spyBars)
		if rs, ok := indicators.RelativeStrength(closes, spyCloses); ok {
			f.RelStrength = rs
		}
	}

	// ── v7: Extended indicator sleeves ────────────────────────────────────────
	fw := e.rules.Global.FeatureWindows

	rvShort := fw.RealizedVolShort
	if rvShort <= 0 {
		rvShort = 20
	}
	rvLong := fw.RealizedVolLong
	if rvLong <= 0 {
		rvLong = 40
	}
	momShort := fw.MomentumShort
	if momShort <= 0 {
		momShort = 63
	}
	momLong := fw.MomentumLong
	if momLong <= 0 {
		momLong = 126
	}
	entropyPeriod := fw.Entropy
	if entropyPeriod <= 0 {
		entropyPeriod = 30
	}
	bolPeriod := fw.Bollinger
	if bolPeriod <= 0 {
		bolPeriod = 20
	}

	if rv, ok := indicators.RealizedVolatility(closes, rvShort); ok {
		f.RealVol20, f.HasRealVol20 = rv, true
	}
	if rv, ok := indicators.RealizedVolatility(closes, rvLong); ok {
		f.RealVol40, f.HasRealVol40 = rv, true
	}
	if vsm63, ok := indicators.VolScaledMomentum(closes, momShort); ok {
		f.VolScaledMom63, f.HasVolScaledMom = vsm63, true
	}
	if vsm126, ok := indicators.VolScaledMomentum(closes, momLong); ok {
		f.VolScaledMom126 = vsm126
		// HasVolScaledMom already set if 63d succeeded; 126d is supplemental
	}
	if ent, ok := indicators.ShannonEntropy(closes, entropyPeriod); ok {
		f.Entropy30, f.HasEntropy30 = ent, true
	}
	if bw, ok := indicators.BollingerWidth(closes, bolPeriod, 2.0); ok {
		f.BollingerWidth20, f.HasBollingerWidth = bw, true
	}
	if sr, ok := indicators.SqueezeRatio(bars, bolPeriod); ok {
		f.SqueezeRatio20, f.HasSqueezeRatio = sr, true
	}
	if r63, ok := indicators.ROCLast(closes, momShort); ok {
		f.Return63d, f.HasReturn63d = r63, true
	}
	if r126, ok := indicators.ROCLast(closes, momLong); ok {
		f.Return126d, f.HasReturn126d = r126, true
	}

	// ATR extension: (close - EMA20) / ATR14.
	// Used by bearish_exhaustion_reversal precondition and trend scoring.
	if atr, ok := indicators.ATRLast(bars, 14); ok && atr > 0 && f.HasEMA20 {
		f.ATR14 = atr
		f.ATRExtension = (f.Close - f.EMA20) / atr
		f.HasATRExtension = true
	}

	return f
}

// ── Family scoring ────────────────────────────────────────────────────────────

// scoreFamily evaluates one setup family against the computed features.
// Returns a FamilyScore with full dimension breakdown.
// This function has no side effects and no logging.
func scoreFamily(
	name string,
	cfg FamilyConfig,
	f Features,
	patterns map[string]bool,
	patScores PatternScoreConfig,
	penalties PenaltiesConfig,
	antiPatterns []string,
) FamilyScore {

	fs := FamilyScore{Family: name}

	// ── a. Preconditions ─────────────────────────────────────────────────────
	if fail := checkPreconditions(cfg.Preconditions, f); fail != "" {
		fs.Eligible = false
		fs.FailedPrecond = fail
		fs.Status = "rejected"
		return fs
	}
	fs.Eligible = true

	// ── b. Score 5 dimensions ─────────────────────────────────────────────
	fs.TrendStructure = scoreTrendStructure(name, cfg, f)
	fs.MomentumAlignment = scoreMomentumAlignment(cfg, f)
	fs.VolumeParticipation = scoreVolumeParticipation(cfg, f)
	fs.EntryQuality = scoreEntryQuality(name, cfg, f)

	isBullish := isBullishFamily(name)
	patScore := sumPatternScore(patterns, patScores, isBullish)
	fs.PatternStrength = scorePatternStrength(patScore)

	// ── c. Weighted score ─────────────────────────────────────────────────
	w := cfg.Scoring.Weights
	fs.WeightedScore = fs.TrendStructure*float64(w.TrendStructure) +
		fs.MomentumAlignment*float64(w.MomentumAlignment) +
		fs.VolumeParticipation*float64(w.VolumeParticipation) +
		fs.EntryQuality*float64(w.EntryQuality) +
		fs.PatternStrength*float64(w.PatternStrength)

	// ── d. Penalties ──────────────────────────────────────────────────────
	fs.Penalties = computePenalties(name, f, penalties, antiPatterns)
	fs.FinalScore = math.Max(0, fs.WeightedScore-fs.Penalties)

	// ── e. Entry conditions (separate from score) ─────────────────────────
	fs.EntryConditionsMet, fs.EntryFailReasons = checkEntryConditions(cfg.EntryConditions, f)

	// ── f. Status from thresholds ─────────────────────────────────────────
	t := cfg.Scoring.Thresholds
	switch {
	case fs.FinalScore < t.StructuralCandidate:
		fs.Status = "rejected"
	case fs.FinalScore < t.EntryReady || !fs.EntryConditionsMet:
		fs.Status = "structural_candidate"
	default:
		fs.Status = "entry_ready"
	}

	// MaxScanStatus cap: certain families (e.g. bearish_exhaustion_reversal)
	// cannot be promoted to entry_ready by the daily scan engine alone.
	// entry_ready for these families is only assigned by the opening confirmation
	// activity after intraday rejection evidence is observed.
	if cfg.MaxScanStatus != "" && fs.Status == "entry_ready" {
		fs.Status = cfg.MaxScanStatus
	}

	return fs
}

// checkPreconditions returns the name of the first failing precondition,
// or "" if all pass. Order is deterministic and matches YAML documentation.
func checkPreconditions(p FamilyPreconditions, f Features) string {
	if p.EMA20AboveEMA50 && !(f.HasEMA20 && f.HasEMA50 && f.EMA20 > f.EMA50) {
		return "ema20_above_ema50"
	}
	if p.EMA20AboveEMA100 && !(f.HasEMA20 && f.HasEMA100 && f.EMA20 > f.EMA100) {
		return "ema20_above_ema100"
	}
	if p.CloseAboveEMA20 && !(f.HasEMA20 && f.Close > f.EMA20) {
		return "close_above_ema20"
	}
	if p.MACDHistPositive && !(f.HasMACD && f.MACDHist > 0) {
		return "macd_histogram_positive"
	}
	if p.BTCNotNegative && f.BTCROC20 < 0 {
		return "btc_regime_not_negative"
	}
	if p.EMA20BelowEMA50 && !(f.HasEMA20 && f.HasEMA50 && f.EMA20 < f.EMA50) {
		return "ema20_below_ema50"
	}
	if p.EMA20BelowEMA100 && !(f.HasEMA20 && f.HasEMA100 && f.EMA20 < f.EMA100) {
		return "ema20_below_ema100"
	}
	if p.CloseBelowEMA20 && !(f.HasEMA20 && f.Close < f.EMA20) {
		return "close_below_ema20"
	}
	if p.MACDHistNegative && !(f.HasMACD && f.MACDHist < 0) {
		return "macd_histogram_negative"
	}
	if p.EMA20SlopePositive && !(f.HasEMA20Slope && f.EMA20Slope > 0) {
		return "ema20_slope_positive"
	}
	if p.EMA20SlopeNegative && !(f.HasEMA20Slope && f.EMA20Slope < 0) {
		return "ema20_slope_negative"
	}
	// Exhaustion reversal: numeric threshold preconditions (zero = not enforced)
	if p.RSIMinPrecondition > 0 && !(f.HasRSI && f.RSI >= p.RSIMinPrecondition) {
		return "rsi_min_precondition"
	}
	if p.ATRExtensionMin > 0 && !(f.HasATRExtension && f.ATRExtension >= p.ATRExtensionMin) {
		return "atr_extension_min"
	}
	return ""
}

// ── Dimension scorers (each returns 0.0-1.0) ─────────────────────────────────

// scoreTrendStructure measures the quality of the EMA stack alignment.
// For continuation families: EMA20-EMA50 gap depth.
// For momentum families: EMA20 slope steepness (% per 5 bars).
//
// v7 sleeve: SMA/EMA divergence (Return63d vs Return126d).
//   Short momentum > long momentum → trend is accelerating → +0.10 bonus.
//   Short momentum < long momentum → trend may be exhausting → −0.05 penalty.
func scoreTrendStructure(name string, cfg FamilyConfig, f Features) float64 {
	// Exhaustion reversal: score based on ATR extension magnitude.
	// More extended above EMA20 = higher reversal potential = better score.
	// Precondition already gates ATRExtension >= 1.8, so all callers here are >= 1.8.
	if name == "bearish_exhaustion_reversal" {
		if !f.HasATRExtension {
			return 0.3
		}
		ext := f.ATRExtension
		var base float64
		switch {
		case ext >= 3.5:
			base = 1.0
		case ext >= 2.5:
			base = lerp(0.70, 1.0, (ext-2.5)/1.0)
		case ext >= 1.8:
			base = lerp(0.40, 0.70, (ext-1.8)/0.7)
		default:
			base = 0.2
		}
		return clamp01(base)
	}

	isMomentum := name == "bullish_momentum_breakout" || name == "bearish_momentum_breakdown"
	var base float64

	if isMomentum {
		if !f.HasEMA20Slope {
			base = 0.3
		} else {
			slope := f.EMA20Slope
			if name == "bearish_momentum_breakdown" {
				slope = -slope
			}
			switch {
			case slope >= 1.0:
				base = 1.0
			case slope >= 0.5:
				base = lerp(0.7, 1.0, (slope-0.5)/0.5)
			case slope >= 0.2:
				base = lerp(0.5, 0.7, (slope-0.2)/0.3)
			default:
				base = 0.3
			}
		}
	} else {
		// Continuation: score based on EMA20-EMA50 gap quality.
		g := cfg.EMAGapPct
		if g.StrongMin == 0 && g.AdequateMin == 0 {
			base = 0.5
		} else {
			gapPct := f.EMA20vsEMA50Pct
			if name == "bearish_continuation" {
				gapPct = -gapPct
			}
			switch {
			case gapPct >= g.StrongMin:
				base = 1.0
			case gapPct >= g.AdequateMin:
				if g.StrongMin > g.AdequateMin {
					base = lerp(0.4, 1.0, (gapPct-g.AdequateMin)/(g.StrongMin-g.AdequateMin))
				} else {
					base = 0.6
				}
			default:
				base = 0.2
			}
		}
	}

	// Sleeve 3: SMA/EMA divergence — short vs long momentum alignment
	if f.HasReturn63d && f.HasReturn126d {
		isBull := isBullishFamily(name)
		short := f.Return63d
		long := f.Return126d
		if !isBull {
			short = -short // for bearish families, negative returns are positive signals
			long = -long
		}
		if short > long+5.0 {
			// Short momentum meaningfully exceeds long → trend accelerating
			base = math.Min(1.0, base+0.10)
		} else if short < long-5.0 {
			// Short momentum lagging long → potential trend exhaustion
			base = math.Max(0, base-0.05)
		}
	}

	return clamp01(base)
}

// scoreMomentumAlignment measures MACD magnitude, RSI position, and (v7)
// vol-scaled momentum quality gated by Shannon entropy.
//
// v7 sleeve integration:
//   - Vol-scaled momentum (vsm63) adds up to +0.15 bonus when momentum is
//     strong and risk-adjusted (vsm > 1.5).
//   - Shannon entropy gates the bonus: high entropy (choppy) → no bonus;
//     low entropy (clean trend) → full bonus. Entropy > 0.92 also penalizes
//     the base score slightly.
func scoreMomentumAlignment(cfg FamilyConfig, f Features) float64 {
	macdScore := scoreMACDComponent(f)
	rsiScore := scoreRSIComponent(cfg.RSI, f)
	base := (macdScore + rsiScore) / 2.0

	// Sleeve 1: vol-scaled momentum bonus (0..+0.15)
	vsmBonus := 0.0
	if f.HasVolScaledMom {
		vsm := math.Abs(f.VolScaledMom63)
		switch {
		case vsm >= 2.5:
			vsmBonus = 0.15
		case vsm >= 1.5:
			vsmBonus = lerp(0.07, 0.15, (vsm-1.5)/1.0)
		case vsm >= 0.8:
			vsmBonus = lerp(0.0, 0.07, (vsm-0.8)/0.7)
		}
	}

	// Sleeve 2: entropy gate — low entropy (clean trend) = full bonus;
	// high entropy (choppy) = reduced bonus and small base penalty.
	entropyMultiplier := 1.0
	if f.HasEntropy30 {
		ent := f.Entropy30
		if ent > 0.92 {
			// Very choppy — apply mild base penalty and zero vsmBonus
			base = math.Max(0, base-0.08)
			vsmBonus = 0
			entropyMultiplier = 0
		} else if ent > 0.80 {
			// Moderately choppy — scale down bonus
			entropyMultiplier = lerp(0.0, 1.0, (0.92-ent)/(0.92-0.80))
		}
		// ent <= 0.80: clean trend → full bonus (multiplier stays 1.0)
	}

	return math.Min(1.0, base+vsmBonus*entropyMultiplier)
}

// scoreMACDComponent scores the MACD histogram magnitude relative to price.
// Normalizes the histogram as a percentage of price to be comparable across tickers.
func scoreMACDComponent(f Features) float64 {
	if !f.HasMACD || f.Close == 0 {
		return 0.3
	}
	// Normalize histogram as % of price for cross-ticker comparability.
	// Sign doesn't matter here (precondition already gates direction).
	histPct := math.Abs(f.MACDHist) / f.Close * 100
	// ≥ 0.5% → strong, ≥ 0.2% → moderate, ≥ 0.05% → weak positive
	switch {
	case histPct >= 0.5:
		return 1.0
	case histPct >= 0.2:
		return lerp(0.6, 1.0, (histPct-0.2)/0.3)
	case histPct >= 0.05:
		return lerp(0.35, 0.6, (histPct-0.05)/0.15)
	default:
		return 0.25 // barely positive (precondition passed but very weak)
	}
}

// scoreRSIComponent scores RSI position within the family's ideal/acceptable bands.
func scoreRSIComponent(bands FamilyRSIBands, f Features) float64 {
	if !f.HasRSI {
		return 0.3
	}
	rsi := f.RSI
	if rsi >= bands.IdealMin && rsi <= bands.IdealMax {
		return 1.0
	}
	if rsi >= bands.AcceptableMin && rsi <= bands.AcceptableMax {
		// Score decreases as RSI moves away from the ideal range.
		distFromIdeal := math.Min(
			math.Abs(rsi-bands.IdealMin),
			math.Abs(rsi-bands.IdealMax),
		)
		idealWidth := bands.AcceptableMax - bands.AcceptableMin
		if idealWidth == 0 {
			return 0.5
		}
		return math.Max(0.3, 0.9-distFromIdeal/idealWidth*0.6)
	}
	return 0.0 // outside acceptable range
}

// scoreVolumeParticipation scores relative volume against family thresholds.
func scoreVolumeParticipation(cfg FamilyConfig, f Features) float64 {
	if !f.HasVolumeRatio {
		return 0.3
	}
	vr := f.VolumeRatio
	v := cfg.Volume
	switch {
	case vr >= v.StrongMin:
		return 1.0
	case vr >= v.AdequateMin:
		if v.StrongMin > v.AdequateMin {
			return lerp(0.5, 1.0, (vr-v.AdequateMin)/(v.StrongMin-v.AdequateMin))
		}
		return 0.6
	default:
		return 0.2 // below adequate — counts against entry but not a hard block at structural level
	}
}

// scoreEntryQuality scores how close to ideal the current entry point is.
// Extension from EMA20 is the primary signal; RSI tightness adds refinement.
//
// For bearish_exhaustion_reversal the logic is inverted: MORE extension above
// EMA20 is a better exhaustion signal. We also score upper-wick quality from
// the last daily bar as an early rejection indicator.
//
// v7 sleeve 4: Bollinger width + squeeze ratio (breakout/expansion quality).
//   Squeeze active (ratio < 1.0): potential energy → +0.10 entry quality bonus.
//   Expanding bands (width > 0.08): breakout in progress → +0.08 bonus.
//   Extremely wide bands (width > 0.15): overextended → small penalty.
func scoreEntryQuality(name string, cfg FamilyConfig, f Features) float64 {
	// Exhaustion reversal: inverted extension logic.
	// Larger ATR extension = better entry quality (more room to fall).
	// Additionally reward upper wicks on the last bar as early rejection signals.
	if name == "bearish_exhaustion_reversal" {
		var base float64
		if f.HasATRExtension {
			ext := f.ATRExtension
			switch {
			case ext >= 3.0:
				base = 1.0
			case ext >= 2.2:
				base = lerp(0.65, 1.0, (ext-2.2)/0.8)
			case ext >= 1.8:
				base = lerp(0.40, 0.65, (ext-1.8)/0.4)
			default:
				base = 0.3
			}
		} else {
			base = 0.3
		}
		// Bonus for upper wick on last daily bar: early sign of intraday selling.
		// Upper wick = (high - max(open, close)) / (high - low).
		if f.HasBollingerWidth { // BollingerWidth computed → bars were sufficient
			// wick quality is computed from Features context — we can't access bars here.
			// The small Bollinger Width bonus below captures squeeze/expansion quality.
		}
		// Wide Bollinger bands around a peak = volatility warning → mild bonus
		if f.HasBollingerWidth && f.BollingerWidth20 > 0.08 {
			base = math.Min(1.0, base+0.05)
		}
		return clamp01(base)
	}

	ext := cfg.ExtensionPct
	extPct := math.Abs(f.CloseVsEMA20Pct)

	var extScore float64
	switch {
	case extPct <= ext.IdealMax:
		extScore = 1.0
	case extPct <= ext.AcceptableMax:
		extScore = lerp(0.5, 1.0, 1.0-(extPct-ext.IdealMax)/(ext.AcceptableMax-ext.IdealMax))
	case extPct <= ext.HardReject:
		extScore = 0.1
	default:
		extScore = 0.0
	}

	// RSI tightness bonus
	rsiBonus := 0.0
	if f.HasRSI {
		if f.RSI >= cfg.RSI.IdealMin && f.RSI <= cfg.RSI.IdealMax {
			rsiBonus = 0.2
		}
	}
	base := math.Min(1.0, extScore+rsiBonus*extScore)

	// Sleeve 4: Bollinger / squeeze expansion quality
	expansionBonus := 0.0
	if f.HasSqueezeRatio && f.SqueezeRatio20 < 1.0 {
		// Squeeze active: energy coiling, potential breakout
		// Tighter squeeze → larger bonus
		squeezeness := 1.0 - f.SqueezeRatio20 // 0 = not squeezed, 1 = fully inside channel
		expansionBonus += lerp(0.0, 0.10, squeezeness)
	}
	if f.HasBollingerWidth {
		bw := f.BollingerWidth20
		if bw > 0.15 {
			// Bands very wide → overextended, small penalty
			base = math.Max(0, base-0.06)
		} else if bw > 0.08 {
			// Bands expanding — breakout underway
			expansionBonus += lerp(0.0, 0.08, (bw-0.08)/0.07)
		}
		// bw < 0.08: tight bands, squeeze bonus already handled above
	}

	return clamp01(base + expansionBonus)
}

// scorePatternStrength normalizes the integer pattern score to 0.0-1.0.
// Max score of 8 points → 1.0 (matches the highest-scoring pattern set in YAML).
func scorePatternStrength(rawScore int) float64 {
	const maxPatternScore = 8.0
	if rawScore <= 0 {
		return 0.0
	}
	return math.Min(1.0, float64(rawScore)/maxPatternScore)
}

// ── Penalties ─────────────────────────────────────────────────────────────────

// computePenalties returns the total score deduction for this candidate.
// Multiple penalties are additive.
func computePenalties(family string, f Features, p PenaltiesConfig, antiPatterns []string) float64 {
	total := 0.0
	isBullish := isBullishFamily(family)

	for _, ap := range antiPatterns {
		switch ap {
		case "late_stage_extension":
			total += p.LateStageExtension
		case "distribution_severe":
			total += p.DistributionSevere
		}
	}

	// RSI extremes incur a penalty (not a hard block) so the score signal is clear.
	// Exception: bearish_exhaustion_reversal intentionally requires high RSI — no penalty.
	if f.HasRSI && family != "bearish_exhaustion_reversal" {
		if isBullish && f.RSI > 78 {
			total += p.RSIOverextendedBullish
		}
		if !isBullish && f.RSI < 22 {
			total += p.RSIOversoldBearish
		}
	}
	return total
}

// ── Entry conditions ──────────────────────────────────────────────────────────

// checkEntryConditions evaluates the hard entry gates defined in YAML.
// Returns (met bool, failing reason codes).
// These are separate from the weighted score — even a high-scoring ticker
// must pass these to receive entry_ready status.
func checkEntryConditions(ec FamilyEntryConditions, f Features) (bool, []string) {
	var fails []string

	if f.HasVolumeRatio && f.VolumeRatio < ec.VolumeMin {
		fails = append(fails, "volume_weak")
	}
	if f.HasRSI {
		if f.RSI < ec.RSIMin {
			fails = append(fails, "rsi_too_weak")
		} else if f.RSI > ec.RSIMax {
			fails = append(fails, "rsi_extended")
		}
	}
	extPct := math.Abs(f.CloseVsEMA20Pct)
	if extPct > ec.ExtensionMaxPct {
		fails = append(fails, "entry_too_extended")
	}

	return len(fails) == 0, fails
}

// ── Family selection ──────────────────────────────────────────────────────────

// selectBestFamily picks the highest-scoring family that reached at least
// structural_candidate. On a FinalScore tie, continuation wins over momentum
// (more conservative). Returns nil if no family qualifies.
func selectBestFamily(scores []FamilyScore) (*FamilyScore, []string) {
	var matched []string
	var best *FamilyScore

	for i := range scores {
		fs := &scores[i]
		if !fs.Eligible || fs.Status == "rejected" {
			continue
		}
		matched = append(matched, fs.Family)
		if best == nil {
			best = fs
			continue
		}
		if fs.FinalScore > best.FinalScore {
			best = fs
			continue
		}
		// Tie-break: prefer continuation over momentum (safer)
		if fs.FinalScore == best.FinalScore && isContinuationFamily(fs.Family) && !isContinuationFamily(best.Family) {
			best = fs
		}
	}
	return best, matched
}

// ── Pattern helpers ───────────────────────────────────────────────────────────

// detectPatterns runs the available pattern detectors and returns a map of
// pattern_name → detected. Shared by all families; the engine selects
// the relevant subset by direction.
func detectPatterns(bars []indicators.Bar, relStrength float64, f Features) map[string]bool {
	out := make(map[string]bool)

	if bf, _ := indicators.IsBullFlag(bars); bf {
		out["bull_flag"] = true
	}
	if tb, _ := indicators.IsTightBase(bars, 12); tb {
		out["tight_base"] = true
		out["flat_base"] = true // flat_base and tight_base are equivalent
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
	if relStrength > 5.0 {
		out["relative_strength_bullish"] = true
	}
	if relStrength < -5.0 {
		out["relative_weakness_bearish"] = true
	}

	n := len(bars)

	// overextension_exhaustion: price extended >= 1.8 ATR above EMA20 AND
	// the last bar shows a meaningful upper wick (intraday sellers emerging).
	if f.HasATRExtension && f.ATRExtension >= 1.8 && n >= 1 {
		last := bars[n-1]
		candleRange := last.High - last.Low
		upperWick := last.High - math.Max(last.Open, last.Close)
		if candleRange > 0 && upperWick/candleRange >= 0.20 {
			out["overextension_exhaustion"] = true
		}
	}

	// rejection_wick_reversal: large upper wick on the last bar (>= 40% of range),
	// indicating meaningful selling into the day's high.
	if n >= 1 {
		last := bars[n-1]
		candleRange := last.High - last.Low
		upperWick := last.High - math.Max(last.Open, last.Close)
		if candleRange > 0 && upperWick/candleRange >= 0.40 {
			out["rejection_wick_reversal"] = true
		}
	}

	return out
}

// sumPatternScore sums integer YAML scores for detected patterns.
func sumPatternScore(detected map[string]bool, scores PatternScoreConfig, bullish bool) int {
	m := scores.Bullish
	if !bullish {
		m = scores.Bearish
	}
	total := 0
	for name, isDetected := range detected {
		if isDetected {
			if pts, ok := m[name]; ok {
				total += pts
			}
		}
	}
	return total
}

// bestPatternName returns the highest-scoring detected pattern name.
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
func detectAntiPatterns(bars []indicators.Bar, r *Rules) []string {
	var found []string
	if indicators.IsLateStageExtension(bars) {
		for _, ap := range r.AntiPatternConfig.BullishReject {
			if ap == "late_stage_extension" {
				found = append(found, "late_stage_extension")
				break
			}
		}
	}
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

// ── Target and stop computation ───────────────────────────────────────────────

func (e *Engine) fillTargets(a *SymbolAnalysis, bars []indicators.Bar, family string) {
	isBull := isBullishFamily(family)
	direction := "bearish"
	if isBull {
		direction = "bullish"
	}

	base, stretch, ok := indicators.ATRTargetRange(bars, 14, a.ClosePrice, direction)
	if !ok {
		if isBull {
			base, stretch = a.ClosePrice*1.04, a.ClosePrice*1.07
		} else {
			base, stretch = a.ClosePrice*0.96, a.ClosePrice*0.93
		}
	}

	// Prefer nearest structural level over ATR if available
	if isBull {
		if nr := indicators.NearestResistance(bars, a.ClosePrice, 30); nr > 0 {
			base = nr
		}
	} else {
		if ns := indicators.NearestSupport(bars, a.ClosePrice, 30); ns > 0 {
			base = ns
		}
	}

	a.BaseTarget = base
	a.StretchTarget = stretch
	a.Target1 = base
	a.Target2 = stretch

	// ATR-based entry zone and stop
	atr14, atrOK := indicators.ATRLast(bars, 14)
	if atrOK && atr14 > 0 {
		a.EntryLow = a.ClosePrice - 0.25*atr14
		a.EntryHigh = a.ClosePrice + 0.25*atr14
		if isBull {
			a.StopLoss = a.ClosePrice - 1.5*atr14
		} else {
			a.StopLoss = a.ClosePrice + 1.5*atr14
		}
		if a.BaseTarget > 0 && a.StopLoss > 0 && a.ClosePrice > 0 {
			reward := absDiff(a.BaseTarget, a.ClosePrice)
			risk := absDiff(a.StopLoss, a.ClosePrice)
			if risk > 0 {
				a.RRRatio = reward / risk
			}
		}
	}
}

// ── Informational gate fields ─────────────────────────────────────────────────

func (e *Engine) fillGateFields(a *SymbolAnalysis, f Features) {
	r := e.rules

	if f.HasEMA20 && f.HasEMA50 {
		tp := f.EMA20 > f.EMA50
		tr := ""
		if !tp {
			tr = fmt.Sprintf("EMA20 (%.2f) < EMA50 (%.2f)", f.EMA20, f.EMA50)
		}
		a.GateTrend = GateResult{Passed: tp, Reason: tr, Value: f.EMA20}
	} else {
		a.GateTrend = GateResult{Passed: false, Reason: "insufficient data"}
	}

	if f.HasMACD {
		mp := f.MACDHist > 0
		mr := ""
		if !mp {
			mr = fmt.Sprintf("MACD hist (%.4f) <= 0", f.MACDHist)
		}
		a.GateMomentum = GateResult{Passed: mp, Reason: mr, Value: f.MACDHist}
	} else {
		a.GateMomentum = GateResult{Passed: false, Reason: "insufficient data"}
	}

	if f.HasVolumeRatio {
		volMin := 1.2 // global minimum for informational gate
		if len(r.Families) > 0 {
			// Use the lowest entry_conditions.volume_min across families as the display threshold
			for _, fc := range r.Families {
				if fc.EntryConditions.VolumeMin > 0 && fc.EntryConditions.VolumeMin < volMin {
					volMin = fc.EntryConditions.VolumeMin
				}
			}
		}
		vp := f.VolumeRatio >= volMin
		vr := ""
		if !vp {
			vr = fmt.Sprintf("volume ratio (%.2fx) < %.1f", f.VolumeRatio, volMin)
		}
		a.GateVolume = GateResult{Passed: vp, Reason: vr, Value: f.VolumeRatio}
	} else {
		a.GateVolume = GateResult{Passed: false, Reason: "insufficient data"}
	}

	vixMax := r.Regime.HardBlocks.VIXMax
	vixPass := f.VIX < vixMax
	vixReason := ""
	if !vixPass {
		vixReason = fmt.Sprintf("VIX (%.1f) >= %.0f — regime hard gate", f.VIX, vixMax)
	}
	a.GateVIX = GateResult{Passed: vixPass, Reason: vixReason, Value: f.VIX}

	btcMin := r.Regime.HardBlocks.BTCRoc20Min
	btcPass := f.BTCROC20 >= btcMin
	btcReason := ""
	if !btcPass {
		btcReason = fmt.Sprintf("BTC 20d ROC (%.1f%%) < 0", f.BTCROC20)
	}
	a.GateBTC = GateResult{Passed: btcPass, Reason: btcReason, Value: f.BTCROC20}

	if f.HasRSI {
		rp := f.RSI >= 30 && f.RSI <= 80
		rr := ""
		if f.RSI < 30 {
			rr = fmt.Sprintf("RSI (%.1f) < 30", f.RSI)
		} else if f.RSI > 80 {
			rr = fmt.Sprintf("RSI (%.1f) > 80", f.RSI)
		}
		a.GateRSI = GateResult{Passed: rp, Reason: rr, Value: f.RSI}
	} else {
		a.GateRSI = GateResult{Passed: false, Reason: "insufficient data"}
	}
}

// ── Utility ───────────────────────────────────────────────────────────────────

func (e *Engine) reject(a *SymbolAnalysis, reason string, codes ...string) SymbolAnalysis {
	a.Eligible = false
	a.ScreenReason = reason
	a.CandidateStatus = "rejected"
	a.AllGatesPassed = false
	a.RejectReason = reason
	for _, c := range codes {
		a.ReasonCodes = appendUniq(a.ReasonCodes, c)
	}
	return *a
}

// isBullishFamily returns true for the two bullish setup families.
func isBullishFamily(family string) bool {
	return family == "bullish_continuation" || family == "bullish_momentum_breakout"
}

// isContinuationFamily returns true for the two continuation families.
func isContinuationFamily(family string) bool {
	return family == "bullish_continuation" || family == "bearish_continuation"
}

// noMatchReasonCodes infers why no family matched based on computed features.
func noMatchReasonCodes(f Features) []string {
	var codes []string
	if f.HasMACD {
		if f.MACDHist > 0 {
			codes = append(codes, "macd_positive")
		} else {
			codes = append(codes, "macd_negative")
		}
	}
	if f.HasRSI {
		if f.RSI > 72 {
			codes = append(codes, "rsi_extended")
		} else if f.RSI < 28 {
			codes = append(codes, "rsi_too_weak")
		}
	}
	if f.HasEMA20 && f.HasEMA50 {
		if f.EMA20 > f.EMA50 && f.Close <= f.EMA20 {
			codes = append(codes, "below_ema20")
		} else if f.EMA20 < f.EMA50 && f.Close >= f.EMA20 {
			codes = append(codes, "above_ema20")
		}
	}
	if len(codes) == 0 {
		codes = append(codes, "trend_down")
	}
	return codes
}

// classifyTrend returns a human-readable trend bias for informational display.
func classifyTrend(f Features) (bias, reason string) {
	if f.HasEMA20 && f.HasEMA50 {
		if f.EMA20 > f.EMA50 && f.Close > f.EMA20 {
			return "bullish", fmt.Sprintf("EMA20 (%.2f) > EMA50 (%.2f), close above EMA20", f.EMA20, f.EMA50)
		}
		if f.EMA20 < f.EMA50 && f.Close < f.EMA20 {
			return "bearish", fmt.Sprintf("EMA20 (%.2f) < EMA50 (%.2f), close below EMA20", f.EMA20, f.EMA50)
		}
		return "mixed", fmt.Sprintf("EMA20 (%.2f) vs EMA50 (%.2f) — no clear alignment", f.EMA20, f.EMA50)
	}
	return "mixed", "insufficient data for EMA20/EMA50"
}

func sentimentLabel(score float64) string {
	switch {
	case score > 0.2:
		return "positive"
	case score < -0.2:
		return "negative"
	default:
		return "neutral"
	}
}

// lerp performs linear interpolation: returns t ∈ [0,1] mapped to [a,b].
func lerp(a, b, t float64) float64 {
	return a + (b-a)*clamp01(t)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// appendUniq appends each string to the slice only if not already present.
func appendUniq(slice []string, values ...string) []string {
	for _, v := range values {
		found := false
		for _, s := range slice {
			if s == v {
				found = true
				break
			}
		}
		if !found {
			slice = append(slice, v)
		}
	}
	return slice
}

// RegimeLabel returns a human-readable market regime label.
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

// ShortlistEntryReady filters and ranks entry_ready candidates for Claude confirmation.
//
// Selection rules (all configurable via Rules.TradeFrequency):
//  1. Only entry_ready status — structural_candidate and others are excluded.
//  2. FinalScore >= minScore — weak candidates are filtered before Claude sees them.
//  3. Results are sorted by FinalScore descending.
//  4. The top maxCount candidates are returned.
//
// The returned slice is what gets sent to RunOpeningConfirmationActivity.
// Claude is the final authority; this function is the pre-filter only.
func ShortlistEntryReady(analyses []SymbolAnalysis, maxCount int, minScore float64) []SymbolAnalysis {
	// Filter
	var candidates []SymbolAnalysis
	for _, a := range analyses {
		if a.CandidateStatus == "entry_ready" && a.ScoreBreakdown.FinalScore >= minScore {
			candidates = append(candidates, a)
		}
	}
	// Sort by FinalScore descending (insertion sort is fine for ≤26 symbols)
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].ScoreBreakdown.FinalScore > candidates[j-1].ScoreBreakdown.FinalScore; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
	// Trim to maxCount
	if maxCount > 0 && len(candidates) > maxCount {
		candidates = candidates[:maxCount]
	}
	return candidates
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
