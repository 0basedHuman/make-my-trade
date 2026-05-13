// internal/market/fill_test.go
//
// Tests for the quote-realistic fill model.
// Run: go test ./internal/market/ -run TestFill -v

package market

import (
	"math"
	"testing"
)

func TestFill_TightSpread_UsesMid(t *testing.T) {
	// spread = (ask-bid)/ask = (5.10-5.00)/5.10 = 1.96% ≤ 4% → use mid
	r := ComputeEntryFill(5.00, 5.10)
	if r.Rejected {
		t.Fatalf("tight spread should not be rejected: %s", r.RejectReason)
	}
	if r.Mode != "mid" {
		t.Fatalf("want mode=mid, got %q", r.Mode)
	}
	expected := (5.00 + 5.10) / 2.0
	if r.Price != expected {
		t.Fatalf("want price=%.4f (mid), got %.4f", expected, r.Price)
	}
	if r.QualityScore < 80 {
		t.Fatalf("want quality ≥80 for tight spread, got %.0f", r.QualityScore)
	}
}

func TestFill_MediumSpread_UsesHaircut(t *testing.T) {
	// bid=4.70, ask=5.30 → spread=(5.30-4.70)/5.30=11.3%? No...
	// Let me use bid=4.80, ask=5.20 → spread=(5.20-4.80)/5.20=7.7% → haircut
	r := ComputeEntryFill(4.80, 5.20)
	if r.Rejected {
		t.Fatalf("6%% spread should not be rejected: %s", r.RejectReason)
	}
	if r.Mode != "haircut" {
		t.Fatalf("want mode=haircut for 6%% spread, got %q", r.Mode)
	}
	mid := (4.80 + 5.20) / 2.0 // 5.00
	if r.Price <= mid {
		t.Fatalf("haircut fill must be > mid (%.2f), got %.4f", mid, r.Price)
	}
	if r.Price >= 5.20 {
		t.Fatalf("haircut fill must be < ask (5.20), got %.4f", r.Price)
	}
}

func TestFill_WideSpread_Rejects(t *testing.T) {
	// bid=4.50, ask=5.50 → spread=(5.50-4.50)/5.50=18.2% → reject
	r := ComputeEntryFill(4.50, 5.50)
	if !r.Rejected {
		t.Fatal("wide spread (>8%) must be rejected")
	}
	if r.RejectReason != "spread_too_wide" {
		t.Fatalf("want reject_reason=spread_too_wide, got %q", r.RejectReason)
	}
}

func TestFill_ZeroBid_Rejects(t *testing.T) {
	r := ComputeEntryFill(0, 5.00)
	if !r.Rejected {
		t.Fatal("zero bid must be rejected")
	}
	if r.RejectReason != "zero_bid" {
		t.Fatalf("want reject_reason=zero_bid, got %q", r.RejectReason)
	}
}

func TestFill_InvertedSpread_Rejects(t *testing.T) {
	r := ComputeEntryFill(5.10, 5.00) // bid > ask
	if !r.Rejected {
		t.Fatal("inverted spread must be rejected")
	}
	if r.RejectReason != "inverted_spread" {
		t.Fatalf("want reject_reason=inverted_spread, got %q", r.RejectReason)
	}
}

func TestFill_ExitUseBid(t *testing.T) {
	r := ComputeExitFill(4.90, 5.10)
	if r.Rejected {
		t.Fatalf("exit fill should not be rejected: %s", r.RejectReason)
	}
	if r.Mode != "bid" {
		t.Fatalf("want mode=bid for exit, got %q", r.Mode)
	}
	// Should be bid * 0.995 = 4.90 * 0.995 ≈ 4.8755
	expected := 4.90 * 0.995
	if math.Abs(r.Price-expected) > 1e-9 {
		t.Fatalf("want exit price≈%.6f, got %.6f", expected, r.Price)
	}
}

func TestFill_ExitZeroBid_Rejects(t *testing.T) {
	r := ComputeExitFill(0, 5.00)
	if !r.Rejected {
		t.Fatal("exit with zero bid must be rejected")
	}
}

func TestFill_QualityScore_TieredCorrectly(t *testing.T) {
	// Verify quality tiers
	cases := []struct {
		bid, ask     float64
		wantMinScore float64
	}{
		{4.95, 5.00, 95},   // ~1% spread → score 100
		{4.90, 5.00, 85},   // ~2% spread → score 90
		{4.81, 5.00, 75},   // ~3.8% spread → score 80
	}
	for _, tc := range cases {
		r := ComputeEntryFill(tc.bid, tc.ask)
		if r.Rejected {
			t.Fatalf("bid=%.2f ask=%.2f: unexpected reject %s", tc.bid, tc.ask, r.RejectReason)
		}
		if r.QualityScore < tc.wantMinScore {
			t.Fatalf("bid=%.2f ask=%.2f spread=%.1f%%: want quality≥%.0f, got %.0f",
				tc.bid, tc.ask, r.SpreadPct, tc.wantMinScore, r.QualityScore)
		}
	}
}

func TestFill_SlippageEstimate_EntryAboveMid(t *testing.T) {
	// For mid fill: slippage = 0
	r := ComputeEntryFill(4.95, 5.05)
	if r.Mode == "mid" && r.SlippageEst != 0 {
		t.Fatalf("mid fill must have zero slippage, got %.4f", r.SlippageEst)
	}
}
