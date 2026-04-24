package market

import (
	"testing"
)

func makeContract(optType string, dte int, delta, spreadPct float64, oi int) OptionContract {
	absDelta := delta
	if absDelta < 0 {
		absDelta = -absDelta
	}
	return OptionContract{
		Type:         optType,
		DTE:          dte,
		Delta:        delta,
		SpreadPct:    spreadPct,
		OpenInterest: oi,
		OptionVolume: oi / 2,
		Bid:          1.00,
		Ask:          1.00 + 1.00*(spreadPct/100),
	}
}

var defaultOpts = ContractSelectionOpts{
	DTEMin: 7, DTEMax: 14, AvoidDTEBelow: 4,
	TargetDTE: 10, DeltaMin: 0.30, DeltaMax: 0.70,
}

// ── DTE filtering ─────────────────────────────────────────────────────────────

func TestSelectBestContract_RefusesDTEBelowAvoid(t *testing.T) {
	contracts := []OptionContract{
		makeContract("call", 3, 0.50, 2.0, 500), // DTE=3 < AvoidDTEBelow=4
		makeContract("call", 10, 0.50, 2.0, 500),
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best == nil {
		t.Fatal("expected a result")
	}
	if best.DTE == 3 {
		t.Fatalf("should refuse DTE=3 (below AvoidDTEBelow=4), selected DTE=%d", best.DTE)
	}
	if best.DTE != 10 {
		t.Fatalf("expected DTE=10, got DTE=%d", best.DTE)
	}
}

func TestSelectBestContract_RefusesDTEOutsideRange(t *testing.T) {
	contracts := []OptionContract{
		makeContract("call", 5, 0.50, 2.0, 500),  // DTE=5 < DTEMin=7
		makeContract("call", 20, 0.50, 2.0, 500), // DTE=20 > DTEMax=14
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best != nil {
		t.Fatalf("expected nil (no contracts in DTE range), got DTE=%d", best.DTE)
	}
}

func TestSelectBestContract_PrefersClosestToTargetDTE(t *testing.T) {
	// Target DTE=10; both are in range. DTE=10 should be preferred over DTE=14.
	contracts := []OptionContract{
		makeContract("call", 10, 0.50, 2.0, 500),
		makeContract("call", 14, 0.50, 2.0, 500),
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best == nil {
		t.Fatal("expected result")
	}
	if best.DTE != 10 {
		t.Fatalf("expected DTE=10 (closest to target), got DTE=%d", best.DTE)
	}
}

func TestSelectBestContract_PrefersClosestToTargetDTE_OneBelowOneAbove(t *testing.T) {
	// DTE=9 is 1 away; DTE=13 is 3 away. Should pick DTE=9.
	contracts := []OptionContract{
		makeContract("call", 9, 0.50, 2.0, 500),
		makeContract("call", 13, 0.50, 2.0, 500),
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best.DTE != 9 {
		t.Fatalf("expected DTE=9, got DTE=%d", best.DTE)
	}
}

// ── Direction filter ──────────────────────────────────────────────────────────

func TestSelectBestContract_FiltersWrongType(t *testing.T) {
	contracts := []OptionContract{
		makeContract("put", 10, -0.50, 2.0, 500), // wrong type for "call" search
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best != nil {
		t.Fatal("expected nil — no call contracts available")
	}
}

// ── Delta preference ──────────────────────────────────────────────────────────

func TestSelectBestContract_PrefersInBandDeltaOverOutOfBand(t *testing.T) {
	// DTE same; delta=0.50 is in-band (0.30–0.70); delta=0.80 is out-of-band.
	contracts := []OptionContract{
		makeContract("call", 10, 0.80, 2.0, 500), // out of band
		makeContract("call", 10, 0.50, 2.0, 500), // in band
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best.Delta != 0.50 {
		t.Fatalf("expected in-band delta=0.50, got %.2f", best.Delta)
	}
}

func TestSelectBestContract_FallsBackToOutOfBandWhenNoBandContracts(t *testing.T) {
	// Only out-of-band contracts — should still return something.
	contracts := []OptionContract{
		makeContract("call", 10, 0.90, 2.0, 500),
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best == nil {
		t.Fatal("expected fallback to out-of-band contract when no in-band available")
	}
}

// ── Nil when no candidates ────────────────────────────────────────────────────

func TestSelectBestContract_NilOnEmpty(t *testing.T) {
	best := SelectBestContract(nil, "call", defaultOpts)
	if best != nil {
		t.Fatal("expected nil on empty input")
	}
}

func TestSelectBestContract_NilWhenAllFilteredOut(t *testing.T) {
	contracts := []OptionContract{
		makeContract("call", 3, 0.50, 2.0, 500), // DTE=3, below AvoidDTEBelow=4
	}
	best := SelectBestContract(contracts, "call", defaultOpts)
	if best != nil {
		t.Fatalf("expected nil — all contracts below avoid_dte_below, got DTE=%d", best.DTE)
	}
}
