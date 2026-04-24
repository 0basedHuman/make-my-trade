package indicators

import (
	"math"
	"testing"
)

// TestRealizedVolatility checks that realized vol is non-negative and roughly
// correct for a flat series (all same close → vol = 0) and a trending series.
func TestRealizedVolatility(t *testing.T) {
	// Flat series: no daily changes → zero vol
	flat := make([]float64, 25)
	for i := range flat {
		flat[i] = 100.0
	}
	rv, ok := RealizedVolatility(flat, 20)
	if !ok {
		t.Fatal("RealizedVolatility(flat): expected ok=true")
	}
	if rv != 0 {
		t.Errorf("RealizedVolatility(flat): expected 0, got %v", rv)
	}

	// Insufficient data
	_, ok = RealizedVolatility(flat[:5], 20)
	if ok {
		t.Error("RealizedVolatility: expected ok=false for insufficient data")
	}

	// Trending series: should return positive vol
	trending := make([]float64, 25)
	for i := range trending {
		trending[i] = 100.0 + float64(i)*0.5
	}
	rv, ok = RealizedVolatility(trending, 20)
	if !ok {
		t.Fatal("RealizedVolatility(trending): expected ok=true")
	}
	if rv <= 0 {
		t.Errorf("RealizedVolatility(trending): expected > 0, got %v", rv)
	}
}

// TestVolScaledMomentum ensures the function produces finite values for normal inputs.
func TestVolScaledMomentum(t *testing.T) {
	// Trending up series: should give positive vol-scaled momentum
	closes := make([]float64, 70)
	for i := range closes {
		closes[i] = 100.0 + float64(i)*0.3
	}
	vsm, ok := VolScaledMomentum(closes, 63)
	if !ok {
		t.Fatal("VolScaledMomentum: expected ok=true")
	}
	if math.IsNaN(vsm) || math.IsInf(vsm, 0) {
		t.Errorf("VolScaledMomentum: got non-finite value %v", vsm)
	}
	if vsm <= 0 {
		t.Errorf("VolScaledMomentum: expected > 0 for uptrend, got %v", vsm)
	}
}

// TestShannonEntropy checks boundary cases.
func TestShannonEntropy(t *testing.T) {
	// All-up series → entropy near 0 (only up moves)
	allUp := make([]float64, 35)
	for i := range allUp {
		allUp[i] = 100.0 + float64(i)
	}
	ent, ok := ShannonEntropy(allUp, 30)
	if !ok {
		t.Fatal("ShannonEntropy(allUp): expected ok=true")
	}
	if ent != 0 {
		t.Errorf("ShannonEntropy(allUp): expected 0, got %v", ent)
	}

	// Alternating up/down → entropy near 1.0
	alternating := make([]float64, 35)
	alternating[0] = 100.0
	for i := 1; i < len(alternating); i++ {
		if i%2 == 0 {
			alternating[i] = alternating[i-1] + 1
		} else {
			alternating[i] = alternating[i-1] - 1
		}
	}
	ent, ok = ShannonEntropy(alternating, 30)
	if !ok {
		t.Fatal("ShannonEntropy(alternating): expected ok=true")
	}
	if ent < 0.95 {
		t.Errorf("ShannonEntropy(alternating): expected ~1.0, got %v", ent)
	}

	// Insufficient data
	_, ok = ShannonEntropy(allUp[:5], 30)
	if ok {
		t.Error("ShannonEntropy: expected ok=false for insufficient data")
	}
}

// TestBollingerWidth checks that width is non-negative and zero for flat series.
func TestBollingerWidth(t *testing.T) {
	flat := make([]float64, 25)
	for i := range flat {
		flat[i] = 100.0
	}
	bw, ok := BollingerWidth(flat, 20, 2.0)
	if !ok {
		t.Fatal("BollingerWidth(flat): expected ok=true")
	}
	if bw != 0 {
		t.Errorf("BollingerWidth(flat): expected 0, got %v", bw)
	}

	// Volatile series: width > 0
	volatile := make([]float64, 25)
	for i := range volatile {
		if i%2 == 0 {
			volatile[i] = 95.0
		} else {
			volatile[i] = 105.0
		}
	}
	bw, ok = BollingerWidth(volatile, 20, 2.0)
	if !ok {
		t.Fatal("BollingerWidth(volatile): expected ok=true")
	}
	if bw <= 0 {
		t.Errorf("BollingerWidth(volatile): expected > 0, got %v", bw)
	}
}

// TestSqueezeRatio checks that it compiles and returns ok=false for short data.
func TestSqueezeRatio(t *testing.T) {
	// Short data: should return ok=false
	shortBars := make([]Bar, 5)
	for i := range shortBars {
		shortBars[i] = Bar{Open: 100, High: 101, Low: 99, Close: 100, Volume: 1000}
	}
	_, ok := SqueezeRatio(shortBars, 20)
	if ok {
		t.Error("SqueezeRatio: expected ok=false for short data")
	}

	// Sufficient flat data: should return a value
	bars := make([]Bar, 25)
	for i := range bars {
		bars[i] = Bar{Open: 100, High: 101 + float64(i%3), Low: 99, Close: 100, Volume: 1000}
	}
	sr, ok := SqueezeRatio(bars, 20)
	if !ok {
		t.Fatal("SqueezeRatio: expected ok=true for sufficient data")
	}
	if math.IsNaN(sr) || math.IsInf(sr, 0) {
		t.Errorf("SqueezeRatio: got non-finite value %v", sr)
	}
	if sr < 0 {
		t.Errorf("SqueezeRatio: expected >= 0, got %v", sr)
	}
}
