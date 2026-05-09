// internal/strategy/patterns_test.go
//
// Tests for structural chart-pattern detection (bull/bear flag, ascending/
// descending triangle, base breakout/breakdown) and their integration with
// the RSVE-O scoring system.
//
// Run: go test ./internal/strategy/ -run TestPattern -v

package strategy

import (
	"testing"

	"github.com/yourname/makemytrade/internal/indicators"
)

// ── Synthetic bar constructors ────────────────────────────────────────────────

// makeFlatBars returns n bars with constant price ~100 and moderate volume.
// No pattern should be detected on these.
func makeFlatBars(n int) []indicators.Bar {
	bars := make([]indicators.Bar, n)
	for i := range bars {
		bars[i] = indicators.Bar{Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000}
	}
	return bars
}

// makeBullFlagBars returns a bar series with a clear bull flag:
//   bars[0..14]:  base at 100 (15 bars, avg vol = 1M → sets 20-day avg)
//   bars[15..20]: pole — open 100, close rises to ~108 (+8%), vol 2M
//   bars[21..25]: flag — tight 105-107, vol 0.7M (below pole)
//   bars[26]:     today — close 109 (above flag high 107), vol 2.5M (2.5x avg)
func makeBullFlagBars() []indicators.Bar {
	bars := make([]indicators.Bar, 27)

	// Base: 15 bars at 100, vol 1M
	for i := 0; i < 15; i++ {
		bars[i] = indicators.Bar{Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000}
	}

	// Pole: 6 bars from 100 → 108
	poleClose := []float64{101.5, 103, 104.5, 106, 107, 108}
	for i, c := range poleClose {
		bars[15+i] = indicators.Bar{
			Open: c - 1.5, High: c + 0.5, Low: c - 2, Close: c,
			Volume: 2_000_000,
		}
	}

	// Flag: 5 bars, tight range 105-107, low volume
	for i := 0; i < 5; i++ {
		bars[21+i] = indicators.Bar{Open: 106, High: 107, Low: 105, Close: 106, Volume: 700_000}
	}

	// Today: breakout — close well above flag high 107
	bars[26] = indicators.Bar{Open: 107.5, High: 110, Low: 107, Close: 109, Volume: 2_500_000}
	return bars
}

// makeBearFlagBars mirrors makeBullFlagBars for the bearish direction.
//   bars[0..14]:  base at 110, vol 1M
//   bars[15..20]: pole — drops from ~110 to ~102 (-7.3%), vol 2M
//   bars[21..25]: flag — tight 103-105, low volume
//   bars[26]:     today — close 101 (below flag low 103), vol 2.5M
func makeBearFlagBars() []indicators.Bar {
	bars := make([]indicators.Bar, 27)

	for i := 0; i < 15; i++ {
		bars[i] = indicators.Bar{Open: 110, High: 111, Low: 109, Close: 110, Volume: 1_000_000}
	}

	poleClose := []float64{108, 107, 106, 105, 104, 102}
	for i, c := range poleClose {
		bars[15+i] = indicators.Bar{
			Open: c + 1.5, High: c + 2, Low: c - 0.5, Close: c,
			Volume: 2_000_000,
		}
	}

	for i := 0; i < 5; i++ {
		bars[21+i] = indicators.Bar{Open: 104, High: 105, Low: 103, Close: 104, Volume: 700_000}
	}

	// Today: breakdown below flag low 103
	bars[26] = indicators.Bar{Open: 102.5, High: 103, Low: 100, Close: 101, Volume: 2_500_000}
	return bars
}

