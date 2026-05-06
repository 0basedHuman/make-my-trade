// internal/market/yahoo.go
//
// WHAT: Fetches per-ticker options put/call ratio from Yahoo Finance's
//       unofficial options JSON endpoint. No API key required.
//
// WHY:  The per-ticker P/C ratio shows how the options market is positioned
//       on a specific stock — different from the market-wide CBOE ratio.
//         - High P/C (puts >> calls): options market is hedging or betting down
//         - Low P/C (calls >> puts): crowd is buying calls (bullish or speculative)
//       Computed from open interest across the nearest two expirations.
//
// SOURCE: https://query1.finance.yahoo.com/v7/finance/options/AAPL
// UPDATE: ~15-minute delayed during market hours.
// AUTH:   None. Unofficial endpoint — may break if Yahoo changes their API.
//
// WHAT BREAKS: Yahoo may change the JSON schema or add auth requirements.
//              On any parse error the function returns PCRatio=0 gracefully.

package market

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const yahooOptionsURL = "https://query1.finance.yahoo.com/v7/finance/options/"

// YahooPCRatio is the per-ticker options put/call ratio from Yahoo Finance.
type YahooPCRatio struct {
	Ticker  string
	CallOI  int64   // total call open interest (nearest 2 expirations)
	PutOI   int64   // total put open interest
	PCRatio float64 // PutOI / CallOI; 0 if no data
	Bias    string  // "put_heavy" | "call_heavy" | "balanced"
}

// yahooOptionsResponse is the minimal JSON shape we parse from Yahoo Finance.
type yahooOptionsResponse struct {
	OptionChain struct {
		Result []struct {
			Options []struct {
				Calls []yahooContract `json:"calls"`
				Puts  []yahooContract `json:"puts"`
			} `json:"options"`
		} `json:"result"`
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	} `json:"optionChain"`
}

type yahooContract struct {
	OpenInterest int64 `json:"openInterest"`
}

// FetchYahooPCRatio returns the put/call OI ratio for the given ticker.
// Returns a zero-value result on any error.
func FetchYahooPCRatio(ticker string) (YahooPCRatio, error) {
	result := YahooPCRatio{Ticker: ticker, Bias: "balanced"}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", yahooOptionsURL+ticker, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return result, fmt.Errorf("yahoo options fetch %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result, fmt.Errorf("yahoo options %s HTTP %d", ticker, resp.StatusCode)
	}

	var data yahooOptionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return result, fmt.Errorf("yahoo options decode %s: %w", ticker, err)
	}
	if data.OptionChain.Error != nil {
		return result, fmt.Errorf("yahoo options %s: %s", ticker, data.OptionChain.Error.Description)
	}
	if len(data.OptionChain.Result) == 0 {
		return result, nil
	}

	// Sum OI across all returned expirations (Yahoo returns nearest by default)
	var totalCallOI, totalPutOI int64
	for _, res := range data.OptionChain.Result {
		for _, opt := range res.Options {
			for _, c := range opt.Calls {
				totalCallOI += c.OpenInterest
			}
			for _, p := range opt.Puts {
				totalPutOI += p.OpenInterest
			}
		}
	}

	result.CallOI = totalCallOI
	result.PutOI = totalPutOI
	if totalCallOI > 0 {
		result.PCRatio = float64(totalPutOI) / float64(totalCallOI)
	}
	switch {
	case result.PCRatio >= 1.2:
		result.Bias = "put_heavy"
	case result.PCRatio <= 0.6:
		result.Bias = "call_heavy"
	default:
		result.Bias = "balanced"
	}

	return result, nil
}
