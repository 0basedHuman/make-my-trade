// internal/research/walkforward.go
//
// WHAT: Walk-forward threshold validation for RSVE-O PRO.
//
// WHY:  Fixed thresholds (RS>2%, RVOL≥1.2, BB≤30%) are assumptions, not proof.
//       Walk-forward testing validates whether a threshold is stable across
//       time periods, which is a much stronger signal than in-sample fit.
//
// HOW:  Rolling windows: train T days → test M days → advance M days → repeat.
//       For each window, apply threshold filter to candidates, then measure
//       out-of-sample P&L metrics from actual closed trades.
//       The best threshold is NOT the highest return — it is the MOST STABLE
//       across windows (lowest std dev of per-window expectancy).
//
// WHAT BREAKS:
//   - Shuffling candidates before splitting → look-ahead bias. Never do this.
//   - Using test-period outcome to select train thresholds → leakage. Forbidden.
//   - Fewer than trainDays+testDays candidates → result is empty.
//
// VERIFY: go test ./internal/research/ -run TestWalkForward -v

package research

import (
	"math"
	"sort"
)

// ThresholdConfig is one combination of RSVE-O thresholds to test.
// Zero values mean "use strategy default" — they are not filtered.
type ThresholdConfig struct {
	RSMinPct        float64 // RS minimum % vs SPY: 2, 3, 5, 8
	RVOLMin         float64 // RVOL minimum: 1.0, 1.2, 1.5
	BBPercentileMax float64 // BB squeeze max percentile: 0.20, 0.30, 0.40
	BreakoutBars    int     // prior-N-bar breakout window: 10, 20, 30
	EMARegime       int     // trend EMA period: 20, 50, 100, 150, 200
}

// CandidateRecord is one historical candidate for walk-forward analysis.
// Candidates must be sorted ascending by Date before passing to RunWalkForward.
type CandidateRecord struct {
	Date        string  // "2006-01-02" — sort key
	Ticker      string
	Direction   string  // "bullish" | "bearish"
	RS20dPct    float64 // 20d relative strength vs SPY (%)
	RVOL        float64 // relative volume at scan time
	BBPct       float64 // BB width percentile (0–1)
	BreakoutPct float64 // close vs prior-N-bar high (%)
	EMAGap      float64 // (close - ema) / ema
	OutcomePnL  float64 // realized P&L % for this trade
	WasTaken    bool    // true if trade was actually entered historically
}

// WindowResult is the out-of-sample performance for one walk-forward window.
type WindowResult struct {
	TrainStart   string
	TrainEnd     string
	TestStart    string
	TestEnd      string
	TradeCount   int
	WinRate      float64 // fraction of winners (OutcomePnL > 0)
	Expectancy   float64 // mean OutcomePnL across trades in this window
	ProfitFactor float64 // sum(winners) / abs(sum(losers)); 0 if no losers
	MaxDrawdown  float64 // largest peak-to-trough drawdown in cumulative P&L
	SharpeApprox float64 // expectancy / stddev(pnl); 0 if stddev==0
}

// WalkForwardResult aggregates all OOS windows for one ThresholdConfig.
type WalkForwardResult struct {
	Config         ThresholdConfig
	Windows        []WindowResult
	TradeCount     int     // total trades across all OOS windows
	WinRate        float64 // window-average win rate
	Expectancy     float64 // window-average expectancy
	ProfitFactor   float64 // window-average profit factor
	MaxDrawdown    float64 // worst single-window max drawdown
	StabilityScore float64 // std dev of per-window Expectancy (lower = more stable)
}

// RunWalkForward applies threshold filtering and computes rolling OOS metrics.
//
// candidates must be sorted ascending by Date (oldest first).
// trainDays and testDays are in units of candidate records (not calendar days).
// Uses a step size equal to testDays (non-overlapping OOS windows).
//
// No data from the test period is ever visible to the train-period filter:
// the threshold filter is applied independently in each window, and test
// candidates are only scored after the train window closes.
func RunWalkForward(candidates []CandidateRecord, cfg ThresholdConfig, trainDays, testDays int) WalkForwardResult {
	if trainDays <= 0 {
		trainDays = 126 // ~6 months of trading days
	}
	if testDays <= 0 {
		testDays = 21 // ~1 month
	}

	result := WalkForwardResult{Config: cfg}
	n := len(candidates)
	if n < trainDays+testDays {
		return result
	}

	for start := 0; start+trainDays+testDays <= n; start += testDays {
		trainSlice := candidates[start : start+trainDays]
		testSlice := candidates[start+trainDays : start+trainDays+testDays]

		// Anti-leakage check: first test date must be strictly after last train date.
		if len(trainSlice) > 0 && len(testSlice) > 0 {
			lastTrain := trainSlice[len(trainSlice)-1].Date
			firstTest := testSlice[0].Date
			if firstTest <= lastTrain {
				continue // leakage detected — skip this window
			}
		}

		// Apply threshold filter to the TEST slice only.
		// (Train slice could be used to tune thresholds; here we test a fixed config.)
		filtered := applyThresholds(testSlice, cfg)

		wr := computeWindowResult(trainSlice, filtered)
		if len(trainSlice) > 0 && len(testSlice) > 0 {
			wr.TrainStart = trainSlice[0].Date
			wr.TrainEnd = trainSlice[len(trainSlice)-1].Date
			wr.TestStart = testSlice[0].Date
			wr.TestEnd = testSlice[len(testSlice)-1].Date
		}
		result.Windows = append(result.Windows, wr)
	}

	result = aggregateWindows(result)
	return result
}