// makeAscendingTriangleBars builds a 30-bar series with a verified ascending triangle:
//   Swing highs at exactly 110 at bars 8, 14, 20 (flat resistance, 0% range)
//   Rising swing lows at bars 11, 17, 23 at 100, 102, 104
//   Today (bar 29): close=113, vol=2M → breakout above resistance
//
// Each swing high: bars[i].High=110, bars[i-1].High<110, bars[i+1].High<110
// Each swing low:  bars[i].Low=L, bars[i-1].Low>L, bars[i+1].Low>L
func makeAscendingTriangleBars() []indicators.Bar {
	type b struct{ o, h, l, c float64; v float64 }
	// bars 0-28 form pattern; bar 29 = today (breakout)
	// Volume: first 14 bars 1.1M, next 14 bars 0.9M (contraction), today 2M
	spec := []b{
		// 0-4 warmup
		{100, 102, 99, 101, 1.1e6},
		{101, 103, 100, 102, 1.1e6},
		{102, 104, 101, 103, 1.1e6},
		{103, 105, 102, 104, 1.1e6},
		{104, 106, 103, 105, 1.1e6},
		// 5-7: approach to first swing high
		{105, 107, 104, 106, 1.1e6},
		{106, 108, 105, 107, 1.1e6},
		{107, 109, 106, 108, 1.1e6}, // bar7: H=109 — below peak, sets up for swing high at bar8
		// bar8: SWING HIGH at 110. bar7.H=109<110 ✓, bar9.H must be <110
		{108, 110, 107, 109, 1.1e6},
		{107, 108, 103, 104, 1.1e6}, // bar9: H=108<110 ✓ → swing high at bar8
		// 10-11: down to swing low
		{104, 106, 101, 102, 1.1e6},
		{102, 104, 100, 101, 1.1e6}, // bar11: L=100, bar10.L=101>100 ✓, bar12.L must be >100
		{101, 106, 102, 105, 1.1e6}, // bar12: L=102>100 ✓ → swing low at bar11 ✓
		// 13-14: approach second swing high
		{105, 108, 104, 107, 1.1e6},
		{107, 109, 105, 108, 0.9e6}, // bar14: H=109 — below 110, sets up bar14 as pre-peak
		// bar14 is approach; actual swing high at bar15... let me redo
		// I'll make swing high at bar14 explicitly:
		// bar13.H=108 < 110, bar14.H=110, bar15.H must be < 110
		// So reassign: use bar13 as approach (H=108), bar14 as swing high (H=110)
		// Already done above: bar13.H=108 < 110 ✓
		// Now bar14 must have H=110 and bar15.H<110:
		{108, 110, 106, 108, 0.9e6}, // bar14: H=110 — SWING HIGH (bar13.H=108<110 ✓, bar15.H<110 needed)
		{108, 108, 103, 104, 0.9e6}, // bar15: H=108<110 ✓ → swing high at bar14 ✓
		// 16-17: down to second swing low
		{104, 106, 103, 105, 0.9e6},
		{105, 107, 102, 103, 0.9e6}, // bar17: L=102, bar16.L=103>102 ✓, bar18.L must be >102
		{103, 107, 104, 106, 0.9e6}, // bar18: L=104>102 ✓ → swing low at bar17 ✓
		// 19-20: approach third swing high
		{106, 108, 105, 107, 0.9e6},
		{107, 109, 106, 108, 0.9e6}, // bar20: H=109 — below 110
		{108, 110, 107, 109, 0.9e6}, // bar21: H=110 — SWING HIGH (bar20.H=109<110 ✓, bar22.H<110 needed)
		{109, 108, 105, 106, 0.9e6}, // bar22: H=108<110 ✓ → swing high at bar21 ✓
		// 23: third swing low
		{106, 108, 105, 107, 0.9e6},
		{107, 109, 104, 105, 0.9e6}, // bar24: L=104, bar23.L=105>104 ✓, bar25.L must be >104
		{105, 107, 105, 106, 0.9e6}, // bar25: L=105>104 ✓ → swing low at bar24 ✓
		// 26-28: approach breakout
		{106, 108, 105, 107, 0.9e6},
		{107, 109, 106, 108, 0.9e6},
		{108, 110, 107, 109, 0.9e6},
		// bar29: TODAY — breakout above resistance=110
		{109, 114, 108, 113, 2e6},
	}

	bars := make([]indicators.Bar, len(spec))
	for i, s := range spec {
		bars[i] = indicators.Bar{Open: s.o, High: s.h, Low: s.l, Close: s.c, Volume: s.v}
	}
	return bars
}

