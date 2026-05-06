// internal/market/finra.go
//
// WHAT: Fetches official short interest data for a ticker from FINRA's free REST API.
//
// WHY:  FINRA Rule 4560 requires member firms to report short positions twice monthly.
//       This gives us authoritative short interest counts — not scraped estimates.
//       The TREND (rising vs falling short interest) matters as much as the level:
//         - Rising short + bearish setup = strong confirmation
//         - Falling short + bearish setup = potential squeeze risk
//         - Rising short + bullish setup = squeeze fuel (contrarian signal)
//
// SOURCE: https://api.finra.org/data/group/otcMarket/name/EquityShortInterest
// UPDATE: Twice monthly (FINRA settlement dates, typically 15th and last business day).
// AUTH:   None required for basic access.
//
// WHAT BREAKS: FINRA API returns empty array for tickers not on their list
//              (e.g. ETFs, some small caps). Callers should check ShortShares > 0.

package market

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const finraShortURL = "https://api.finra.org/data/group/otcMarket/name/EquityShortInterest"

// FinraShortInterest holds the two most recent FINRA short interest reports for a ticker.
type FinraShortInterest struct {
	Symbol         string
	SettlementDate string  // most recent reporting date "YYYY-MM-DD"
	ShortShares    int64   // current short interest (shares)
	PrevShares     int64   // prior period short interest
	ChangePercent  float64 // (current - prev) / prev * 100
	Trend          string  // "rising" | "falling" | "flat"
}

type finraRow struct {
	Symbol         string  `json:"issueSymbolIdentifier"`
	SettlementDate string  `json:"settlementDate"`
	ShortShares    float64 `json:"currentShortInterestShareNumber"`   // FINRA uses float in JSON
	PrevShares     float64 `json:"previousShortInterestShareNumber"`
	ChangePercent  float64 `json:"shortInterestChangePercent"`
}

// FetchFinraShortInterest returns the latest FINRA short interest for ticker.
// Returns a zero-value result (ShortShares=0) on error so callers degrade gracefully.
func FetchFinraShortInterest(ticker string) (FinraShortInterest, error) {
	result := FinraShortInterest{Symbol: ticker}

	filters := fmt.Sprintf(`[{"fieldName":"issueSymbolIdentifier","compareType":"equal","fieldValue":"%s"}]`, ticker)
	params := url.Values{}
	params.Set("limit", "1")
	params.Set("offset", "0")
	params.Set("compareFilters", filters)
	params.Set("sortFields", `[{"fieldName":"settlementDate","sortType":"DESC"}]`)

	reqURL := finraShortURL + "?" + params.Encode()

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; research-bot/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return result, fmt.Errorf("finra fetch %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result, fmt.Errorf("finra %s HTTP %d", ticker, resp.StatusCode)
	}

	var rows []finraRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return result, fmt.Errorf("finra decode %s: %w", ticker, err)
	}
	if len(rows) == 0 {
		return result, nil // ticker not in FINRA dataset — not an error
	}

	row := rows[0]
	result.SettlementDate = row.SettlementDate
	result.ShortShares = int64(row.ShortShares)
	result.PrevShares = int64(row.PrevShares)
	result.ChangePercent = row.ChangePercent

	switch {
	case result.ChangePercent > 5:
		result.Trend = "rising"
	case result.ChangePercent < -5:
		result.Trend = "falling"
	default:
		result.Trend = "flat"
	}

	return result, nil
}