// ThresholdGrid returns the canonical set of ThresholdConfigs to test.
// Each dimension is varied independently; total combinations = product of slice lengths.
func ThresholdGrid() []ThresholdConfig {
	rsList := []float64{2.0, 3.0, 5.0, 8.0}
	rvolList := []float64{1.0, 1.2, 1.5}
	bbList := []float64{0.20, 0.30, 0.40}
	bkList := []int{10, 20, 30}
	emaList := []int{20, 50, 100, 150, 200}

	var cfgs []ThresholdConfig
	for _, rs := range rsList {
		for _, rvol := range rvolList {
			for _, bb := range bbList {
				for _, bk := range bkList {
					for _, ema := range emaList {
						cfgs = append(cfgs, ThresholdConfig{
							RSMinPct:        rs,
							RVOLMin:         rvol,
							BBPercentileMax: bb,
							BreakoutBars:    bk,
							EMARegime:       ema,
						})
					}
				}
			}
		}
	}
	return cfgs
}

// SortByStability sorts results ascending by StabilityScore (most stable first).
// Among ties, higher Expectancy wins.
func SortByStability(results []WalkForwardResult) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].StabilityScore != results[j].StabilityScore {
			return results[i].StabilityScore < results[j].StabilityScore
		}
		return results[i].Expectancy > results[j].Expectancy
	})
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func applyThresholds(candidates []CandidateRecord, cfg ThresholdConfig) []CandidateRecord {
	out := make([]CandidateRecord, 0, len(candidates))
	for _, c := range candidates {
		if cfg.RSMinPct > 0 && c.RS20dPct < cfg.RSMinPct {
			continue
		}
		if cfg.RVOLMin > 0 && c.RVOL < cfg.RVOLMin {
			continue
		}
		if cfg.BBPercentileMax > 0 && c.BBPct > cfg.BBPercentileMax {
			continue
		}
		out = append(out, c)
	}
	return out
}

func computeWindowResult(trainSlice, testFiltered []CandidateRecord) WindowResult {
	_ = trainSlice // reserved for future threshold tuning within windows
	wr := WindowResult{TradeCount: len(testFiltered)}
	if len(testFiltered) == 0 {
		return wr
	}

	var wins, totalPnL, sumWin, sumLoss float64
	pnls := make([]float64, 0, len(testFiltered))
	for _, c := range testFiltered {
		pnls = append(pnls, c.OutcomePnL)
		totalPnL += c.OutcomePnL
		if c.OutcomePnL > 0 {
			wins++
			sumWin += c.OutcomePnL
		} else {
			sumLoss += math.Abs(c.OutcomePnL)
		}
	}

	n := float64(len(testFiltered))
	wr.Expectancy = totalPnL / n
	wr.WinRate = wins / n
	if sumLoss > 0 {
		wr.ProfitFactor = sumWin / sumLoss
	}
	wr.MaxDrawdown = computeMaxDrawdown(pnls)
	wr.SharpeApprox = computeSharpe(pnls)
	return wr
}

func computeMaxDrawdown(pnls []float64) float64 {
	peak, dd, cum := 0.0, 0.0, 0.0
	for _, p := range pnls {
		cum += p
		if cum > peak {
			peak = cum
		}
		drawdown := peak - cum
		if drawdown > dd {
			dd = drawdown
		}
	}
	return dd
}

func computeSharpe(pnls []float64) float64 {
	if len(pnls) < 2 {
		return 0
	}
	n := float64(len(pnls))
	var sum float64
	for _, p := range pnls {
		sum += p
	}
	mean := sum / n
	var variance float64
	for _, p := range pnls {
		d := p - mean
		variance += d * d
	}
	variance /= n - 1
	if variance <= 0 {
		return 0
	}
	return mean / math.Sqrt(variance)
}

func aggregateWindows(r WalkForwardResult) WalkForwardResult {
	if len(r.Windows) == 0 {
		return r
	}
	n := float64(len(r.Windows))
	var sumExp, sumWR, sumPF, sumTC float64
	worstDD := 0.0
	expectations := make([]float64, 0, len(r.Windows))

	for _, w := range r.Windows {
		sumExp += w.Expectancy
		sumWR += w.WinRate
		sumPF += w.ProfitFactor
		sumTC += float64(w.TradeCount)
		if w.MaxDrawdown > worstDD {
			worstDD = w.MaxDrawdown
		}
		expectations = append(expectations, w.Expectancy)
	}

	r.Expectancy = sumExp / n
	r.WinRate = sumWR / n
	r.ProfitFactor = sumPF / n
	r.TradeCount = int(sumTC)
	r.MaxDrawdown = worstDD
	r.StabilityScore = stddev(expectations)
	return r
}

func stddev(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	n := float64(len(vals))
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / n
	var variance float64
	for _, v := range vals {
		d := v - mean
		variance += d * d
	}
	return math.Sqrt(variance / (n - 1))
}