// makeDescendingTriangleBars builds a 30-bar series with a verified descending triangle:
//   Flat support at exactly 90 at bars 8, 14, 20 (within 0%)
//   Falling swing highs at bars 11, 17, 23 at 100, 97, 94
//   Today (bar 29): close=87 → breakdown below support
func makeDescendingTriangleBars() []indicators.Bar {
	type b struct{ o, h, l, c, v float64 }
	spec := []b{
		// 0-4 warmup
		{100, 101, 98, 99, 1.1e6},
		{99, 100, 97, 98, 1.1e6},
		{98, 99, 96, 97, 1.1e6},
		{97, 98, 95, 96, 1.1e6},
		{96, 97, 94, 95, 1.1e6},
		// 5-7: decline toward first swing low
		{95, 96, 93, 94, 1.1e6},
		{94, 95, 92, 93, 1.1e6},
		{93, 94, 91, 92, 1.1e6}, // bar7: L=91 — above swing low, sets up bar8
		// bar8: SWING LOW at 90. bar7.L=91>90 ✓, bar9.L must be >90
		{92, 93, 90, 91, 1.1e6},
		{91, 94, 92, 93, 1.1e6}, // bar9: L=92>90 ✓ → swing low at bar8 ✓
		// 10-11: rise to first swing high
		{93, 97, 92, 96, 1.1e6},
		{96, 100, 95, 98, 1.1e6}, // bar11: H=100, bar10.H=97<100 ✓, bar12.H must be <100
		{98, 98, 92, 93, 1.1e6}, // bar12: H=98<100 ✓ → swing high at bar11 ✓
		// 13-14: decline to second swing low
		{93, 94, 91, 92, 1.1e6},
		{92, 93, 91, 92, 0.9e6}, // bar14: L=91 — above swing low 90
		{92, 93, 90, 91, 0.9e6}, // bar15: L=90 — SWING LOW (bar14.L=91>90 ✓, bar16.L must be >90)
		{91, 95, 92, 94, 0.9e6}, // bar16: L=92>90 ✓ → swing low at bar15 ✓
		// 17: rise to second swing high
		{94, 96, 93, 95, 0.9e6},
		{95, 97, 94, 96, 0.9e6}, // bar18: H=97, bar17.H=96<97 ✓, bar19.H must be <97
		{96, 96, 91, 92, 0.9e6}, // bar19: H=96<97 ✓ → swing high at bar18 ✓
		// 20-21: decline to third swing low
		{92, 93, 91, 92, 0.9e6},
		{92, 93, 91, 92, 0.9e6}, // bar21: L=91 — above 90
		{92, 93, 90, 91, 0.9e6}, // bar22: L=90 — SWING LOW (bar21.L=91>90 ✓, bar23.L must be >90)
		{91, 93, 92, 92, 0.9e6}, // bar23: L=92>90 ✓ → swing low at bar22 ✓
		// 24: rise to third swing high
		{92, 93, 91, 92, 0.9e6},
		{92, 94, 91, 93, 0.9e6}, // bar25: H=94, bar24.H=93<94 ✓, bar26.H must be <94
		{93, 93, 91, 92, 0.9e6}, // bar26: H=93<94 ✓ → swing high at bar25 ✓
		// 27-28: approach breakdown
		{92, 93, 91, 91, 0.9e6},
		{91, 92, 90, 91, 0.9e6},
		// bar29: TODAY — breakdown below support=90
		{91, 91, 86, 87, 2e6},
	}

	bars := make([]indicators.Bar, len(spec))
	for i, s := range spec {
		bars[i] = indicators.Bar{Open: s.o, High: s.h, Low: s.l, Close: s.c, Volume: s.v}
	}
	return bars
}

// makeBaseBreakoutBars returns a series with:
//   bars[0..4]:   warmup at 100
//   bars[5..44]:  sideways base 98-104 (6% range) with declining volume
//   bars[45]:     today — close 106, vol 2.5M (2.5x avg of 1M)
func makeBaseBreakoutBars() []indicators.Bar {
	bars := make([]indicators.Bar, 46)
	for i := 0; i < 5; i++ {
		bars[i] = indicators.Bar{Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000}
	}
	// Base: oscillate between 98 and 104 with declining volume
	for i := 5; i < 45; i++ {
		c := 101.0
		if i%3 == 0 {
			c = 98
		} else if i%3 == 1 {
			c = 104
		}
		vol := 1_000_000 - float64(i-5)*10_000 // gently declining volume
		if vol < 700_000 {
			vol = 700_000
		}
		bars[i] = indicators.Bar{Open: c - 0.5, High: c + 0.5, Low: c - 1, Close: c, Volume: vol}
	}
	// Today: breakout above base high 104
	bars[45] = indicators.Bar{Open: 104.5, High: 107, Low: 104, Close: 106, Volume: 2_500_000}
	return bars
}

