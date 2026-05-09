// internal/strategy/rsve_test.go
//
// Tests for the RSVE-O binary gate evaluator.
// Each test covers one gate in isolation by constructing minimal bar data
// that satisfies all other gates and only varies the gate under test.
//
// Run: go test ./internal/strategy/ -run TestRSVE -v

package strategy

import (
	"testing"

	"github.com/yourname/makemytrade/internal/indicators"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

// makeBullishBars builds a synthetic bar series that satisfies all bullish
// structural gates. The last bar's close is the entry point.
// Direction: steady uptrend, EMA20 > EMA50, RVOL high, in a squeeze.
func makeBullishBars(n int, entryClose float64) []indicators.Bar {
	bars := make([]indicators.Bar, n)
	price := entryClose * 0.80
	step := (entryClose - price) / float64(n)
	for i := range bars {
		p := price + float64(i)*step
		// Create tight bars during consolidation phase, then breakout on last bar
		bars[i] = indicators.Bar{
			Open:   p * 0.999,
			High:   p * 1.005,
			Low:    p * 0.995,
			Close:  p,
			Volume: 1_500_000,
		}
	}
	// Last bar: breakout — close well above prior 20 highs
	bars[n-1] = indicators.Bar{
		Open:   entryClose * 0.999,
		High:   entryClose * 1.010,
		Low:    entryClose * 0.990,
		Close:  entryClose,
		Volume: 2_500_000, // 1.67x of prior avg
	}
	return bars
}

// makeSPYBars builds synthetic SPY bars in a gentle uptrend.
// SPY close will be above its EMA50 (market_uptrend gate passes).
func makeSPYBars(n int) []indicators.Bar {
	bars := make([]indicators.Bar, n)
	price := 440.0
	for i := range bars {
		p := price + float64(i)*0.10 // slow uptrend
		bars[i] = indicators.Bar{
			Open:   p,
			High:   p * 1.003,
			Low:    p * 0.997,
			Close:  p,
			Volume: 80_000_000,
		}
	}
	return bars
}

// defaultTestConfig returns an RSVE config suitable for testing.
func defaultTestConfig() RSVEConfig {
	return DefaultRSVEConfig()
}

// makeValidBullishBars generates a 150-bar series designed to pass all bullish
// structural gates:
//   - Bars 0-89:   volatile swings (high BB width) → sets a "wide" baseline
//   - Bars 90-148: tight consolidation (low BB width) → creates squeeze signal
//   - Bar 149:     breakout candle above the prior 20-bar high with strong volume
//
// This ensures bbWidthPercentile < 0.30 (squeeze active) and the close breaks
// above the highest high of bars 129-148.
func makeValidBullishBars(n int) []indicators.Bar {
	bars := make([]indicators.Bar, n)
	// Phase 1: volatile — price oscillates widely so BB widths are large
	basePrice := 190.0
	for i := 0; i < 90; i++ {
		sign := 1.0
		if i%2 == 1 {
			sign = -1.0
		}
		p := basePrice + sign*float64(i%5)*0.8
		bars[i] = indicators.Bar{
			Open:   p * 0.990,
			High:   p * 1.020, // ±2% daily swings
			Low:    p * 0.980,
			Close:  p,
			Volume: 1_500_000,
		}
	}
	// Phase 2: tight consolidation — price drifts gently up, tiny candles
	for i := 90; i < n-1; i++ {
		frac := float64(i-90) / float64(n-1-90)
		p := 191.0 + frac*8.0 // gentle drift from 191 → 199
		bars[i] = indicators.Bar{
			Open:   p * 0.9998,
			High:   p * 1.003, // only ±0.3% — very tight
			Low:    p * 0.997,
			Close:  p,
			Volume: 1_500_000, // consistent volume (RVOL ~1.0 for now)
		}
	}
	// Phase 3: breakout bar with strong volume
	// Prior 20-bar high (bars 129-148) tops out around 198.87 * 1.003 ≈ 199.47
	priorHigh := 0.0
	for i := n - 21; i < n-1; i++ {
		if bars[i].High > priorHigh {
			priorHigh = bars[i].High
		}
	}
	breakoutClose := priorHigh * 1.006 // clearly above prior highs
	bars[n-1] = indicators.Bar{
		Open:   breakoutClose * 0.999,
		High:   breakoutClose * 1.008,
		Low:    breakoutClose * 0.993,
		Close:  breakoutClose,
		Volume: 3_000_000, // 2× avg → strong RVOL
	}
	return bars
}

// makeValidBullishInput builds a standard bullish RSVEInput where all gates pass.
// Individual tests override specific fields to test gate isolation.
func makeValidBullishInput() RSVEInput {
	n := 150
	spyBars := makeSPYBars(n)
	stockBars := makeValidBullishBars(n)

	return RSVEInput{
		Ticker:           "TEST",
		Date:             "2026-05-01",
		Bars:             stockBars,
		SPYBars:          spyBars,
		VIX:              15.0,
		EarningsDaysAway: -1,   // unknown → pass
		IVRank:           50.0, // well below 70 threshold
		BidAskSpreadPct:  5.0,  // well below 8% threshold
		OpenInterest:     1000, // well above 500 minimum
		OptionVolume:     100,  // well above 50 minimum
	}
}

// ── Gate 1: VIX regime ────────────────────────────────────────────────────────

func TestRSVE_VIXGate_Blocks(t *testing.T) {
	input := makeValidBullishInput()
	input.VIX = 25.0 // above 24.0 threshold
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	if r.AllPass {
		t.Fatal("expected rejection on VIX >= 24, got AllPass=true")
	}
	if r.RejectGate != "vix_regime" {
		t.Errorf("expected RejectGate=vix_regime, got %q", r.RejectGate)
	}
}

func TestRSVE_VIXGate_Passes(t *testing.T) {
	input := makeValidBullishInput()
	input.VIX = 18.0 // well under 24.0
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	// vix gate must pass (AllPass depends on other gates, but vix must not be blocking)
	for _, g := range r.Gates {
		if g.Name == "vix_regime" && !g.Passed {
			t.Errorf("vix_regime gate should pass at VIX=18, got Passed=false")
		}
	}
}

// ── Gate 3: Earnings blackout ─────────────────────────────────────────────────

func TestRSVE_EarningsGate_Blocks(t *testing.T) {
	input := makeValidBullishInput()
	input.VIX = 18.0
	input.EarningsDaysAway = 3 // within 5-day blackout
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	if r.AllPass {
		t.Fatal("expected rejection when earnings in 3 days, got AllPass=true")
	}
	if r.RejectGate != "no_earnings" {
		t.Errorf("expected RejectGate=no_earnings, got %q", r.RejectGate)
	}
}

func TestRSVE_EarningsGate_UnknownPasses(t *testing.T) {
	input := makeValidBullishInput()
	input.EarningsDaysAway = -1 // unknown → should not block
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	for _, g := range r.Gates {
		if g.Name == "no_earnings" && !g.Passed {
			t.Errorf("no_earnings gate should pass when EarningsDaysAway=-1 (unknown)")
		}
	}
}

// ── Gate 4: Relative strength ─────────────────────────────────────────────────

func TestRSVE_RelativeStrength_WeakStockBlocks(t *testing.T) {
	// Stock that lags SPY → RS negative → bullish RS gate fails
	n := 150
	spyBars := makeSPYBars(n)
	// Make stock underperform: flat price while SPY climbs
	stockBars := make([]indicators.Bar, n)
	price := 200.0
	for i := range stockBars {
		stockBars[i] = indicators.Bar{
			Open:  price * 0.999,
			High:  price * 1.005,
			Low:   price * 0.995,
			Close: price, // flat — SPY is climbing so RS will be negative
			Volume: 2_000_000,
		}
	}

	input := RSVEInput{
		Ticker:           "WEAK",
		Date:             "2026-05-01",
		Bars:             stockBars,
		SPYBars:          spyBars,
		VIX:              15.0,
		EarningsDaysAway: -1,
		IVRank:           -1,
		BidAskSpreadPct:  -1,
		OpenInterest:     -1,
	}
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	if r.AllPass {
		t.Fatal("expected rejection for stock lagging SPY, got AllPass=true")
	}
	// The gate might fail on relative_strength or relative_weakness depending on which
	// branch passed further — just verify it was rejected
	t.Logf("rejected at gate: %s (direction=%s)", r.RejectGate, r.Direction)
}

// ── Gate 7: Volume expansion ──────────────────────────────────────────────────

func TestRSVE_VolumeGate_LowVolumeBlocks(t *testing.T) {
	// Low RVOL: all bars at same volume = RVOL ~1.0 (below 1.2 threshold)
	n := 150
	spyBars := makeSPYBars(n)
	stockBars := makeBullishBars(n, 200.0)
	// Set all volumes equal (including last bar) → RVOL = 1.0
	for i := range stockBars {
		stockBars[i].Volume = 1_000_000
	}

	input := RSVEInput{
		Ticker:           "LOWVOL",
		Date:             "2026-05-01",
		Bars:             stockBars,
		SPYBars:          spyBars,
		VIX:              15.0,
		EarningsDaysAway: -1,
		IVRank:           -1,
		BidAskSpreadPct:  -1,
		OpenInterest:     -1,
	}
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	if r.AllPass {
		t.Fatal("expected rejection for low volume, got AllPass=true")
	}
	for _, g := range r.Gates {
		if g.Name == "volume_expansion" && g.Passed {
			t.Errorf("volume_expansion gate should fail with RVOL ~1.0")
		}
	}
}

// ── Gate 11-13: Options gates — unavailable data passes silently ─────────────

func TestRSVE_OptionsGates_UnavailableDataPasses(t *testing.T) {
	input := makeValidBullishInput()
	input.IVRank = -1
	input.BidAskSpreadPct = -1
	input.OpenInterest = -1
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	for _, g := range r.Gates {
		switch g.Name {
		case "iv_rank_ok", "spread_quality", "oi_minimum":
			if !g.Passed {
				t.Errorf("gate %s should pass when data is unavailable (-1)", g.Name)
			}
		}
	}
}

func TestRSVE_IVRankGate_HighIVBlocks(t *testing.T) {
	input := makeValidBullishInput()
	input.VIX = 18.0
	input.IVRank = 80.0 // above 70 threshold
	cfg := defaultTestConfig()
	// Relax all non-options gates to isolate the IV rank gate
	cfg.Bullish.BBWidthPercentileMax = 1.0
	cfg.Bullish.RSMinPct = -100
	r := EvaluateRSVE(input, cfg)

	if r.AllPass {
		t.Fatal("expected rejection for high IV rank, got AllPass=true")
	}
	if r.RejectGate != "iv_rank_ok" {
		t.Errorf("expected RejectGate=iv_rank_ok, got %q", r.RejectGate)
	}
}

func TestRSVE_SpreadGate_WideSpreadBlocks(t *testing.T) {
	input := makeValidBullishInput()
	input.VIX = 18.0
	input.BidAskSpreadPct = 15.0 // above 10% threshold
	cfg := defaultTestConfig()
	// Relax all non-spread gates to isolate the spread gate
	cfg.Bullish.BBWidthPercentileMax = 1.0
	cfg.Bullish.RSMinPct = -100
	r := EvaluateRSVE(input, cfg)

	if r.AllPass {
		t.Fatal("expected rejection for wide spread, got AllPass=true")
	}
	if r.RejectGate != "spread_quality" {
		t.Errorf("expected RejectGate=spread_quality, got %q", r.RejectGate)
	}
}

// ── Score: not required for tradability ──────────────────────────────────────

func TestRSVE_Score_NotRequiredForConfirmed(t *testing.T) {
	// Score is purely for ranking — a low score must NOT prevent "confirmed".
	// Use permissive thresholds so the focus is on the AllPass/Status contract.
	input := makeValidBullishInput()
	input.VIX = 18.0
	cfg := defaultTestConfig()
	cfg.Bullish.RSMinPct = -100           // any RS passes (synthetic data is neutral)
	cfg.Bullish.BBWidthPercentileMax = 1.0 // skip squeeze requirement
	r := EvaluateRSVE(input, cfg)

	if !r.AllPass {
		t.Logf("gate diagnostics:")
		for _, g := range r.Gates {
			t.Logf("  %-24s  pass=%-5v  block=%-5v  actual=%s", g.Name, g.Passed, g.Blocking, g.ActualValue)
		}
		t.Fatalf("expected AllPass=true with permissive config, got false (reject=%s)", r.RejectGate)
	}
	if r.Status != "paper_trade_created" {
		t.Errorf("expected status=paper_trade_created, got %s", r.Status)
	}
	t.Logf("score=%.1f (ranking only — not a gate)", r.Score)
}

// ── Bearish branch ────────────────────────────────────────────────────────────

func TestRSVE_BearishBranch_VIXThreshold(t *testing.T) {
	// Bearish allows VIX up to 32 (not 24)
	input := makeValidBullishInput()
	input.VIX = 28.0 // above 24 (bullish limit) but below 32 (bearish limit)
	cfg := defaultTestConfig()
	r := EvaluateRSVE(input, cfg)

	// The bullish branch should fail (VIX>24) and the bearish branch may pass VIX gate.
	// r.Direction reveals which branch was chosen for the result.
	if r.Direction == "bullish" && r.AllPass {
		t.Error("bullish branch should not pass when VIX=28 (>24)")
	}
	// If bearish branch: VIX gate should pass (28 < 32)
	if r.Direction == "bearish" {
		for _, g := range r.Gates {
			if g.Name == "vix_regime" && !g.Passed {
				t.Error("bearish vix_regime gate should pass at VIX=28 (threshold=32)")
			}
		}
	}
}

// ── GateDiagnostic output completeness ───────────────────────────────────────

func TestRSVE_DiagnosticsComplete(t *testing.T) {
	// When all stock gates pass and option data is present, 12 gates are returned.
	input := makeValidBullishInput()
	cfg := defaultTestConfig()
	// Relax stock gates so all 12 gates are evaluated (not early-exited)
	cfg.Bullish.RSMinPct = -100
	cfg.Bullish.BBWidthPercentileMax = 1.0
	r := EvaluateRSVE(input, cfg)

	if len(r.Gates) != 12 {
		t.Errorf("expected 12 gate diagnostics, got %d", len(r.Gates))
		for i, g := range r.Gates {
			t.Logf("  gate[%d]: %s passed=%v blocking=%v", i, g.Name, g.Passed, g.Blocking)
		}
	}

	// Each gate must have a non-empty name and data source.
	for _, g := range r.Gates {
		if g.Name == "" {
			t.Error("gate has empty Name")
		}
		if g.DataSource == "" {
			t.Error("gate has empty DataSource")
		}
	}

	// At most one gate should be Blocking=true.
	blockingCount := 0
	for _, g := range r.Gates {
		if g.Blocking {
			blockingCount++
		}
	}
	if blockingCount > 1 {
		t.Errorf("expected at most 1 blocking gate, got %d", blockingCount)
	}
}

// ── Insufficient bars ─────────────────────────────────────────────────────────

func TestRSVE_InsufficientBars_DoesNotPanic(t *testing.T) {
	input := RSVEInput{
		Ticker:           "FEW",
		Date:             "2026-05-01",
		Bars:             makeBullishBars(10, 100.0), // way too few
		SPYBars:          makeSPYBars(10),
		VIX:              15.0,
		EarningsDaysAway: -1,
		IVRank:           -1,
		BidAskSpreadPct:  -1,
		OpenInterest:     -1,
	}
	cfg := defaultTestConfig()

	// Must not panic regardless of input quality
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("EvaluateRSVE panicked on short bars: %v", rec)
		}
	}()
	r := EvaluateRSVE(input, cfg)
	if r.AllPass {
		t.Error("short bar input should not pass all gates")
	}
}
