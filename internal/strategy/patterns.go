// internal/strategy/patterns.go
//
// WHAT: Structural chart-pattern detector for the RSVE-O strategy.
//       Detects three pattern families in both bullish and bearish directions:
//         1. Bull Flag / Bear Flag
//         2. Ascending Triangle / Descending Triangle
//         3. Base Breakout / Base Breakdown
//
// WHY:  Indicator-based gates (EMA, BB, RS, breakout, volume) confirm that a
//       setup has the right conditions, but they don't verify structural price
//       action — the shape of consolidation before the breakout. Structural
//       patterns add a second layer of evidence that improves ranking quality.
//
// HOW:  All detectors operate on []indicators.Bar (oldest first, newest last).
//       The last bar is always the candidate breakout/breakdown bar.
//       Detectors return *PatternResult (nil = not detected).
//       AnalyzePatterns tries all enabled patterns for the given direction.
//
// WHAT BREAKS: Patterns require a minimum bar count. Fewer bars → nil.
//              Volume data of 0 skips volume checks (treated as unavailable).
//
// NOT SUPPORTED (intentionally excluded):
//   - Head-and-shoulders (requires subjective shoulder symmetry)
//   - Cup-and-handle (requires multi-month data and rounded bottom)
//   - Candlestick patterns (one-bar signals, too noisy)
//   - MACD-cross patterns (indicator cross, not structural price action)
//
// VERIFY: go test ./internal/strategy/ -run TestPattern -v

package strategy

import (
	"fmt"
	"math"

	"github.com/yourname/makemytrade/internal/indicators"
)

// ── Public types ──────────────────────────────────────────────────────────────

// PatternResult is the structured output of one pattern detection.
type PatternResult struct {
	PatternName        string   `json:"pattern_name"`
	Direction          string   `json:"direction"`            // "bullish" | "bearish"
	QualityScore       float64  `json:"quality_score"`        // 0.0–1.0
	BreakoutLevel      float64  `json:"breakout_level"`       // price level of the breakout
	InvalidationLevel  float64  `json:"invalidation_level"`   // price level that voids the pattern
	ImpulseStart       int      `json:"impulse_start"`        // bar index (0-based)
	ImpulseEnd         int      `json:"impulse_end"`          // bar index
	ConsolidationStart int      `json:"consolidation_start"`  // bar index
	ConsolidationEnd   int      `json:"consolidation_end"`    // bar index (inclusive)
	Reasons            []string `json:"reasons"`              // diagnostic details
}

// PatternAnalysis holds the full pattern analysis result for one ticker.
type PatternAnalysis struct {
	Detected    bool            `json:"detected"`     // true when a pattern with quality >= minimum is found
	BestPattern *PatternResult  `json:"best_pattern"` // highest quality result, nil when none detected
	AllPatterns []PatternResult `json:"all_patterns"` // all found patterns (including below-minimum quality)
	Reasons     []string        `json:"reasons"`      // "no_pattern_detected" etc.

	// PatternState is set after evaluating the breakout gate in evaluateBranch:
	//   "no_pattern"                — no pattern detected
	//   "pattern_forming"           — pattern detected but breakout not yet confirmed
	//   "pattern_breakout_confirmed"— pattern detected AND breakout gate passed
	PatternState string `json:"pattern_state"`
}

// ── Public API ────────────────────────────────────────────────────────────────