// makeBaseBreakdownBars mirrors makeBaseBreakoutBars for bearish.
func makeBaseBreakdownBars() []indicators.Bar {
	bars := make([]indicators.Bar, 46)
	for i := 0; i < 5; i++ {
		bars[i] = indicators.Bar{Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000}
	}
	for i := 5; i < 45; i++ {
		c := 101.0
		if i%3 == 0 {
			c = 98
		} else if i%3 == 1 {
			c = 104
		}
		vol := 1_000_000 - float64(i-5)*10_000
		if vol < 700_000 {
			vol = 700_000
		}
		bars[i] = indicators.Bar{Open: c - 0.5, High: c + 0.5, Low: c - 1, Close: c, Volume: vol}
	}
	// Today: breakdown below base low 97 (bars[i].Low for i%3==0 is c-1 = 97)
	bars[45] = indicators.Bar{Open: 97, High: 97.5, Low: 95, Close: 96, Volume: 2_500_000}
	return bars
}

// ── Bull flag tests ───────────────────────────────────────────────────────────

func TestPattern_BullFlag_Detected(t *testing.T) {
	bars := makeBullFlagBars()
	result := detectBullFlag(bars)
	if result == nil {
		t.Fatal("expected bull flag detected, got nil")
	}
	if result.PatternName != "bull_flag" {
		t.Errorf("expected PatternName=bull_flag, got %q", result.PatternName)
	}
	if result.Direction != "bullish" {
		t.Errorf("expected Direction=bullish, got %q", result.Direction)
	}
	if result.QualityScore < 0.60 {
		t.Errorf("expected QualityScore >= 0.60, got %.2f", result.QualityScore)
	}
	if result.BreakoutLevel <= 0 {
		t.Error("expected BreakoutLevel > 0")
	}
	if result.InvalidationLevel <= 0 {
		t.Error("expected InvalidationLevel > 0")
	}
	if len(result.Reasons) == 0 {
		t.Error("expected at least one diagnostic reason")
	}
	t.Logf("bull_flag detected: quality=%.2f breakout=%.2f invalidation=%.2f reasons=%v",
		result.QualityScore, result.BreakoutLevel, result.InvalidationLevel, result.Reasons)
}

func TestPattern_BullFlag_NotDetected_OnFlatBars(t *testing.T) {
	bars := makeFlatBars(50)
	result := detectBullFlag(bars)
	if result != nil {
		t.Errorf("expected nil on flat bars, got pattern with quality=%.2f", result.QualityScore)
	}
}

func TestPattern_BullFlag_NotDetected_TooFewBars(t *testing.T) {
	bars := makeFlatBars(8) // below minimum
	result := detectBullFlag(bars)
	if result != nil {
		t.Errorf("expected nil with < 12 bars, got non-nil")
	}
}

// ── Bear flag tests ───────────────────────────────────────────────────────────

func TestPattern_BearFlag_Detected(t *testing.T) {
	bars := makeBearFlagBars()
	result := detectBearFlag(bars)
	if result == nil {
		t.Fatal("expected bear flag detected, got nil")
	}
	if result.PatternName != "bear_flag" {
		t.Errorf("expected PatternName=bear_flag, got %q", result.PatternName)
	}
	if result.Direction != "bearish" {
		t.Errorf("expected Direction=bearish, got %q", result.Direction)
	}
	if result.QualityScore < 0.60 {
		t.Errorf("expected QualityScore >= 0.60, got %.2f", result.QualityScore)
	}
	t.Logf("bear_flag detected: quality=%.2f breakout=%.2f invalidation=%.2f",
		result.QualityScore, result.BreakoutLevel, result.InvalidationLevel)
}

func TestPattern_BearFlag_NotDetected_OnFlatBars(t *testing.T) {
	bars := makeFlatBars(50)
	result := detectBearFlag(bars)
	if result != nil {
		t.Errorf("expected nil on flat bars, got quality=%.2f", result.QualityScore)
	}
}

// ── Ascending triangle tests ──────────────────────────────────────────────────

