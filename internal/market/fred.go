// internal/market/fred.go
//
// WHAT: FRED (Federal Reserve Economic Data) client.
//       Used to fetch the CBOE Volatility Index (VIX) and macro series.
//
// WHY:  VIX is a hard qualifier gate — if VIX >= 24 we reject all setups.
//       FRED's VIXCLS series is authoritative, updated daily after market close.
//       Alpaca doesn't carry VIX as a ticker on the free data feed.
//
// HOW:  Single JSON endpoint: api.stlouisfed.org/fred/series/observations
//       We fetch the most recent observation and return it as a float64.
//
// WHAT BREAKS: FRED updates VIXCLS the next business day after 4 PM ET.
//              If you call before that, you get yesterday's value — which is
//              correct for pre-open analysis (we want the prior close VIX).
//
// VERIFY: curl "https://api.stlouisfed.org/fred/series/observations?
//               series_id=VIXCLS&sort_order=desc&limit=1&api_key=<KEY>&file_type=json"

package market

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// FREDClient wraps the St. Louis Fed REST API.
type FREDClient struct {
	apiKey string
	http   *http.Client
}

// NewFREDClient creates a client with the given API key.
func NewFREDClient(apiKey string) *FREDClient {
	return &FREDClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

const fredBase = "https://api.stlouisfed.org/fred"

// fredObservation is one FRED data point.
type fredObservation struct {
	Date  string `json:"date"`
	Value string `json:"value"` // FRED sends "." for missing values
}

// FetchLatestVIX returns the most recent CBOE VIX close from FRED series VIXCLS.
// If FRED is unavailable or the value is missing, returns 20.0 (neutral assumption)
// and logs a warning.
func (c *FREDClient) FetchLatestVIX() (float64, string, error) {
	return c.fetchLatestObservation("VIXCLS")
}

// FetchLatestDXY fetches the DXY (US Dollar Index) from FRED DTWEXBGS.
func (c *FREDClient) FetchLatestDXY() (float64, string, error) {
	return c.fetchLatestObservation("DTWEXBGS")
}

// FetchRecentVIX fetches the last N observations of VIX.
func (c *FREDClient) FetchRecentVIX(limit int) ([]float64, error) {
	vals, _, err := c.fetchObservations("VIXCLS", limit)
	return vals, err
}

func (c *FREDClient) fetchLatestObservation(seriesID string) (float64, string, error) {
	vals, dates, err := c.fetchObservations(seriesID, 5) // fetch last 5 to handle missing
	if err != nil {
		return 20.0, "", fmt.Errorf("fred %s: %w", seriesID, err)
	}
	for i := len(vals) - 1; i >= 0; i-- {
		if vals[i] > 0 {
			return vals[i], dates[i], nil
		}
	}
	return 20.0, "", fmt.Errorf("fred %s: no valid observation found", seriesID)
}

func (c *FREDClient) fetchObservations(seriesID string, limit int) ([]float64, []string, error) {
	reqURL := fmt.Sprintf(
		"%s/series/observations?series_id=%s&sort_order=desc&limit=%d&api_key=%s&file_type=json",
		fredBase, seriesID, limit, c.apiKey,
	)

	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, nil, fmt.Errorf("fred GET: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("fred HTTP %d: %s", resp.StatusCode, string(body))
	}

	var envelope struct {
		Observations []fredObservation `json:"observations"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil, fmt.Errorf("fred decode: %w", err)
	}

	// Reverse to get oldest-first
	obs := envelope.Observations
	for i, j := 0, len(obs)-1; i < j; i, j = i+1, j-1 {
		obs[i], obs[j] = obs[j], obs[i]
	}

	vals := make([]float64, 0, len(obs))
	dates := make([]string, 0, len(obs))
	for _, o := range obs {
		if o.Value == "." || o.Value == "" {
			continue
		}
		v, err := strconv.ParseFloat(o.Value, 64)
		if err != nil {
			continue
		}
		vals = append(vals, v)
		dates = append(dates, o.Date)
	}
	return vals, dates, nil
}
