// internal/market/cboe.go
//
// WHAT: Fetches the CBOE daily equity put/call ratio from their free CSV endpoint.
//
// WHY:  The equity P/C ratio is a market-wide sentiment gauge.
//       P/C > 1.0 → more puts than calls → crowd is buying protection → fear dominates.
//       P/C < 0.7 → complacency → crowd is buying calls without hedging.
//       Regime context for the Claude reviewer and scoring engine.
//
// SOURCE: https://cdn.cboe.com/resources/options/volume_and_call_put_ratios/equitypc.csv
// FORMAT: DATE,CALL,PUT,TOTAL,P/C Ratio (header row + daily rows, oldest first)
// UPDATE: Daily EOD, no auth required.

package market

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const cboeEquityPCURL = "https://cdn.cboe.com/resources/options/volume_and_call_put_ratios/equitypc.csv"

// PCRatioSnapshot is the most recent CBOE equity put/call ratio.
type PCRatioSnapshot struct {
	Date    string  // "MM/DD/YYYY"
	Calls   float64 // equity call volume
	Puts    float64 // equity put volume
	PCRatio float64 // puts / calls
	Bias    string  // "fear" | "complacency" | "neutral"
}

// FetchEquityPCRatio downloads the CBOE equity P/C CSV and returns the most recent row.
// Returns a zero-value snapshot (PCRatio=0) on any error so callers can degrade gracefully.
func FetchEquityPCRatio() (PCRatioSnapshot, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("GET", cboeEquityPCURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; research-bot/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return PCRatioSnapshot{}, fmt.Errorf("cboe pc-ratio fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return PCRatioSnapshot{}, fmt.Errorf("cboe pc-ratio HTTP %d", resp.StatusCode)
	}

	r := csv.NewReader(resp.Body)
	r.TrimLeadingSpace = true

	// Skip header
	if _, err := r.Read(); err != nil {
		return PCRatioSnapshot{}, fmt.Errorf("cboe pc-ratio header: %w", err)
	}

	// Read all rows, keep last non-empty
	var last []string
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(row) >= 5 && strings.TrimSpace(row[0]) != "" {
			last = row
		}
	}
	if last == nil {
		return PCRatioSnapshot{}, fmt.Errorf("cboe pc-ratio: no data rows")
	}

	calls, _ := strconv.ParseFloat(strings.TrimSpace(last[1]), 64)
	puts, _ := strconv.ParseFloat(strings.TrimSpace(last[2]), 64)
	pc, _ := strconv.ParseFloat(strings.TrimSpace(last[4]), 64)

	snap := PCRatioSnapshot{
		Date:    strings.TrimSpace(last[0]),
		Calls:   calls,
		Puts:    puts,
		PCRatio: pc,
	}
	switch {
	case pc >= 1.0:
		snap.Bias = "fear"
	case pc <= 0.7:
		snap.Bias = "complacency"
	default:
		snap.Bias = "neutral"
	}
	return snap, nil
}