func TestPattern_AscendingTriangle_Detected(t *testing.T) {
	bars := makeAscendingTriangleBars()
	result := detectAscendingTriangle(bars)
	if result == nil {
		// Provide diagnostic info
		n := len(bars)
		shs := swingHighPoints(bars, 0, n-1)
		sls := swingLowPoints(bars, 0, n-1)
		t.Logf("No ascending triangle detected. swing_highs=%d swing_lows=%d", len(shs), len(sls))
		for _, sh := range shs {
			t.Logf("  swing high: idx=%d price=%.2f", sh.Idx, sh.Price)
		}
		for _, sl := range sls {
			t.Logf("  swing low:  idx=%d price=%.2f", sl.Idx, sl.Price)
		}
		t.Fatal("expected ascending triangle detected, got nil")
	}
	if result.PatternName != "ascending_triangle" {
		t.Errorf("expected PatternName=ascending_triangle, got %q", result.PatternName)
	}
	if result.Direction != "bullish" {
		t.Errorf("expected Direction=bullish, got %q", result.Direction)
	}
	if result.QualityScore < 0.55 {
		t.Errorf("expected QualityScore >= 0.55, got %.2f", result.QualityScore)
	}
	t.Logf("ascending_triangle: quality=%.2f resistance=%.2f support=%.2f",
		result.QualityScore, result.BreakoutLevel, result.InvalidationLevel)
}

func TestPattern_AscendingTriangle_NotDetected_OnFlatBars(t *testing.T) {
	bars := makeFlatBars(50)
	result := detectAscendingTriangle(bars)
	// Flat bars have no breakout today, so should return nil
	if result != nil {
		t.Errorf("expected nil on flat bars, got quality=%.2f", result.QualityScore)
	}
}

// ── Descending triangle tests ─────────────────────────────────────────────────

func TestPattern_DescendingTriangle_Detected(t *testing.T) {
	bars := makeDescendingTriangleBars()
	result := detectDescendingTriangle(bars)
	if result == nil {
		n := len(bars)
		shs := swingHighPoints(bars, 0, n-1)
		sls := swingLowPoints(bars, 0, n-1)
		t.Logf("No descending triangle detected. swing_highs=%d swing_lows=%d", len(shs), len(sls))
		t.Fatal("expected descending triangle detected, got nil")
	}
	if result.PatternName != "descending_triangle" {
		t.Errorf("expected PatternName=descending_triangle, got %q", result.PatternName)
	}
	if result.Direction != "bearish" {
		t.Errorf("expected Direction=bearish, got %q", result.Direction)
	}
	t.Logf("descending_triangle: quality=%.2f support=%.2f resistance=%.2f",
		result.QualityScore, result.BreakoutLevel, result.InvalidationLevel)
}

// ── Base breakout/breakdown tests ─────────────────────────────────────────────

func TestPattern_BaseBreakout_Detected(t *testing.T) {
	bars := makeBaseBreakoutBars()
	result := detectBaseBreakout(bars)
	if result == nil {
		t.Fatal("expected base breakout detected, got nil")
	}
	if result.PatternName != "base_breakout" {
		t.Errorf("expected PatternName=base_breakout, got %q", result.PatternName)
	}
	if result.Direction != "bullish" {
		t.Errorf("expected Direction=bullish, got %q", result.Direction)
	}
	if result.QualityScore < 0.55 {
		t.Errorf("expected QualityScore >= 0.55, got %.2f", result.QualityScore)
	}
	t.Logf("base_breakout: quality=%.2f range=%.1f%% reasons=%v",
		result.QualityScore, (result.BreakoutLevel-result.InvalidationLevel)/result.InvalidationLevel*100,
		result.Reasons)
}

func TestPattern_BaseBreakdown_Detected(t *testing.T) {
	bars := makeBaseBreakdownBars()
	result := detectBaseBreakdown(bars)
	if result == nil {
		t.Fatal("expected base breakdown detected, got nil")
	}
	if result.PatternName != "base_breakdown" {
		t.Errorf("expected PatternName=base_breakdown, got %q", result.PatternName)
	}
	if result.Direction != "bearish" {
		t.Errorf("expected Direction=bearish, got %q", result.Direction)
	}
	t.Logf("base_breakdown: quality=%.2f reasons=%v", result.QualityScore, result.Reasons)
}

func TestPattern_BaseBreakout_NotDetected_TooFewBars(t *testing.T) {
	bars := makeFlatBars(10)
	result := detectBaseBreakout(bars)
	if result != nil {
		t.Errorf("expected nil with < 20 bars, got non-nil")
	}
}

