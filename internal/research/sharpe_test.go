// internal/research/sharpe_test.go
// Run: go test ./internal/research/ -run TestSharpe -v

package research

import (
	"math"
	"testing"
)

func TestSharpe_FDRWarning_WhenDSRNegative(t *testing.T) {
	// nTrials=540 with a mediocre SR should yield a FDR warning.
	// E[max SR | H0] for 540 trials ≈ 3.0 (many trials → high expected max by chance).
	// Observed maxSharpe=0.5 << expectedMax → DSR < 0 → FDR warning.
	r := DeflatedSharpeReport(540, 0.5, 0.3, 50, 0, 0)
	if !r.FDRWarning {
		t.Fatalf("expected FDR warning with 540 trials and SR=0.5, DSR=%.3f", r.DeflatedSharpe)
	}
}

func TestSharpe_NoFDRWarning_StrongSR(t *testing.T) {
	// 1 trial, SR=2.0, T=200: expected max SR ≈ 0, DSR should be positive.
	r := DeflatedSharpeReport(1, 2.0, 1.5, 200, 0, 0)
	if r.FDRWarning {
		t.Fatalf("should not warn FDR with 1 trial and SR=2.0, DSR=%.3f", r.DeflatedSharpe)
	}
}

func TestSharpe_TrialCountCaptured(t *testing.T) {
	r := DeflatedSharpeReport(180, 1.5, 1.0, 100, 0, 0)
	if r.NTrials != 180 {
		t.Fatalf("want NTrials=180, got %d", r.NTrials)
	}
}

func TestSharpe_Confidence_BetweenZeroAndOne(t *testing.T) {
	r := DeflatedSharpeReport(50, 1.5, 1.0, 100, 0, 0)
	if r.Confidence < 0 || r.Confidence > 1 {
		t.Fatalf("confidence must be in [0,1], got %.4f", r.Confidence)
	}
}

func TestSharpe_ZeroTrials_ReturnsEmpty(t *testing.T) {
	r := DeflatedSharpeReport(0, 1.0, 0.5, 100, 0, 0)
	if r.DeflatedSharpe != 0 {
		t.Fatalf("zero trials: expected DSR=0, got %.4f", r.DeflatedSharpe)
	}
}

func TestSharpe_NormalCDF_MonotonicallyIncreasing(t *testing.T) {
	xs := []float64{-3, -2, -1, 0, 1, 2, 3}
	prev := normalCDF(xs[0])
	for _, x := range xs[1:] {
		cur := normalCDF(x)
		if cur <= prev {
			t.Fatalf("normalCDF not monotonically increasing: CDF(%.0f)=%.4f <= CDF(%.0f)=%.4f",
				x, cur, x-1, prev)
		}
		prev = cur
	}
}

func TestSharpe_NormalQuantile_RoundTrip(t *testing.T) {
	// Φ(Φ⁻¹(p)) ≈ p for p in (0,1)
	ps := []float64{0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 0.99}
	for _, p := range ps {
		q := normalQuantile(p)
		got := normalCDF(q)
		if math.Abs(got-p) > 0.001 {
			t.Fatalf("round-trip: normalCDF(normalQuantile(%.2f)) = %.4f, want ≈%.2f", p, got, p)
		}
	}
}

func TestSharpe_ExpectedMaxSharpe_IncreasesWithTrials(t *testing.T) {
	r1 := DeflatedSharpeReport(10, 2.0, 1.5, 100, 0, 0)
	r2 := DeflatedSharpeReport(100, 2.0, 1.5, 100, 0, 0)
	r3 := DeflatedSharpeReport(540, 2.0, 1.5, 100, 0, 0)
	if r1.ExpectedMaxSharpe >= r2.ExpectedMaxSharpe {
		t.Fatalf("more trials should increase E[max SR]: 10 trials=%.3f ≥ 100 trials=%.3f",
			r1.ExpectedMaxSharpe, r2.ExpectedMaxSharpe)
	}
	if r2.ExpectedMaxSharpe >= r3.ExpectedMaxSharpe {
		t.Fatalf("more trials should increase E[max SR]: 100 trials=%.3f ≥ 540 trials=%.3f",
			r2.ExpectedMaxSharpe, r3.ExpectedMaxSharpe)
	}
}
