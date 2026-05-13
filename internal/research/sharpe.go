// internal/research/sharpe.go
//
// WHAT: Deflated Sharpe Ratio (DSR) calculation and false-discovery reporting.
//
// WHY:  When you test 180 threshold combinations (4×3×3×3×5), some will show
//       high Sharpe ratios purely by chance. The Deflated Sharpe Ratio adjusts
//       for this multiple-testing problem, giving a mathematically honest answer
//       to: "Is this strategy's Sharpe ratio genuine or a data-mining artifact?"
//
// HOW:  Bailey & Lopez de Prado (2014), "The Deflated Sharpe Ratio: Correcting
//       for Selection Bias, Backtest Overfitting, and Non-Normality."
//
//       DSR = (SR_max - E[SR_max | H0]) / sqrt(V[SR_max])
//
//       E[SR_max | H0] = expected maximum Sharpe under null hypothesis (no skill)
//                      for N independent trials.
//       V[SR_max]      = variance of the Sharpe estimator for T observations.
//
//       A false-discovery warning fires when DSR < 0 (observed max Sharpe is not
//       statistically better than what chance alone would produce).
//
// WHAT BREAKS: Small T (< 30 observations) makes V[SR] unstable.
//              Skewness and kurtosis matter for accurate DSR; passing zero gives
//              a simplified estimate that assumes normality.
//
// VERIFY: go test ./internal/research/ -run TestSharpe -v

package research

import (
	"math"
)

const eulerMascheroni = 0.5772156649

// SharpeReport is the output of DeflatedSharpeReport.
type SharpeReport struct {
	NTrials           int     // total threshold combinations tested
	InSampleSharpe    float64 // best observed Sharpe (in-sample)
	OOSSharpe         float64 // best Sharpe on out-of-sample data
	ExpectedMaxSharpe float64 // E[max SR | H0] — what chance alone would produce
	DeflatedSharpe    float64 // DSR = (SR_max - E[max SR | H0]) / sqrt(V[SR])
	FDRWarning        bool    // true when DSR < 0 (likely data-mining artifact)
	Confidence        float64 // Φ(DSR): probability the strategy has genuine skill
	VarianceSR        float64 // V[SR_max] — reliability of the SR estimate
}

// DeflatedSharpeReport computes the Deflated Sharpe Ratio and false-discovery flag.
//
// Parameters:
//
//	nTrials      — number of independent threshold combinations tested
//	maxSharpe    — highest Sharpe ratio observed across all in-sample tests
//	oosSharpe    — Sharpe ratio on the held-out out-of-sample period
//	T            — number of observations used to compute maxSharpe
//	skewness     — return distribution skewness (pass 0 for normality assumption)
//	kurtosis     — return distribution excess kurtosis (pass 0 for normality)
//
// The false-discovery warning fires when DSR < 0, meaning the best result found
// is not statistically better than the maximum Sharpe one would expect purely
// from testing N combinations of random strategies.
func DeflatedSharpeReport(nTrials int, maxSharpe, oosSharpe float64, T int, skewness, kurtosis float64) SharpeReport {
	r := SharpeReport{
		NTrials:        nTrials,
		InSampleSharpe: maxSharpe,
		OOSSharpe:      oosSharpe,
	}

	if nTrials <= 0 || T <= 1 {
		return r
	}

	// E[max SR | H0]: expected maximum Sharpe under null hypothesis for N trials.
	// Bailey & Lopez de Prado (2014), Eq. 8, simplified approximation:
	//   E[max SR_N] ≈ (1-γ) × Φ⁻¹(1-1/N) + γ × Φ⁻¹(1-1/(N·e))
	N := float64(nTrials)
	q1 := normalQuantile(1.0 - 1.0/N)
	q2 := normalQuantile(1.0 - 1.0/(N*math.E))
	expectedMax := (1-eulerMascheroni)*q1 + eulerMascheroni*q2
	r.ExpectedMaxSharpe = expectedMax

	// V[SR]: variance of the Sharpe estimator for T observations.
	// Eq. 7 from Bailey & Lopez de Prado (2014):
	//   V[SR] = (1/T) × (1 + 0.5·SR²·(δ₄-1) - δ₃·SR + 0.25·(δ₄-1)²/T)
	// where δ₃=skewness, δ₄=excess kurtosis+3.
	// Simplified with kurtosis=3 (normal) and skewness=0:
	//   V[SR] = (1/T) × (1 + 0.5·SR²)
	Tf := float64(T)
	kurt3 := kurtosis + 3 // excess kurtosis → raw kurtosis
	varianceSR := (1.0 / Tf) * (1.0 + 0.5*maxSharpe*maxSharpe*(kurt3-1) - skewness*maxSharpe + 0.25*(kurt3-1)*(kurt3-1)/Tf)
	if varianceSR <= 0 {
		varianceSR = (1.0 / Tf) * (1.0 + 0.5*maxSharpe*maxSharpe)
	}
	r.VarianceSR = varianceSR

	// DSR = (SR_max - E[max SR | H0]) / sqrt(V[SR_max])
	dsr := (maxSharpe - expectedMax) / math.Sqrt(varianceSR)
	r.DeflatedSharpe = dsr
	r.FDRWarning = dsr < 0

	// Confidence = Φ(DSR): probability that SR > E[max SR | H0] by more than observed.
	r.Confidence = normalCDF(dsr)
	return r
}

// ── Normal distribution helpers ───────────────────────────────────────────────

// normalCDF returns Φ(x) — the standard normal cumulative distribution function.
// Uses the Horner rational approximation (Abramowitz & Stegun 26.2.17); error < 7.5e-8.
func normalCDF(x float64) float64 {
	if x < -8 {
		return 0
	}
	if x > 8 {
		return 1
	}
	// Use math.Erfc for accuracy
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// normalQuantile returns Φ⁻¹(p) — the inverse normal CDF (percent-point function).
// Uses a rational approximation valid for p in (0, 1).
// Beasley & Springer (1977), Algorithm AS 111.
func normalQuantile(p float64) float64 {
	if p <= 0 {
		return -8
	}
	if p >= 1 {
		return 8
	}
	if p < 0.5 {
		return -rationalApprox(math.Sqrt(-2 * math.Log(p)))
	}
	return rationalApprox(math.Sqrt(-2 * math.Log(1-p)))
}

func rationalApprox(t float64) float64 {
	const (
		c0, c1, c2 = 2.515517, 0.802853, 0.010328
		d1, d2, d3 = 1.432788, 0.189269, 0.001308
	)
	num := c0 + t*(c1+t*c2)
	den := 1.0 + t*(d1+t*(d2+t*d3))
	return t - num/den
}