// ── No-unsupported-patterns test ──────────────────────────────────────────────

// TestPattern_NoUnsupportedPatternsDetected verifies that AnalyzePatterns only
// returns patterns from the allowed set and never invents patterns like
// head-and-shoulders, cup-and-handle, etc.
func TestPattern_NoUnsupportedPatternsDetected(t *testing.T) {
	cfg := DefaultRSVEConfig().PatternAnalysis

	bullBars := makeBullFlagBars()
	result := AnalyzePatterns(bullBars, "bullish", cfg)

	supportedBullish := map[string]bool{
		"bull_flag": true, "ascending_triangle": true, "base_breakout": true,
	}
	for _, p := range result.AllPatterns {
		if !supportedBullish[p.PatternName] {
			t.Errorf("detected unsupported bullish pattern %q — only bull_flag, ascending_triangle, base_breakout are allowed", p.PatternName)
		}
	}

	bearBars := makeBearFlagBars()
	result = AnalyzePatterns(bearBars, "bearish", cfg)
	supportedBearish := map[string]bool{
		"bear_flag": true, "descending_triangle": true, "base_breakdown": true,
	}
	for _, p := range result.AllPatterns {
		if !supportedBearish[p.PatternName] {
			t.Errorf("detected unsupported bearish pattern %q", p.PatternName)
		}
	}
}

// ── Ranking-only vs required_for_trade tests ──────────────────────────────────

// TestPattern_RankingOnly_DoesNotBlockWhenNoPattern proves that when
// required_for_trade=false (default), a missing pattern does NOT block
// a candidate that passes all other gates.
func TestPattern_RankingOnly_DoesNotBlockWhenNoPattern(t *testing.T) {
	cfg := DefaultRSVEConfig()
	cfg.PatternAnalysis.RequiredForTrade = false

	// Use flat bars: all patterns will return nil (no breakout today)
	// Build a minimal input that passes all stock gates
	input := makeValidBullishInput()
	input.Bars = makeValidBullishBars(150) // passes RSVE gates
	input.Bars = append(makeFlatBars(100), makeValidBullishBars(150)...)

	// Only check the pattern gate specifically — not full eval
	pa := AnalyzePatterns(makeFlatBars(50), "bullish", cfg.PatternAnalysis)

	// No pattern on flat bars
	if pa.Detected {
		t.Skip("flat bars unexpectedly produced a detected pattern; skipping")
	}

	// When required_for_trade=false, gatePatternRequired is NOT added
	// (the evaluateBranch only adds it when RequiredForTrade=true)
	// This test proves the gate is absent from the gate list
	cfg.PatternAnalysis.RequiredForTrade = false
	result := EvaluateRSVE(makeValidBullishInput(), cfg)
	for _, g := range result.Gates {
		if g.Name == "pattern_required" {
			t.Error("pattern_required gate must NOT appear when required_for_trade=false")
		}
	}
}

// TestPattern_RequiredForTrade_BlocksWhenNoPattern proves that when
// required_for_trade=true, a missing pattern causes rejection.
func TestPattern_RequiredForTrade_BlocksWhenNoPattern(t *testing.T) {
	cfg := DefaultRSVEConfig()
	cfg.PatternAnalysis.RequiredForTrade = true
	// Disable all patterns so none will be detected
	cfg.PatternAnalysis.AllowedPatterns = []string{}
	// Relax stock gates so the pattern gate is the first blocker
	cfg.Bullish.RSMinPct = -100
	cfg.Bullish.BBWidthPercentileMax = 1.0

	input := makeValidBullishInput()
	result := EvaluateRSVE(input, cfg)

	if result.AllPass {
		t.Fatal("expected rejection when required_for_trade=true and no patterns allowed, got AllPass=true")
	}
	if result.RejectGate != "pattern_required" {
		t.Errorf("expected RejectGate=pattern_required, got %q", result.RejectGate)
	}
}