// AnalyzePatterns runs all enabled pattern detectors for the given direction
// and returns the aggregated result.
func AnalyzePatterns(bars []indicators.Bar, direction string, cfg PatternAnalysisConfig) PatternAnalysis {
	if !cfg.Enabled {
		return PatternAnalysis{Reasons: []string{"pattern_analysis_disabled"}}
	}

	allowed := make(map[string]bool, len(cfg.AllowedPatterns))
	for _, p := range cfg.AllowedPatterns {
		allowed[p] = true
	}

	type detector func([]indicators.Bar) *PatternResult

	var detectors []detector
	if direction == "bullish" {
		if allowed["bull_flag"] {
			detectors = append(detectors, detectBullFlag)
		}
		if allowed["ascending_triangle"] {
			detectors = append(detectors, detectAscendingTriangle)
		}
		if allowed["base_breakout"] {
			detectors = append(detectors, detectBaseBreakout)
		}
		if allowed["support_resistance_retest"] {
			detectors = append(detectors, detectSupportResistanceRetestBullish)
		}
	} else {
		if allowed["bear_flag"] {
			detectors = append(detectors, detectBearFlag)
		}
		if allowed["descending_triangle"] {
			detectors = append(detectors, detectDescendingTriangle)
		}
		if allowed["base_breakdown"] {
			detectors = append(detectors, detectBaseBreakdown)
		}
		if allowed["support_resistance_retest"] {
			detectors = append(detectors, detectSupportResistanceRetestBearish)
		}
	}

	result := PatternAnalysis{}
	for _, detect := range detectors {
		pr := detect(bars)
		if pr == nil {
			continue
		}
		result.AllPatterns = append(result.AllPatterns, *pr)
		if result.BestPattern == nil || pr.QualityScore > result.BestPattern.QualityScore {
			cp := *pr
			result.BestPattern = &cp
		}
	}

	minQ := cfg.MinimumPatternQuality
	if minQ <= 0 {
		minQ = 0.60
	}
	if result.BestPattern != nil && result.BestPattern.QualityScore >= minQ {
		result.Detected = true
	}

	if !result.Detected {
		result.Reasons = []string{"no_pattern_detected"}
	}
	return result
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// swingPoint stores an index and price for a local swing high or low.
type swingPoint struct {
	Idx   int
	Price float64
}

// swingHighPoints returns all 3-bar swing highs in bars[start:end].
// A swing high at i: bars[i].High > bars[i-1].High AND bars[i].High > bars[i+1].High.
func swingHighPoints(bars []indicators.Bar, start, end int) []swingPoint {
	var out []swingPoint
	for i := start + 1; i < end-1; i++ {
		if bars[i].High > bars[i-1].High && bars[i].High > bars[i+1].High {
			out = append(out, swingPoint{i, bars[i].High})
		}
	}
	return out
}

// swingLowPoints returns all 3-bar swing lows in bars[start:end].
func swingLowPoints(bars []indicators.Bar, start, end int) []swingPoint {
	var out []swingPoint
	for i := start + 1; i < end-1; i++ {
		if bars[i].Low < bars[i-1].Low && bars[i].Low < bars[i+1].Low {
			out = append(out, swingPoint{i, bars[i].Low})
		}
	}
	return out
}

// avgBarVolume returns the mean volume of a bar slice. Returns 0 if empty.
func avgBarVolume(bars []indicators.Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	var sum float64
	for _, b := range bars {
		sum += b.Volume
	}
	return sum / float64(len(bars))
}

// minBarLow returns the minimum Low in a bar slice.
func minBarLow(bars []indicators.Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	m := bars[0].Low
	for _, b := range bars[1:] {
		if b.Low < m {
			m = b.Low
		}
	}
	return m
}

// maxBarHigh returns the maximum High in a bar slice.
func maxBarHigh(bars []indicators.Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	m := bars[0].High
	for _, b := range bars[1:] {
		if b.High > m {
			m = b.High
		}
	}
	return m
}

// maxBarClose returns the maximum Close in a bar slice.
func maxBarClose(bars []indicators.Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	m := bars[0].Close
	for _, b := range bars[1:] {
		if b.Close > m {
			m = b.Close
		}
	}
	return m
}

// minBarClose returns the minimum Close in a bar slice.
func minBarClose(bars []indicators.Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	m := bars[0].Close
	for _, b := range bars[1:] {
		if b.Close < m {
			m = b.Close
		}
	}
	return m
}

// ── Pattern detectors ─────────────────────────────────────────────────────────

// detectBullFlag detects a bull flag pattern.
//
// Structure: [pole: 5-10 bar impulse up ≥5%] [flag: 3-8 bar pullback ≤50% of pole] [today: breakout close > flag high]
//
// Quality drivers: shallow pullback, contracting flag volume, strong breakout volume, large pole.
func detectBullFlag(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 12 {
		return nil
	}

	closes := indicators.Closes(bars)
	volumes := indicators.Volumes(bars)

	atr, hasATR := indicators.ATRLast(bars, 14)
	ema20, hasEMA20 := indicators.EMALast(closes, 20)
	avgVol20 := indicators.AvgDailyVolume(volumes, 20)

	today := bars[n-1]

	// Breakout volume check (skip when volume data unavailable)
	if avgVol20 > 0 && today.Volume < avgVol20*1.2 {
		return nil
	}

	var best *PatternResult

	for flagLen := 3; flagLen <= 8; flagLen++ {
		// flag = bars[flagStart .. n-2]; today = bars[n-1]
		flagStart := n - 1 - flagLen
		if flagStart < 5 {
			continue
		}
		flagBars := bars[flagStart : n-1]

		flagHigh := maxBarHigh(flagBars)
		flagLow := minBarLow(flagBars)
		flagMinClose := minBarClose(flagBars)

		// Breakout: today closes above flag high
		if today.Close <= flagHigh {
			continue
		}

		avgFlagVol := avgBarVolume(flagBars)

		for poleLen := 5; poleLen <= 10; poleLen++ {
			poleEnd := flagStart - 1
			poleStart := poleEnd - poleLen + 1
			if poleStart < 0 {
				continue
			}

			poleBars := bars[poleStart : poleEnd+1]
			poleOpen := poleBars[0].Open
			poleClose := poleBars[len(poleBars)-1].Close
			if poleOpen <= 0 {
				continue
			}

			poleGainPct := (poleClose - poleOpen) / poleOpen * 100

			atrGain := 0.0
			if hasATR && atr > 0 {
				atrGain = (poleClose - poleOpen) / atr
			}

			// Impulse requirement: ≥5% price gain OR ≥1.5 ATR move
			if poleGainPct < 5.0 && (!hasATR || atrGain < 1.5) {
				continue
			}

			// Pullback must not retrace more than 50% of pole gain
			poleGainAmount := poleClose - poleOpen
			if poleGainAmount <= 0 {
				continue
			}
			pullbackAmount := poleClose - flagMinClose
			pullbackDepth := pullbackAmount / poleGainAmount
			if pullbackDepth > 0.50 {
				continue
			}

			// Flag must not break below EMA20 (within 2% tolerance)
			if hasEMA20 && ema20 > 0 && flagLow < ema20*0.98 {
				continue
			}

			avgPoleVol := avgBarVolume(poleBars)

			// Quality score
			q := 0.50
			if avgPoleVol > 0 && avgFlagVol < avgPoleVol*0.70 {
				q += 0.15 // volume contraction in flag — bullish
			}
			switch {
			case pullbackDepth < 0.30:
				q += 0.15
			case pullbackDepth < 0.40:
				q += 0.08
			}
			switch {
			case poleGainPct >= 10.0:
				q += 0.10
			case poleGainPct >= 7.0:
				q += 0.05
			}
			if avgVol20 > 0 && today.Volume >= avgVol20*2.0 {
				q += 0.10
			}

			pr := &PatternResult{
				PatternName:        "bull_flag",
				Direction:          "bullish",
				QualityScore:       math.Min(q, 0.95),
				BreakoutLevel:      flagHigh,
				InvalidationLevel:  flagLow,
				ImpulseStart:       poleStart,
				ImpulseEnd:         poleEnd,
				ConsolidationStart: flagStart,
				ConsolidationEnd:   n - 2,
				Reasons: []string{
					fmt.Sprintf("pole_gain=%.1f%%", poleGainPct),
					fmt.Sprintf("pullback_depth=%.0f%%", pullbackDepth*100),
					fmt.Sprintf("flag_vol_ratio=%.2f", safeDivide(avgFlagVol, avgPoleVol)),
				},
			}
			if best == nil || pr.QualityScore > best.QualityScore {
				best = pr
			}
		}
	}
	return best
}

// detectBearFlag detects a bear flag pattern (mirror of bull flag).
//
// Structure: [pole: 5-10 bar impulse down ≥5%] [flag: 3-8 bar consolidation ≤50% recovery] [today: breakdown close < flag low]
func detectBearFlag(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 12 {
		return nil
	}

	closes := indicators.Closes(bars)
	volumes := indicators.Volumes(bars)

	atr, hasATR := indicators.ATRLast(bars, 14)
	ema20, hasEMA20 := indicators.EMALast(closes, 20)
	avgVol20 := indicators.AvgDailyVolume(volumes, 20)

	today := bars[n-1]

	if avgVol20 > 0 && today.Volume < avgVol20*1.2 {
		return nil
	}

	var best *PatternResult

	for flagLen := 3; flagLen <= 8; flagLen++ {
		flagStart := n - 1 - flagLen
		if flagStart < 5 {
			continue
		}
		flagBars := bars[flagStart : n-1]

		flagHigh := maxBarHigh(flagBars)
		flagLow := minBarLow(flagBars)
		flagMaxClose := maxBarClose(flagBars)

		// Breakdown: today closes below flag low
		if today.Close >= flagLow {
			continue
		}

		avgFlagVol := avgBarVolume(flagBars)

		for poleLen := 5; poleLen <= 10; poleLen++ {
			poleEnd := flagStart - 1
			poleStart := poleEnd - poleLen + 1
			if poleStart < 0 {
				continue
			}

			poleBars := bars[poleStart : poleEnd+1]
			poleOpen := poleBars[0].Open
			poleClose := poleBars[len(poleBars)-1].Close
			if poleOpen <= 0 {
				continue
			}

			poleDeclinePct := (poleOpen - poleClose) / poleOpen * 100

			atrDecline := 0.0
			if hasATR && atr > 0 {
				atrDecline = (poleOpen - poleClose) / atr
			}

			if poleDeclinePct < 5.0 && (!hasATR || atrDecline < 1.5) {
				continue
			}

			poleDeclineAmount := poleOpen - poleClose
			if poleDeclineAmount <= 0 {
				continue
			}
			recoveryAmount := flagMaxClose - poleClose
			recoveryDepth := recoveryAmount / poleDeclineAmount
			if recoveryDepth > 0.50 {
				continue
			}

			// Flag must not break above EMA20 (within 2% tolerance)
			if hasEMA20 && ema20 > 0 && flagHigh > ema20*1.02 {
				continue
			}

			avgPoleVol := avgBarVolume(poleBars)

			q := 0.50
			if avgPoleVol > 0 && avgFlagVol < avgPoleVol*0.70 {
				q += 0.15
			}
			switch {
			case recoveryDepth < 0.30:
				q += 0.15
			case recoveryDepth < 0.40:
				q += 0.08
			}
			switch {
			case poleDeclinePct >= 10.0:
				q += 0.10
			case poleDeclinePct >= 7.0:
				q += 0.05
			}
			if avgVol20 > 0 && today.Volume >= avgVol20*2.0 {
				q += 0.10
			}

			pr := &PatternResult{
				PatternName:        "bear_flag",
				Direction:          "bearish",
				QualityScore:       math.Min(q, 0.95),
				BreakoutLevel:      flagLow,
				InvalidationLevel:  flagHigh,
				ImpulseStart:       poleStart,
				ImpulseEnd:         poleEnd,
				ConsolidationStart: flagStart,
				ConsolidationEnd:   n - 2,
				Reasons: []string{
					fmt.Sprintf("pole_decline=%.1f%%", poleDeclinePct),
					fmt.Sprintf("recovery_depth=%.0f%%", recoveryDepth*100),
					fmt.Sprintf("flag_vol_ratio=%.2f", safeDivide(avgFlagVol, avgPoleVol)),
				},
			}
			if best == nil || pr.QualityScore > best.QualityScore {
				best = pr
			}
		}
	}
	return best
}

// detectAscendingTriangle detects an ascending triangle pattern.
//
// Structure: 10-30 bar consolidation with flat resistance (≥2 swing highs within 1.5%)
// and rising swing lows (≥2 higher lows). Today breaks above resistance with volume.
func detectAscendingTriangle(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 15 {
		return nil
	}

	volumes := indicators.Volumes(bars)
	avgVol20 := indicators.AvgDailyVolume(volumes, 20)
	today := bars[n-1]

	if avgVol20 > 0 && today.Volume < avgVol20*1.2 {
		return nil
	}

	var best *PatternResult

	for lookback := 10; lookback <= 30; lookback++ {
		if lookback >= n {
			continue
		}
		patStart := n - 1 - lookback // pattern bars: patStart .. n-2

		shs := swingHighPoints(bars, patStart, n-1)
		if len(shs) < 2 {
			continue
		}

		// Resistance: swing highs must be relatively flat (within 1.5%)
		maxH := shs[0].Price
		minH := shs[0].Price
		for _, sp := range shs[1:] {
			if sp.Price > maxH {
				maxH = sp.Price
			}
			if sp.Price < minH {
				minH = sp.Price
			}
		}
		if minH <= 0 {
			continue
		}
		highRange := (maxH - minH) / minH
		if highRange > 0.015 {
			continue
		}
		resistance := (maxH + minH) / 2

		// Breakout: today closes above resistance
		if today.Close <= resistance {
			continue
		}

		sls := swingLowPoints(bars, patStart, n-1)
		if len(sls) < 2 {
			continue
		}

		// Higher lows: each swing low must be above the previous
		higherLows := true
		for i := 1; i < len(sls); i++ {
			if sls[i].Price <= sls[i-1].Price {
				higherLows = false
				break
			}
		}
		if !higherLows {
			continue
		}

		// Volume contraction: latter half of pattern has lower avg volume
		patBars := bars[patStart : n-1]
		mid := len(patBars) / 2
		firstHalf := patBars[:mid]
		secondHalf := patBars[mid:]
		avgFirst := avgBarVolume(firstHalf)
		avgSecond := avgBarVolume(secondHalf)
		volContracted := avgFirst > 0 && avgSecond < avgFirst

		support := sls[len(sls)-1].Price // most recent swing low

		q := 0.55
		if volContracted {
			q += 0.15
		}
		if highRange < 0.005 {
			q += 0.10 // very flat resistance
		}
		if len(shs) >= 3 {
			q += 0.05
		}
		if len(sls) >= 3 {
			q += 0.05
		}
		if avgVol20 > 0 && today.Volume >= avgVol20*1.5 {
			q += 0.10
		}

		pr := &PatternResult{
			PatternName:        "ascending_triangle",
			Direction:          "bullish",
			QualityScore:       math.Min(q, 0.95),
			BreakoutLevel:      resistance,
			InvalidationLevel:  support,
			ImpulseStart:       patStart,
			ImpulseEnd:         n - 2,
			ConsolidationStart: patStart,
			ConsolidationEnd:   n - 2,
			Reasons: []string{
				fmt.Sprintf("resistance=%.2f high_range=%.2f%%", resistance, highRange*100),
				fmt.Sprintf("swing_highs=%d swing_lows=%d", len(shs), len(sls)),
				fmt.Sprintf("vol_contracted=%v", volContracted),
			},
		}
		if best == nil || pr.QualityScore > best.QualityScore {
			best = pr
		}
	}
	return best
}

// detectDescendingTriangle detects a descending triangle pattern (mirror of ascending).
//
// Structure: 10-30 bar consolidation with flat support (≥2 swing lows within 1.5%)
// and falling swing highs (≥2 lower highs). Today breaks below support with volume.
func detectDescendingTriangle(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 15 {
		return nil
	}

	volumes := indicators.Volumes(bars)
	avgVol20 := indicators.AvgDailyVolume(volumes, 20)
	today := bars[n-1]

	if avgVol20 > 0 && today.Volume < avgVol20*1.2 {
		return nil
	}

	var best *PatternResult

	for lookback := 10; lookback <= 30; lookback++ {
		if lookback >= n {
			continue
		}
		patStart := n - 1 - lookback

		sls := swingLowPoints(bars, patStart, n-1)
		if len(sls) < 2 {
			continue
		}

		// Support: swing lows must be relatively flat (within 1.5%)
		maxL := sls[0].Price
		minL := sls[0].Price
		for _, sp := range sls[1:] {
			if sp.Price > maxL {
				maxL = sp.Price
			}
			if sp.Price < minL {
				minL = sp.Price
			}
		}
		if minL <= 0 {
			continue
		}
		lowRange := (maxL - minL) / minL
		if lowRange > 0.015 {
			continue
		}
		support := (maxL + minL) / 2

		// Breakdown: today closes below support
		if today.Close >= support {
			continue
		}

		shs := swingHighPoints(bars, patStart, n-1)
		if len(shs) < 2 {
			continue
		}

		// Lower highs: each swing high must be below the previous
		lowerHighs := true
		for i := 1; i < len(shs); i++ {
			if shs[i].Price >= shs[i-1].Price {
				lowerHighs = false
				break
			}
		}
		if !lowerHighs {
			continue
		}

		patBars := bars[patStart : n-1]
		mid := len(patBars) / 2
		avgFirst := avgBarVolume(patBars[:mid])
		avgSecond := avgBarVolume(patBars[mid:])
		volContracted := avgFirst > 0 && avgSecond < avgFirst

		resistance := shs[len(shs)-1].Price

		q := 0.55
		if volContracted {
			q += 0.15
		}
		if lowRange < 0.005 {
			q += 0.10
		}
		if len(sls) >= 3 {
			q += 0.05
		}
		if len(shs) >= 3 {
			q += 0.05
		}
		if avgVol20 > 0 && today.Volume >= avgVol20*1.5 {
			q += 0.10
		}

		pr := &PatternResult{
			PatternName:        "descending_triangle",
			Direction:          "bearish",
			QualityScore:       math.Min(q, 0.95),
			BreakoutLevel:      support,
			InvalidationLevel:  resistance,
			ImpulseStart:       patStart,
			ImpulseEnd:         n - 2,
			ConsolidationStart: patStart,
			ConsolidationEnd:   n - 2,
			Reasons: []string{
				fmt.Sprintf("support=%.2f low_range=%.2f%%", support, lowRange*100),
				fmt.Sprintf("swing_highs=%d swing_lows=%d", len(shs), len(sls)),
				fmt.Sprintf("vol_contracted=%v", volContracted),
			},
		}
		if best == nil || pr.QualityScore > best.QualityScore {
			best = pr
		}
	}
	return best
}

// detectBaseBreakout detects a base breakout pattern.
//
// Structure: 15-40 bar sideways range (width ≤12%) followed by today's close
// above the base high with strong volume (≥1.3x avg). BB squeeze preferred.
func detectBaseBreakout(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 20 {
		return nil
	}

	closes := indicators.Closes(bars)
	volumes := indicators.Volumes(bars)

	avgVol20 := indicators.AvgDailyVolume(volumes, 20)
	today := bars[n-1]

	// Base breakout requires stronger volume confirmation
	if avgVol20 > 0 && today.Volume < avgVol20*1.3 {
		return nil
	}

	bbPct, hasBB := indicators.BollingerWidthPercentile(closes[:n-1], 20, 63, 2.0)

	var best *PatternResult

	for baseLen := 15; baseLen <= 40; baseLen++ {
		if baseLen >= n {
			continue
		}
		baseStart := n - 1 - baseLen
		baseBars := bars[baseStart : n-1]

		baseHigh := maxBarHigh(baseBars)
		baseLow := minBarLow(baseBars)

		mid := (baseHigh + baseLow) / 2
		if mid <= 0 {
			continue
		}
		rangeWidth := (baseHigh - baseLow) / mid
		if rangeWidth > 0.12 {
			continue
		}

		// Breakout: today closes above base high
		if today.Close <= baseHigh {
			continue
		}

		q := 0.55
		switch {
		case rangeWidth < 0.06:
			q += 0.15 // very tight base
		case rangeWidth < 0.09:
			q += 0.08
		}
		if hasBB && bbPct <= 0.30 {
			q += 0.10 // BB compression confirms squeeze
			if bbPct < 0.15 {
				q += 0.05 // extreme compression
			}
		}
		if avgVol20 > 0 && today.Volume >= avgVol20*2.0 {
			q += 0.10
		}
		if baseLen >= 20 {
			q += 0.05 // longer base = more reliable
		}

		pr := &PatternResult{
			PatternName:        "base_breakout",
			Direction:          "bullish",
			QualityScore:       math.Min(q, 0.95),
			BreakoutLevel:      baseHigh,
			InvalidationLevel:  baseLow,
			ImpulseStart:       baseStart,
			ImpulseEnd:         n - 2,
			ConsolidationStart: baseStart,
			ConsolidationEnd:   n - 2,
			Reasons: []string{
				fmt.Sprintf("base_range=%.1f%%", rangeWidth*100),
				fmt.Sprintf("base_bars=%d", baseLen),
				fmt.Sprintf("bb_pct=%s", fmtBBPct(hasBB, bbPct)),
			},
		}
		if best == nil || pr.QualityScore > best.QualityScore {
			best = pr
		}
	}
	return best
}

// detectBaseBreakdown detects a base breakdown pattern (mirror of base breakout).
func detectBaseBreakdown(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 20 {
		return nil
	}

	closes := indicators.Closes(bars)
	volumes := indicators.Volumes(bars)

	avgVol20 := indicators.AvgDailyVolume(volumes, 20)
	today := bars[n-1]

	if avgVol20 > 0 && today.Volume < avgVol20*1.3 {
		return nil
	}

	bbPct, hasBB := indicators.BollingerWidthPercentile(closes[:n-1], 20, 63, 2.0)

	var best *PatternResult

	for baseLen := 15; baseLen <= 40; baseLen++ {
		if baseLen >= n {
			continue
		}
		baseStart := n - 1 - baseLen
		baseBars := bars[baseStart : n-1]

		baseHigh := maxBarHigh(baseBars)
		baseLow := minBarLow(baseBars)

		mid := (baseHigh + baseLow) / 2
		if mid <= 0 {
			continue
		}
		rangeWidth := (baseHigh - baseLow) / mid
		if rangeWidth > 0.12 {
			continue
		}

		// Breakdown: today closes below base low
		if today.Close >= baseLow {
			continue
		}

		q := 0.55
		switch {
		case rangeWidth < 0.06:
			q += 0.15
		case rangeWidth < 0.09:
			q += 0.08
		}
		if hasBB && bbPct <= 0.30 {
			q += 0.10
			if bbPct < 0.15 {
				q += 0.05
			}
		}
		if avgVol20 > 0 && today.Volume >= avgVol20*2.0 {
			q += 0.10
		}
		if baseLen >= 20 {
			q += 0.05
		}

		pr := &PatternResult{
			PatternName:        "base_breakdown",
			Direction:          "bearish",
			QualityScore:       math.Min(q, 0.95),
			BreakoutLevel:      baseLow,
			InvalidationLevel:  baseHigh,
			ImpulseStart:       baseStart,
			ImpulseEnd:         n - 2,
			ConsolidationStart: baseStart,
			ConsolidationEnd:   n - 2,
			Reasons: []string{
				fmt.Sprintf("base_range=%.1f%%", rangeWidth*100),
				fmt.Sprintf("base_bars=%d", baseLen),
				fmt.Sprintf("bb_pct=%s", fmtBBPct(hasBB, bbPct)),
			},
		}
		if best == nil || pr.QualityScore > best.QualityScore {
			best = pr
		}
	}
	return best
}

// detectSupportResistanceRetestBullish detects a bullish S/R retest.
//
// Structure: price previously broke above a prior swing high (resistance),
// then pulled back to retest that level from above. Today's bar touches
// the level (low within 2%) and closes above it (closed on support).
//
// Quality drivers: closeness of retest touch, volume contraction on pullback,
// strong close above the level relative to the day's range.
func detectSupportResistanceRetestBullish(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 15 {
		return nil
	}

	today := bars[n-1]
	volumes := indicators.Volumes(bars)
	avgVol20 := indicators.AvgDailyVolume(volumes, 20)

	// Look for a prior swing high in bars[n-40..n-6] that today retests.
	lookStart := n - 40
	if lookStart < 0 {
		lookStart = 0
	}
	shs := swingHighPoints(bars, lookStart, n-5)
	if len(shs) == 0 {
		return nil
	}

	var best *PatternResult
	for _, sh := range shs {
		level := sh.Price
		if level <= 0 {
			continue
		}

		// Retest: today's low touches the level (within 2%) and closes above it.
		touchedLevel := today.Low <= level*1.02 && today.Low >= level*0.97
		closedAbove := today.Close >= level*0.99
		if !touchedLevel || !closedAbove {
			continue
		}

		// Confirm breakout: at least one bar between sh.Idx and n-2 closed above the level.
		breakoutConfirmed := false
		for i := sh.Idx + 1; i < n-1; i++ {
			if bars[i].Close > level {
				breakoutConfirmed = true
				break
			}
		}
		if !breakoutConfirmed {
			continue
		}

		// Pullback bars: from breakout to today, volume should be contracting.
		pullbackBars := bars[sh.Idx+1 : n-1]
		avgPullbackVol := avgBarVolume(pullbackBars)
		volContracted := avgVol20 > 0 && avgPullbackVol < avgVol20*0.8

		// Quality: how cleanly today closed relative to the level.
		closeAbovePct := (today.Close - level) / level * 100
		touchDepthPct := (level - today.Low) / level * 100

		q := 0.55
		if volContracted {
			q += 0.10
		}
		if touchDepthPct < 0.5 {
			q += 0.15 // very clean touch — barely dipped below
		} else if touchDepthPct < 1.0 {
			q += 0.08
		}
		if closeAbovePct >= 0.5 {
			q += 0.10 // strong close above the level
		}
		if avgVol20 > 0 && today.Volume >= avgVol20*1.3 {
			q += 0.10 // bounce bar has above-average volume
		}

		pr := &PatternResult{
			PatternName:        "support_resistance_retest",
			Direction:          "bullish",
			QualityScore:       math.Min(q, 0.95),
			BreakoutLevel:      level,
			InvalidationLevel:  level * 0.97,
			ImpulseStart:       sh.Idx,
			ImpulseEnd:         sh.Idx,
			ConsolidationStart: sh.Idx + 1,
			ConsolidationEnd:   n - 2,
			Reasons: []string{
				fmt.Sprintf("level=%.2f touch_depth=%.2f%% close_above=%.2f%%", level, touchDepthPct, closeAbovePct),
				fmt.Sprintf("vol_contracted=%v", volContracted),
			},
		}
		if best == nil || pr.QualityScore > best.QualityScore {
			best = pr
		}
	}
	return best
}

// detectSupportResistanceRetestBearish detects a bearish S/R retest.
//
// Structure: price broke below a prior swing low (support), then bounced
// back to test that level from below. Today's bar touches the level from
// below (high within 2%) and closes below it (rejected at resistance).
func detectSupportResistanceRetestBearish(bars []indicators.Bar) *PatternResult {
	n := len(bars)
	if n < 15 {
		return nil
	}

	today := bars[n-1]
	volumes := indicators.Volumes(bars)
	avgVol20 := indicators.AvgDailyVolume(volumes, 20)

	lookStart := n - 40
	if lookStart < 0 {
		lookStart = 0
	}
	sls := swingLowPoints(bars, lookStart, n-5)
	if len(sls) == 0 {
		return nil
	}

	var best *PatternResult
	for _, sl := range sls {
		level := sl.Price
		if level <= 0 {
			continue
		}

		touchedLevel := today.High >= level*0.98 && today.High <= level*1.03
		closedBelow := today.Close <= level*1.01
		if !touchedLevel || !closedBelow {
			continue
		}

		breakdownConfirmed := false
		for i := sl.Idx + 1; i < n-1; i++ {
			if bars[i].Close < level {
				breakdownConfirmed = true
				break
			}
		}
		if !breakdownConfirmed {
			continue
		}

		pullbackBars := bars[sl.Idx+1 : n-1]
		avgPullbackVol := avgBarVolume(pullbackBars)
		volContracted := avgVol20 > 0 && avgPullbackVol < avgVol20*0.8

		touchHeightPct := (today.High - level) / level * 100
		closeBelowPct := (level - today.Close) / level * 100

		q := 0.55
		if volContracted {
			q += 0.10
		}
		if touchHeightPct < 0.5 {
			q += 0.15
		} else if touchHeightPct < 1.0 {
			q += 0.08
		}
		if closeBelowPct >= 0.5 {
			q += 0.10
		}
		if avgVol20 > 0 && today.Volume >= avgVol20*1.3 {
			q += 0.10
		}

		pr := &PatternResult{
			PatternName:        "support_resistance_retest",
			Direction:          "bearish",
			QualityScore:       math.Min(q, 0.95),
			BreakoutLevel:      level,
			InvalidationLevel:  level * 1.03,
			ImpulseStart:       sl.Idx,
			ImpulseEnd:         sl.Idx,
			ConsolidationStart: sl.Idx + 1,
			ConsolidationEnd:   n - 2,
			Reasons: []string{
				fmt.Sprintf("level=%.2f touch_height=%.2f%% close_below=%.2f%%", level, touchHeightPct, closeBelowPct),
				fmt.Sprintf("vol_contracted=%v", volContracted),
			},
		}
		if best == nil || pr.QualityScore > best.QualityScore {
			best = pr
		}
	}
	return best
}

// AnalyzeORBPattern detects an Opening Range Breakout on 5-minute intraday bars.
//
// The Opening Range is defined by the first nORBars 5-minute candles (default 6 = first 30 min).
// Bullish ORB: a subsequent bar closes above OR High with above-average intraday volume.
// Bearish ORB: a subsequent bar closes below OR Low.
//
// bars5m must be sorted oldest-first. Each bar is one 5-minute candle.
// Returns nil when fewer than nORBars+1 bars are available.
func AnalyzeORBPattern(bars5m []indicators.Bar, direction string, nORBars int) *PatternResult {
	if nORBars <= 0 {
		nORBars = 6
	}
	n := len(bars5m)
	if n <= nORBars {
		return nil
	}

	orBars := bars5m[:nORBars]
	orHigh := maxBarHigh(orBars)
	orLow := minBarLow(orBars)
	orAvgVol := avgBarVolume(orBars)

	// Scan post-OR bars for the first breakout/breakdown confirmation.
	for i := nORBars; i < n; i++ {
		bar := bars5m[i]
		volOK := orAvgVol <= 0 || bar.Volume >= orAvgVol*1.2

		if direction == "bullish" && bar.Close > orHigh && volOK {
			breakoutPct := (bar.Close - orHigh) / orHigh * 100
			q := 0.60
			if breakoutPct >= 0.5 {
				q += 0.10
			}
			if orAvgVol > 0 && bar.Volume >= orAvgVol*2.0 {
				q += 0.15
			}
			orRange := (orHigh - orLow) / ((orHigh + orLow) / 2) * 100
			if orRange < 1.0 {
				q += 0.10 // tight OR = cleaner breakout
			}
			return &PatternResult{
				PatternName:        "opening_range_breakout",
				Direction:          "bullish",
				QualityScore:       math.Min(q, 0.95),
				BreakoutLevel:      orHigh,
				InvalidationLevel:  orLow,
				ImpulseStart:       0,
				ImpulseEnd:         nORBars - 1,
				ConsolidationStart: 0,
				ConsolidationEnd:   nORBars - 1,
				Reasons: []string{
					fmt.Sprintf("or_high=%.2f or_low=%.2f range=%.2f%%", orHigh, orLow, orRange),
					fmt.Sprintf("breakout_bar=%d breakout_pct=%.2f%%", i, breakoutPct),
				},
			}
		}

		if direction == "bearish" && bar.Close < orLow && volOK {
			breakdownPct := (orLow - bar.Close) / orLow * 100
			q := 0.60
			if breakdownPct >= 0.5 {
				q += 0.10
			}
			if orAvgVol > 0 && bar.Volume >= orAvgVol*2.0 {
				q += 0.15
			}
			orRange := (orHigh - orLow) / ((orHigh + orLow) / 2) * 100
			if orRange < 1.0 {
				q += 0.10
			}
			return &PatternResult{
				PatternName:        "opening_range_breakout",
				Direction:          "bearish",
				QualityScore:       math.Min(q, 0.95),
				BreakoutLevel:      orLow,
				InvalidationLevel:  orHigh,
				ImpulseStart:       0,
				ImpulseEnd:         nORBars - 1,
				ConsolidationStart: 0,
				ConsolidationEnd:   nORBars - 1,
				Reasons: []string{
					fmt.Sprintf("or_high=%.2f or_low=%.2f range=%.2f%%", orHigh, orLow, orRange),
					fmt.Sprintf("breakdown_bar=%d breakdown_pct=%.2f%%", i, breakdownPct),
				},
			}
		}
	}
	return nil
}

// ── Formatting helpers ────────────────────────────────────────────────────────

func safeDivide(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func fmtBBPct(has bool, pct float64) string {
	if !has {
		return "unavailable"
	}
	return fmt.Sprintf("%.0f%%ile", pct*100)
}
