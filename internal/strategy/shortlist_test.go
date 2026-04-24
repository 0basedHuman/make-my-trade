package strategy

import "testing"

func TestShortlistEntryReady(t *testing.T) {
	analyses := []SymbolAnalysis{
		{Ticker: "A", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 80}},
		{Ticker: "B", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 72}},
		{Ticker: "C", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 65}},
		{Ticker: "D", CandidateStatus: "structural_candidate", ScoreBreakdown: FamilyScore{FinalScore: 90}},
		{Ticker: "E", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 50}}, // below minScore
		{Ticker: "F", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 75}},
	}

	// minScore=65, maxCount=3
	result := ShortlistEntryReady(analyses, 3, 65)

	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}
	// Should be sorted by score descending: A(80), F(75), B(72)
	expected := []string{"A", "F", "B"}
	for i, a := range result {
		if a.Ticker != expected[i] {
			t.Errorf("result[%d]: expected %s, got %s", i, expected[i], a.Ticker)
		}
	}

	// structural_candidate D must not appear even with high score
	for _, a := range result {
		if a.Ticker == "D" {
			t.Error("structural_candidate D should not appear in shortlist")
		}
		if a.Ticker == "E" {
			t.Error("E (score=50) should not appear with minScore=65")
		}
	}
}

func TestShortlistEntryReadyEmpty(t *testing.T) {
	// All below minScore → empty result
	analyses := []SymbolAnalysis{
		{Ticker: "A", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 40}},
	}
	result := ShortlistEntryReady(analyses, 5, 65)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d items", len(result))
	}
}

func TestShortlistEntryReadyNoLimit(t *testing.T) {
	// maxCount=0 → no cap
	analyses := []SymbolAnalysis{
		{Ticker: "A", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 80}},
		{Ticker: "B", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 75}},
		{Ticker: "C", CandidateStatus: "entry_ready", ScoreBreakdown: FamilyScore{FinalScore: 70}},
	}
	result := ShortlistEntryReady(analyses, 0, 65)
	if len(result) != 3 {
		t.Errorf("expected 3 results (no cap), got %d", len(result))
	}
}