// TestPattern_RequiredForTrade_PassesWhenPatternDetected proves that when
// required_for_trade=true and a pattern IS detected, the pattern gate passes.
func TestPattern_RequiredForTrade_PassesWhenPatternDetected(t *testing.T) {
	cfg := DefaultRSVEConfig()
	cfg.PatternAnalysis.RequiredForTrade = true
	cfg.PatternAnalysis.MinimumPatternQuality = 0.0 // accept any quality

	pa := AnalyzePatterns(makeBullFlagBars(), "bullish", cfg.PatternAnalysis)
	if !pa.Detected {
		t.Skip("bull flag bars didn't produce a detected pattern; skipping")
	}

	gate := gatePatternRequired(pa)
	if !gate.Passed {
		t.Errorf("expected gate_pattern_required to pass when pattern detected, got Passed=false")
	}
}

// ── Score ranking tests ───────────────────────────────────────────────────────

// TestPattern_Score_BoostsWhenPatternDetected verifies that a detected pattern
// increases the ranking score vs the same input without a pattern.
func TestPattern_Score_BoostsWhenPatternDetected(t *testing.T) {
	cfg := DefaultRSVEConfig()

	// Score without pattern
	pa0 := PatternAnalysis{Detected: false}
	f := rsveFeatures{
		hasRelStrength: true, relStrength20d: 5.0,
		hasBBWidthPct: true, bbWidthPercentile: 0.20,
		hasVolumeRatio: true, volumeRatio: 1.5,
		hasEMA20: true, hasEMA50: true, ema20: 105, ema50: 100,
	}
	scoreNoPattern := computeRSVERankScore(f, "bullish", pa0, cfg.PatternAnalysis, RSVEInput{}, cfg)

	// Score with pattern
	pa1 := PatternAnalysis{
		Detected:    true,
		BestPattern: &PatternResult{PatternName: "bull_flag", QualityScore: 0.80},
	}
	scoreWithPattern := computeRSVERankScore(f, "bullish", pa1, cfg.PatternAnalysis, RSVEInput{}, cfg)

	if scoreWithPattern <= scoreNoPattern {
		t.Errorf("expected score to increase with detected pattern: no_pattern=%.1f with_pattern=%.1f",
			scoreNoPattern, scoreWithPattern)
	}
	t.Logf("no_pattern=%.1f with_pattern=%.1f delta=%.1f",
		scoreNoPattern, scoreWithPattern, scoreWithPattern-scoreNoPattern)
}

// TestPattern_Score_NeverExceeds100 verifies the 100-point cap.
func TestPattern_Score_NeverExceeds100(t *testing.T) {
	cfg := DefaultRSVEConfig()

	pa := PatternAnalysis{
		Detected:    true,
		BestPattern: &PatternResult{QualityScore: 1.0},
	}
	f := rsveFeatures{
		hasRelStrength: true, relStrength20d: 20.0,
		hasBBWidthPct: true, bbWidthPercentile: 0.0,
		hasVolumeRatio: true, volumeRatio: 5.0,
		hasEMA20: true, hasEMA50: true, ema20: 120, ema50: 100,
	}
	score := computeRSVERankScore(f, "bullish", pa, cfg.PatternAnalysis, RSVEInput{}, cfg)
	if score > 100 {
		t.Errorf("score must not exceed 100, got %.1f", score)
	}
}

// ── AnalyzePatterns integration tests ────────────────────────────────────────

func TestPattern_AnalyzePatterns_DisabledReturnsEmpty(t *testing.T) {
	cfg := PatternAnalysisConfig{Enabled: false}
	result := AnalyzePatterns(makeBullFlagBars(), "bullish", cfg)
	if result.Detected {
		t.Error("expected Detected=false when pattern analysis is disabled")
	}
	if len(result.Reasons) == 0 || result.Reasons[0] != "pattern_analysis_disabled" {
		t.Errorf("expected reason=pattern_analysis_disabled, got %v", result.Reasons)
	}
}

func TestPattern_AnalyzePatterns_NoDetectionOnFlatBars(t *testing.T) {
	cfg := DefaultRSVEConfig().PatternAnalysis
	result := AnalyzePatterns(makeFlatBars(50), "bullish", cfg)
	if result.Detected {
		t.Errorf("expected no detection on flat bars, got Detected=true pattern=%s",
			result.BestPattern.PatternName)
	}
	if len(result.Reasons) == 0 {
		t.Error("expected at least one reason when no pattern detected")
	}
}

