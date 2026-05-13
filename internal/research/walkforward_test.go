// internal/research/walkforward_test.go
// Run: go test ./internal/research/ -run TestWalkForward -v

package research

import (
	"fmt"
	"testing"
)

// makeCandidates builds N synthetic candidates with alternating dates.
// Outcomes cycle: +10, -5, +8, -3 (positive expectancy, ~60% win rate).
func makeCandidates(n int) []CandidateRecord {
	pnls := []float64{10.0, -5.0, 8.0, -3.0}
	candidates := make([]CandidateRecord, n)
	for i := 0; i < n; i++ {
		candidates[i] = CandidateRecord{
			Date:       fmt.Sprintf("2025-%02d-%02d", (i/28)+1, (i%28)+1),
			Ticker:     fmt.Sprintf("T%03d", i),
			Direction:  "bullish",
			RS20dPct:   5.0,
			RVOL:       1.3,
			BBPct:      0.25,
			OutcomePnL: pnls[i%4],
			WasTaken:   true,
		}
	}
	return candidates
}

func TestWalkForward_NoLeakage(t *testing.T) {
	candidates := makeCandidates(300)
	cfg := ThresholdConfig{RSMinPct: 2.0, RVOLMin: 1.0, BBPercentileMax: 0.40}
	result := RunWalkForward(candidates, cfg, 126, 21)

	for _, w := range result.Windows {
		if w.TestStart <= w.TrainEnd {
			t.Fatalf("leakage detected: TestStart=%s ≤ TrainEnd=%s",
				w.TestStart, w.TrainEnd)
		}
	}
}

func TestWalkForward_RollingWindowsOnly(t *testing.T) {
	candidates := makeCandidates(300)
	cfg := ThresholdConfig{RSMinPct: 2.0, RVOLMin: 1.0, BBPercentileMax: 0.40}
	result := RunWalkForward(candidates, cfg, 126, 21)

	if len(result.Windows) == 0 {
		t.Fatal("expected at least one window with 300 candidates, train=126, test=21")
	}

	// Windows must be monotonically increasing in time.
	for i := 1; i < len(result.Windows); i++ {
		prev := result.Windows[i-1]
		cur := result.Windows[i]
		if cur.TestStart <= prev.TestStart {
			t.Fatalf("window %d TestStart=%s is not after window %d TestStart=%s",
				i, cur.TestStart, i-1, prev.TestStart)
		}
	}
}

func TestWalkForward_ThresholdFiltersCorrectly(t *testing.T) {
	// Use a strict RS filter that removes all candidates → zero trades.
	candidates := makeCandidates(300)
	cfg := ThresholdConfig{RSMinPct: 999.0} // impossible threshold
	result := RunWalkForward(candidates, cfg, 126, 21)
	if result.TradeCount != 0 {
		t.Fatalf("impossible RS filter should yield 0 trades, got %d", result.TradeCount)
	}
}

func TestWalkForward_InsufficientData_ReturnsEmpty(t *testing.T) {
	candidates := makeCandidates(10) // too few for train=126 test=21
	cfg := ThresholdConfig{}
	result := RunWalkForward(candidates, cfg, 126, 21)
	if len(result.Windows) != 0 {
		t.Fatalf("expected 0 windows with insufficient data, got %d", len(result.Windows))
	}
}

func TestWalkForward_StabilityScore_ComputedCorrectly(t *testing.T) {
	candidates := makeCandidates(300)
	cfg := ThresholdConfig{RSMinPct: 2.0, RVOLMin: 1.0, BBPercentileMax: 0.40}
	result := RunWalkForward(candidates, cfg, 126, 21)

	if len(result.Windows) < 2 {
		t.Skip("need ≥2 windows for stability score")
	}
	if result.StabilityScore < 0 {
		t.Fatalf("stability score must be non-negative, got %.4f", result.StabilityScore)
	}
}

func TestWalkForward_ThresholdGrid_NoCombinationDuplication(t *testing.T) {
	grid := ThresholdGrid()
	seen := make(map[ThresholdConfig]bool)
	for _, cfg := range grid {
		if seen[cfg] {
			t.Fatalf("duplicate threshold config: %+v", cfg)
		}
		seen[cfg] = true
	}
	// Expected: 4 × 3 × 3 × 3 × 5 = 540 combinations
	expected := 4 * 3 * 3 * 3 * 5
	if len(grid) != expected {
		t.Fatalf("expected %d threshold combinations, got %d", expected, len(grid))
	}
}