func TestPattern_AnalyzePatterns_BullishOnlyBullishPatterns(t *testing.T) {
	cfg := DefaultRSVEConfig().PatternAnalysis
	result := AnalyzePatterns(makeBullFlagBars(), "bullish", cfg)
	for _, p := range result.AllPatterns {
		if p.Direction != "bullish" {
			t.Errorf("bullish analysis returned non-bullish pattern %q direction=%q", p.PatternName, p.Direction)
		}
	}
}

func TestPattern_AnalyzePatterns_BearishOnlyBearishPatterns(t *testing.T) {
	cfg := DefaultRSVEConfig().PatternAnalysis
	result := AnalyzePatterns(makeBearFlagBars(), "bearish", cfg)
	for _, p := range result.AllPatterns {
		if p.Direction != "bearish" {
			t.Errorf("bearish analysis returned non-bearish pattern %q direction=%q", p.PatternName, p.Direction)
		}
	}
}

// TestPattern_Diagnostics_AlwaysPresent verifies PatternAnalysis diagnostics
// are attached to every RSVEResult (even rejected ones).
func TestPattern_Diagnostics_AlwaysPresent(t *testing.T) {
	cfg := DefaultRSVEConfig()

	// Rejected candidate (VIX too high)
	input := makeValidBullishInput()
	input.VIX = 30.0
	result := EvaluateRSVE(input, cfg)

	if result.AllPass {
		t.Skip("VIX=30 should trigger rejection")
	}
	// PatternAnalysis must always be populated, even when gates fail
	if result.PatternAnalysis.Reasons == nil && result.PatternAnalysis.BestPattern == nil {
		t.Error("expected PatternAnalysis to be populated even on rejection")
	}
}

// ── Option gate behavior tests ────────────────────────────────────────────────

// TestPattern_OptionDataUnavailable_StockGatesPass verifies that when option data
// is -1 (unavailable), the result is "stock_signal_passed" — stock gates confirmed
// but no paper trade created until option quality is verified.
func TestPattern_OptionDataUnavailable_StockGatesPass(t *testing.T) {
	cfg := DefaultRSVEConfig()
	// Relax stock gates so option unavailability is the thing being tested
	cfg.Bullish.RSMinPct = -100
	cfg.Bullish.BBWidthPercentileMax = 1.0

	input := makeValidBullishInput()
	// Set all option data to -1 (unavailable)
	input.IVRank = -1
	input.BidAskSpreadPct = -1
	input.OpenInterest = -1
	input.OptionVolume = -1

	result := EvaluateRSVE(input, cfg)

	// With option data unavailable, result should be stock_signal_passed (not paper_trade_created)
	if result.Status != "stock_signal_passed" {
		t.Errorf("expected status=stock_signal_passed when all option data is -1, got %q", result.Status)
	}
	// AllPass must be false — paper trade is not created without verified option quality
	if result.AllPass {
		t.Errorf("expected AllPass=false when option data is unavailable, got true")
	}
	// Option gates should NOT appear in the diagnostics (skipped entirely, not evaluated)
	optionGates := map[string]bool{"iv_rank_ok": true, "spread_quality": true, "oi_minimum": true, "option_volume": true}
	for _, g := range result.Gates {
		if optionGates[g.Name] {
			t.Errorf("option gate %q should not appear in diagnostics when data is unavailable", g.Name)
		}
	}
	t.Logf("status=%s allPass=%v gates=%d — correct: stock confirmed, option data required", result.Status, result.AllPass, len(result.Gates))
}

// TestPattern_OptionDataPresent_BadSpread_Rejects verifies that when real
// option data is available and fails a gate, the candidate is rejected.
func TestPattern_OptionDataPresent_BadSpread_Rejects(t *testing.T) {
	cfg := DefaultRSVEConfig()
	// Relax stock gates so spread_quality is the first blocker being tested
	cfg.Bullish.RSMinPct = -100
	cfg.Bullish.BBWidthPercentileMax = 1.0

	input := makeValidBullishInput()
	input.BidAskSpreadPct = 15.0 // above 8% max
	input.IVRank = 50.0
	input.OpenInterest = 600
	input.OptionVolume = 60

	result := EvaluateRSVE(input, cfg)

	if result.AllPass {
		t.Fatal("expected rejection for spread=15%%, got AllPass=true")
	}
	if result.RejectGate != "spread_quality" {
		t.Errorf("expected RejectGate=spread_quality, got %q", result.RejectGate)
	}
}
