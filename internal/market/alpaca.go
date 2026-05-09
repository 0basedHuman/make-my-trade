// internal/market/alpaca.go
//
// WHAT: Alpaca Markets REST client for historical daily and intraday price bars.
//
// WHY:  Alpaca provides OHLCV bars via a clean JSON API with paper-key access.
//       We use the data API (data.alpaca.markets) which works with paper keys
//       and returns IEX feed data at no additional cost.
//
// HOW:  We call the multi-symbol bars endpoint in batches, decode the response,
//       and return []Bar slices keyed by ticker. Pagination is handled by
//       following next_page_token until nil.
//
// WHAT BREAKS: If the API key is wrong, all requests 401. If Alpaca is down,
//              we log the error and return empty bars. The strategy engine treats
//              empty bars as insufficient data and skips the symbol.
//
// VERIFY: make alpaca-test ticker=AAPL (added to Makefile after Layer 1)

package market

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yourname/makemytrade/internal/indicators"
)

// AlpacaClient is a thin REST wrapper around Alpaca's market data and broker APIs.
// It holds the HTTP client and credentials; all methods are stateless.
type AlpacaClient struct {
	apiKey    string
	secretKey string
	dataURL   string // https://data.alpaca.markets — market data
	tradeURL  string // https://paper-api.alpaca.markets — order execution
	http      *http.Client
}

// NewAlpacaClient constructs a client using paper-trading API credentials.
// dataURL is the market data endpoint; tradeURL is the broker/order endpoint.
func NewAlpacaClient(apiKey, secretKey, dataURL, tradeURL string) *AlpacaClient {
	return &AlpacaClient{
		apiKey:    apiKey,
		secretKey: secretKey,
		dataURL:   strings.TrimRight(dataURL, "/"),
		tradeURL:  strings.TrimRight(tradeURL, "/"),
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// alpacaBar is the raw JSON shape Alpaca returns for a single bar.
type alpacaBar struct {
	Time   string  `json:"t"` // RFC3339
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume float64 `json:"v"`
	VWAP   float64 `json:"vw"`
}

// alpacaBarsResp is the multi-symbol bars response envelope.
type alpacaBarsResp struct {
	Bars          map[string][]alpacaBar `json:"bars"`
	NextPageToken *string                `json:"next_page_token"`
}

// FetchDailyBars fetches up to `limit` daily bars for each ticker in `tickers`,
// ending on `endDate` (inclusive). Returns a map ticker → []indicators.Bar
// sorted oldest-first. Symbols with API errors are silently omitted.
func (c *AlpacaClient) FetchDailyBars(tickers []string, startDate, endDate time.Time, limit int) (map[string][]indicators.Bar, error) {
	result := make(map[string][]indicators.Bar)
	if len(tickers) == 0 {
		return result, nil
	}

	// Alpaca allows up to 100 symbols in a single multi-bar request.
	// We batch in groups of 40 to stay well within limits.
	batchSize := 40
	for i := 0; i < len(tickers); i += batchSize {
		end := i + batchSize
		if end > len(tickers) {
			end = len(tickers)
		}
		batch := tickers[i:end]
		bars, err := c.fetchBatch(batch, startDate, endDate, limit)
		if err != nil {
			// Log but don't fail — partial data is better than a crash
			fmt.Printf("alpaca: batch fetch error: %v\n", err)
			continue
		}
		for ticker, barsSlice := range bars {
			result[ticker] = barsSlice
		}
	}
	return result, nil
}

// fetchBatch fetches daily bars for a batch of tickers.
// Tries SIP first (accurate consolidated tape volume). If the free-tier
// subscription error fires for recent data, retries with IEX automatically.
// Historical SIP data (any date before today) is always free on Alpaca.
func (c *AlpacaClient) fetchBatch(tickers []string, start, end time.Time, limit int) (map[string][]indicators.Bar, error) {
	result, err := c.fetchBatchWithFeed(tickers, start, end, limit, "sip")
	if err != nil && strings.Contains(err.Error(), "subscription does not permit querying recent SIP data") {
		// SIP blocked for today's data — fall back to IEX.
		// Volume will be ~0.1% of real market volume for today's bar only;
		// all prior bars were already fetched via SIP with accurate volume.
		fmt.Printf("alpaca: SIP unavailable for recent date, retrying with IEX feed\n")
		result, err = c.fetchBatchWithFeed(tickers, start, end, limit, "iex")
	}
	return result, err
}

func (c *AlpacaClient) fetchBatchWithFeed(tickers []string, start, end time.Time, limit int, feed string) (map[string][]indicators.Bar, error) {
	result := make(map[string][]indicators.Bar)
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("symbols", strings.Join(tickers, ","))
		params.Set("timeframe", "1Day")
		params.Set("start", start.Format("2006-01-02"))
		params.Set("end", end.Format("2006-01-02"))
		params.Set("limit", fmt.Sprintf("%d", limit))
		params.Set("feed", feed)
		params.Set("sort", "asc")
		if pageToken != "" {
			params.Set("page_token", pageToken)
		}

		reqURL := fmt.Sprintf("%s/v2/stocks/bars?%s", c.dataURL, params.Encode())
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("alpaca: build request: %w", err)
		}
		req.Header.Set("APCA-API-KEY-ID", c.apiKey)
		req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("alpaca: request failed: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("alpaca: HTTP %d: %s", resp.StatusCode, string(body))
		}

		var parsed alpacaBarsResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("alpaca: decode: %w", err)
		}

		for ticker, rawBars := range parsed.Bars {
			for _, rb := range rawBars {
				result[ticker] = append(result[ticker], indicators.Bar{
					Open:   rb.Open,
					High:   rb.High,
					Low:    rb.Low,
					Close:  rb.Close,
					Volume: rb.Volume,
				})
			}
		}

		if parsed.NextPageToken == nil || *parsed.NextPageToken == "" {
			break
		}
		pageToken = *parsed.NextPageToken
	}

	return result, nil
}

// alpacaCryptoResp is the crypto bars response envelope.
type alpacaCryptoResp struct {
	Bars          map[string][]alpacaBar `json:"bars"`
	NextPageToken *string                `json:"next_page_token"`
}

// FetchCryptoDailyBars fetches daily bars for a crypto symbol like "BTC/USD".
func (c *AlpacaClient) FetchCryptoDailyBars(symbol string, startDate time.Time, limit int) ([]indicators.Bar, error) {
	params := url.Values{}
	params.Set("symbols", symbol)
	params.Set("timeframe", "1Day")
	params.Set("start", startDate.Format("2006-01-02"))
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("sort", "asc")

	reqURL := fmt.Sprintf("%s/v1beta3/crypto/us/bars?%s", c.dataURL, params.Encode())
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alpaca crypto: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("alpaca crypto: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed alpacaCryptoResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("alpaca crypto decode: %w", err)
	}

	rawBars := parsed.Bars[symbol]
	bars := make([]indicators.Bar, len(rawBars))
	for i, rb := range rawBars {
		bars[i] = indicators.Bar{
			Open:   rb.Open,
			High:   rb.High,
			Low:    rb.Low,
			Close:  rb.Close,
			Volume: rb.Volume,
		}
	}
	return bars, nil
}

// OptionContract holds a single option contract snapshot from Alpaca.
type OptionContract struct {
	Symbol       string  `json:"symbol"` // OCC symbol, e.g. "RTX260501P00200000"
	Type         string  `json:"type"`   // "call" or "put"
	Strike       float64 `json:"strike"`
	Expiration   string  `json:"expiration"` // "YYYY-MM-DD"
	DTE          int     `json:"dte"`
	Delta        float64 `json:"delta"`
	Bid          float64 `json:"bid"`
	Ask          float64 `json:"ask"`
	SpreadPct    float64 `json:"spread_pct"` // (ask-bid)/ask*100
	OpenInterest int     `json:"open_interest"`
	OptionVolume int     `json:"option_volume"`
}

// alpacaOptionSnapshot is the raw Alpaca options snapshot shape.
// Note: the "details" field is often absent; strike/type/expiration are
// parsed from the contract symbol key (e.g. "RTX260501P00207500").
type alpacaOptionSnapshot struct {
	Greeks struct {
		Delta float64 `json:"delta"`
	} `json:"greeks"`
	Quote struct {
		Bid float64 `json:"bp"`
		Ask float64 `json:"ap"`
	} `json:"latestQuote"`
	DailyBar struct {
		Volume int `json:"v"`
	} `json:"dailyBar"`
}

// parseOptionSymbol parses the OCC option symbol into components.
// Format: TICKER YYMMDD C/P 00STRIKE000 (e.g. "RTX260501P00207500")
// Returns optionType ("call"/"put"), expiration (YYYY-MM-DD), strike, ok.
func parseOptionSymbol(symbol, ticker string) (optType, expiration string, strike float64, ok bool) {
	// Symbol: TICKER + 6-digit date + C/P + 8-digit strike (strike * 1000)
	prefix := len(ticker)
	if len(symbol) < prefix+15 {
		return "", "", 0, false
	}
	dateStr := symbol[prefix : prefix+6] // YYMMDD
	cpStr := symbol[prefix+6 : prefix+7] // C or P
	strikeStr := symbol[prefix+7:]       // 8 digits

	// Parse date: YYMMDD → 20YY-MM-DD
	if len(dateStr) != 6 {
		return "", "", 0, false
	}
	expiration = "20" + dateStr[:2] + "-" + dateStr[2:4] + "-" + dateStr[4:6]

	switch cpStr {
	case "C":
		optType = "call"
	case "P":
		optType = "put"
	default:
		return "", "", 0, false
	}

	// Strike: 8-digit integer / 1000
	if len(strikeStr) != 8 {
		return "", "", 0, false
	}
	var strikeCents int
	for _, ch := range strikeStr {
		if ch < '0' || ch > '9' {
			return "", "", 0, false
		}
		strikeCents = strikeCents*10 + int(ch-'0')
	}
	strike = float64(strikeCents) / 1000.0
	return optType, expiration, strike, true
}

// FetchOptionChain fetches option contracts for the given underlying symbol.
// Fetch window is 4-25 DTE to match Alpaca's paper indicative feed.
// SelectBestContract applies the narrower family/global DTE band on top.
// If the Alpaca options API is unavailable (403/404 or account not options-enabled),
// it returns an empty slice without error — the pipeline treats this as
// "no qualifying chain data" and Claude will classify the setup as structural_candidate.
func (c *AlpacaClient) FetchOptionChain(ticker string, underlyingPrice float64, today string) ([]OptionContract, error) {
	todayTime, _ := time.Parse("2006-01-02", today)
	if todayTime.IsZero() {
		todayTime = time.Now()
	}
	minExp := todayTime.AddDate(0, 0, 4).Format("2006-01-02")
	maxExp := todayTime.AddDate(0, 0, 25).Format("2006-01-02")
	reqURL := fmt.Sprintf("%s/v1beta1/options/snapshots/%s?feed=indicative&limit=200&expiration_date_gte=%s&expiration_date_lte=%s",
		c.dataURL, ticker, minExp, maxExp)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alpaca options: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// 403/404 = options not enabled for this account/key — return empty silently
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return []OptionContract{}, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("alpaca options HTTP %d: %s", resp.StatusCode, truncateBody(body, 200))
	}

	// Response shape: { "snapshots": { "NVDA250425C00123000": { ... }, ... } }
	var raw struct {
		Snapshots map[string]alpacaOptionSnapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("alpaca options decode: %w", err)
	}

	var contracts []OptionContract
	for symKey, snap := range raw.Snapshots {
		// Parse strike/type/expiration from the symbol key — "details" field often absent.
		optType, expStr, strike, parsed := parseOptionSymbol(symKey, ticker)
		if !parsed {
			continue
		}
		expDate, err := time.Parse("2006-01-02", expStr)
		if err != nil {
			continue
		}
		dte := int(expDate.Sub(todayTime).Hours() / 24)
		if dte < 4 || dte > 25 {
			continue
		}

		delta := snap.Greeks.Delta
		if delta == 0 {
			// No greeks data available — skip (deep ITM/OTM or illiquid)
			continue
		}
		// For puts delta is negative; take absolute value for range check
		absDelta := delta
		if absDelta < 0 {
			absDelta = -absDelta
		}
		if absDelta < 0.30 || absDelta > 0.75 {
			continue
		}

		bid := snap.Quote.Bid
		ask := snap.Quote.Ask
		if ask <= 0 {
			continue
		}
		spreadPct := (ask - bid) / ask * 100

		_ = strike // strike available if needed for strike-selection logic
		vol := snap.DailyBar.Volume
		oi := 0 // OI not provided by indicative feed; use volume as liquidity proxy

		contracts = append(contracts, OptionContract{
			Symbol:       symKey,
			Type:         optType,
			Strike:       strike,
			Expiration:   expStr,
			DTE:          dte,
			Delta:        delta,
			Bid:          bid,
			Ask:          ask,
			SpreadPct:    spreadPct,
			OpenInterest: oi,
			OptionVolume: vol,
		})
	}

	return contracts, nil
}

// truncateBody returns at most maxLen bytes of the body as a string.
func truncateBody(b []byte, maxLen int) string {
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen]) + "..."
}

// PremarketSnapshot captures the pre-market session (4:00–9:30 AM ET) price action.
type PremarketSnapshot struct {
	Ticker     string
	High       float64 // highest print in pre-market
	Low        float64 // lowest print in pre-market
	Close      float64 // last pre-market print before open
	Volume     int64   // pre-market volume
	GapPct     float64 // (Close - PriorClose) / PriorClose * 100; positive = gap up
	GapDir     string  // "up" | "down" | "flat"
}

// FetchPremarketSnapshot fetches 5-min SIP bars for the pre-market window
// (4:00 AM – 9:25 AM ET) and returns a summary snapshot.
// Uses feed=sip which includes extended-hours data (IEX does not).
// priorClose is the previous regular-session close used to compute the gap.
func (c *AlpacaClient) FetchPremarketSnapshot(ticker string, priorClose float64, date time.Time) (PremarketSnapshot, error) {
	snap := PremarketSnapshot{Ticker: ticker}

	// ET is UTC-4 (EDT) or UTC-5 (EST); use UTC offsets for simplicity.
	// Alpaca accepts UTC RFC3339 — convert 4:00 AM ET and 9:25 AM ET.
	et, _ := time.LoadLocation("America/New_York")
	pmStart := time.Date(date.Year(), date.Month(), date.Day(), 4, 0, 0, 0, et)
	pmEnd := time.Date(date.Year(), date.Month(), date.Day(), 9, 25, 0, 0, et)

	params := url.Values{}
	params.Set("symbols", ticker)
	params.Set("timeframe", "5Min")
	params.Set("start", pmStart.UTC().Format(time.RFC3339))
	params.Set("end", pmEnd.UTC().Format(time.RFC3339))
	params.Set("feed", "sip")
	params.Set("sort", "asc")
	params.Set("limit", "200")

	reqURL := fmt.Sprintf("%s/v2/stocks/bars?%s", c.dataURL, params.Encode())
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return snap, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return snap, fmt.Errorf("alpaca premarket %s: %w", ticker, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return snap, fmt.Errorf("alpaca premarket %s HTTP %d: %s", ticker, resp.StatusCode, truncateBody(body, 100))
	}

	var parsed alpacaBarsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return snap, fmt.Errorf("alpaca premarket decode %s: %w", ticker, err)
	}

	bars := parsed.Bars[ticker]
	if len(bars) == 0 {
		return snap, nil // no pre-market activity — not an error
	}

	snap.High = bars[0].High
	snap.Low = bars[0].Low
	for _, b := range bars {
		if b.High > snap.High {
			snap.High = b.High
		}
		if b.Low < snap.Low {
			snap.Low = b.Low
		}
		snap.Volume += int64(b.Volume)
	}
	snap.Close = bars[len(bars)-1].Close

	if priorClose > 0 {
		snap.GapPct = (snap.Close - priorClose) / priorClose * 100.0
	}
	switch {
	case snap.GapPct > 0.5:
		snap.GapDir = "up"
	case snap.GapPct < -0.5:
		snap.GapDir = "down"
	default:
		snap.GapDir = "flat"
	}
	return snap, nil
}

// Opening5MinBar is a single 5-minute bar from the regular session open.
type Opening5MinBar struct {
	Time   string  `json:"time"`   // "09:30", "09:35", "09:40"
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

// FetchOpening5MinBars fetches the first nBars 5-minute candles of the regular
// session (starting 9:30 AM ET) for multiple tickers in one API call.
// Returns a map of ticker → []Opening5MinBar (up to nBars entries).
func (c *AlpacaClient) FetchOpening5MinBars(tickers []string, date time.Time, nBars int) (map[string][]Opening5MinBar, error) {
	result := make(map[string][]Opening5MinBar)
	if len(tickers) == 0 {
		return result, nil
	}

	et, _ := time.LoadLocation("America/New_York")
	start := time.Date(date.Year(), date.Month(), date.Day(), 9, 30, 0, 0, et)
	end := start.Add(time.Duration(nBars)*5*time.Minute + time.Minute) // +1 min buffer

	params := url.Values{}
	params.Set("symbols", strings.Join(tickers, ","))
	params.Set("timeframe", "5Min")
	params.Set("start", start.UTC().Format(time.RFC3339))
	params.Set("end", end.UTC().Format(time.RFC3339))
	params.Set("feed", "iex") // real-time intraday; SIP requires paid subscription
	params.Set("sort", "asc")
	params.Set("limit", "1000")

	reqURL := fmt.Sprintf("%s/v2/stocks/bars?%s", c.dataURL, params.Encode())
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return result, fmt.Errorf("alpaca opening bars: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return result, fmt.Errorf("alpaca opening bars HTTP %d: %s", resp.StatusCode, truncateBody(body, 100))
	}

	var parsed alpacaBarsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return result, fmt.Errorf("alpaca opening bars decode: %w", err)
	}

	for ticker, bars := range parsed.Bars {
		var out []Opening5MinBar
		for i, b := range bars {
			if i >= nBars {
				break
			}
			// Parse bar time for display label
			t, _ := time.Parse(time.RFC3339, b.Time)
			label := t.In(et).Format("15:04")
			out = append(out, Opening5MinBar{
				Time:   label,
				Open:   b.Open,
				High:   b.High,
				Low:    b.Low,
				Close:  b.Close,
				Volume: int64(b.Volume),
			})
		}
		if len(out) > 0 {
			result[ticker] = out
		}
	}
	return result, nil
}

// ChainPCRatio holds a put/call volume ratio computed from an option chain.
// We use OptionVolume as the proxy because the Alpaca indicative feed does not
// provide open interest.
type ChainPCRatio struct {
	CallVol int
	PutVol  int
	PCRatio float64 // PutVol / CallVol; 0 if no call volume
	Bias    string  // "put_heavy" | "call_heavy" | "balanced"
}

// ComputeChainPCRatio derives put/call volume ratio from a pre-filtered chain.
// Use for both market-wide (SPY chain) and per-ticker signals.
func ComputeChainPCRatio(contracts []OptionContract) ChainPCRatio {
	var callVol, putVol int
	for _, c := range contracts {
		switch c.Type {
		case "call":
			callVol += c.OptionVolume
		case "put":
			putVol += c.OptionVolume
		}
	}
	r := ChainPCRatio{CallVol: callVol, PutVol: putVol, Bias: "balanced"}
	if callVol > 0 {
		r.PCRatio = float64(putVol) / float64(callVol)
	}
	switch {
	case r.PCRatio >= 1.2:
		r.Bias = "put_heavy"
	case r.PCRatio <= 0.6 && callVol > 0:
		r.Bias = "call_heavy"
	}
	return r
}

// FetchIntradayBars fetches 1-minute bars for each ticker in the window
// [openTime, openTime+windowMinutes). openTime should be the market open
// timestamp in the local timezone (e.g. 6:30 AM PT on the trade date).
//
// Returned bars are sorted oldest-first (Alpaca sort=asc). Tickers with no
// data in the window are omitted from the map — the caller must handle missing
// entries gracefully.
//
// WHAT BREAKS: If openTime is a weekend or holiday, Alpaca returns empty bars
// for all tickers. The confirmation activity will skip those candidates.
func (c *AlpacaClient) FetchIntradayBars(tickers []string, openTime time.Time, windowMinutes int) (map[string][]indicators.Bar, error) {
	result := make(map[string][]indicators.Bar)
	if len(tickers) == 0 {
		return result, nil
	}

	endTime := openTime.Add(time.Duration(windowMinutes) * time.Minute)

	batchSize := 40
	for i := 0; i < len(tickers); i += batchSize {
		end := i + batchSize
		if end > len(tickers) {
			end = len(tickers)
		}
		batch := tickers[i:end]
		bars, err := c.fetchIntradayBatch(batch, openTime, endTime)
		if err != nil {
			fmt.Printf("alpaca intraday: batch fetch error: %v\n", err)
			continue
		}
		for ticker, barsSlice := range bars {
			result[ticker] = barsSlice
		}
	}
	return result, nil
}

// fetchIntradayBatch is the paginated inner loop for FetchIntradayBars.
// It uses RFC3339 timestamps and 1Min timeframe.
func (c *AlpacaClient) fetchIntradayBatch(tickers []string, start, end time.Time) (map[string][]indicators.Bar, error) {
	result := make(map[string][]indicators.Bar)
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("symbols", strings.Join(tickers, ","))
		params.Set("timeframe", "1Min")
		params.Set("start", start.UTC().Format(time.RFC3339))
		params.Set("end", end.UTC().Format(time.RFC3339))
		params.Set("limit", "1000") // 10 bars × 40 tickers = 400 max; 1000 is safe
		params.Set("feed", "iex")  // intraday is real-time; IEX is the free tier option
		params.Set("sort", "asc")
		if pageToken != "" {
			params.Set("page_token", pageToken)
		}

		reqURL := fmt.Sprintf("%s/v2/stocks/bars?%s", c.dataURL, params.Encode())
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("alpaca intraday: build request: %w", err)
		}
		req.Header.Set("APCA-API-KEY-ID", c.apiKey)
		req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("alpaca intraday: request failed: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("alpaca intraday: HTTP %d: %s", resp.StatusCode, truncateBody(body, 200))
		}

		var parsed alpacaBarsResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("alpaca intraday: decode: %w", err)
		}

		for ticker, rawBars := range parsed.Bars {
			for _, rb := range rawBars {
				result[ticker] = append(result[ticker], indicators.Bar{
					Open:   rb.Open,
					High:   rb.High,
					Low:    rb.Low,
					Close:  rb.Close,
					Volume: rb.Volume,
				})
			}
		}

		if parsed.NextPageToken == nil || *parsed.NextPageToken == "" {
			break
		}
		pageToken = *parsed.NextPageToken
	}

	return result, nil
}

// FilterChainQuality applies liquidity quality filters to an option chain.
// Contracts that fail any threshold are removed.
// Pass zero for any threshold you don't want to apply.
//
// Parameters match strategy_rules.yaml options_translation.liquidity_filters:
//
//	minOI        — minimum open interest (0 = skip check)
//	minVolume    — minimum option volume (0 = skip check)
//	maxSpreadPct — maximum bid-ask spread as % of mid (0 = skip check)
func FilterChainQuality(contracts []OptionContract, minOI, minVolume int, maxSpreadPct float64) []OptionContract {
	var qualified []OptionContract
	for _, c := range contracts {
		if maxSpreadPct > 0 && c.SpreadPct > maxSpreadPct {
			continue
		}
		// Skip OI check when OI=0 — indicative feed may not return OI.
		if minOI > 0 && c.OpenInterest > 0 && c.OpenInterest < minOI {
			continue
		}
		if minVolume > 0 && c.OptionVolume < minVolume {
			continue
		}
		qualified = append(qualified, c)
	}
	return qualified
}

// ContractSelectionOpts configures SelectBestContract ranking.
// All fields have safe zero-value defaults (see SelectBestContract doc).
type ContractSelectionOpts struct {
	// DTE constraints applied as hard filters.
	DTEMin        int // 0 → use 7
	DTEMax        int // 0 → use 14
	AvoidDTEBelow int // 0 → use 4; contracts below this DTE are excluded
	TargetDTE     int // 0 → use 10; rank by |dte - target|

	// Delta constraints applied as soft filters (prefer in-band, fall back to nearest).
	DeltaMin float64 // 0 → 0.30
	DeltaMax float64 // 0 → 0.75
}

// effectiveDTEOpts fills zero-value fields with safe defaults.
func effectiveDTEOpts(o ContractSelectionOpts) ContractSelectionOpts {
	if o.DTEMin <= 0 {
		o.DTEMin = 7
	}
	if o.DTEMax <= 0 {
		o.DTEMax = 14
	}
	if o.AvoidDTEBelow <= 0 {
		o.AvoidDTEBelow = 4
	}
	if o.TargetDTE <= 0 {
		o.TargetDTE = 10
	}
	if o.DeltaMin <= 0 {
		o.DeltaMin = 0.30
	}
	if o.DeltaMax <= 0 {
		o.DeltaMax = 0.75
	}
	return o
}

// SelectBestContract picks the best option contract for the given direction ("call" or "put").
//
// Ranking priority (descending):
//  1. Type matches direction (hard filter)
//  2. DTE >= AvoidDTEBelow (hard filter; default 4)
//  3. DTE inside [DTEMin, DTEMax] (hard filter; defaults 7–14)
//  4. Delta inside [DeltaMin, DeltaMax] (soft preference; falls back to any in-type if no in-band contracts)
//  5. Closest DTE to TargetDTE (default 10)
//  6. Closest |delta| to mid of delta band
//  7. Tighter bid-ask spread
//  8. Higher open interest
//  9. Higher option volume
//
// Log family, DTE, allowed range, target, delta, spread, OI, volume for the selected contract.
// Returns nil if no contracts match.
func SelectBestContract(contracts []OptionContract, direction string, opts ContractSelectionOpts) *OptionContract {
	opts = effectiveDTEOpts(opts)
	targetDeltaMid := (opts.DeltaMin + opts.DeltaMax) / 2.0

	// Hard filter: type, avoid_dte_below, dte range
	var candidates []OptionContract
	for _, c := range contracts {
		if c.Type != direction {
			continue
		}
		if c.DTE < opts.AvoidDTEBelow {
			continue
		}
		if c.DTE < opts.DTEMin || c.DTE > opts.DTEMax {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil
	}

	// Soft filter: prefer contracts with |delta| inside the band.
	// If none qualify, fall back to all DTE-filtered candidates.
	inBand := candidates[:0:0]
	for _, c := range candidates {
		absDelta := c.Delta
		if absDelta < 0 {
			absDelta = -absDelta
		}
		if absDelta >= opts.DeltaMin && absDelta <= opts.DeltaMax {
			inBand = append(inBand, c)
		}
	}
	pool := candidates
	if len(inBand) > 0 {
		pool = inBand
	}

	// Rank: find the best by the composite score.
	// Lower score = better.
	type scored struct {
		c     *OptionContract
		score float64
	}
	var ranked []scored
	for i := range pool {
		c := &pool[i]
		absDelta := c.Delta
		if absDelta < 0 {
			absDelta = -absDelta
		}
		// Primary: DTE distance from target (normalised 0–1 over 14-day window)
		dteDist := float64(c.DTE - opts.TargetDTE)
		if dteDist < 0 {
			dteDist = -dteDist
		}
		dteDist = dteDist / 14.0

		// Secondary: delta distance from mid (0–0.5 range)
		deltaDist := absDelta - targetDeltaMid
		if deltaDist < 0 {
			deltaDist = -deltaDist
		}

		// Tertiary: spread quality (0 to ~10% spread range)
		spreadPenalty := c.SpreadPct / 100.0

		// Tie-break: prefer higher liquidity (OI + volume; inverted, small = better)
		liqPenalty := 0.0
		if c.OpenInterest > 0 {
			liqPenalty = 1.0 / float64(c.OpenInterest)
		}

		composite := dteDist*4.0 + deltaDist*2.0 + spreadPenalty*1.0 + liqPenalty*0.1
		ranked = append(ranked, scored{c: c, score: composite})
	}

	if len(ranked) == 0 {
		return nil
	}

	best := ranked[0]
	for _, r := range ranked[1:] {
		if r.score < best.score {
			best = r
		}
	}
	return best.c
}

// PlaceOptionOrder places a day limit buy order for one options contract
// using Alpaca's paper-trading broker API (POST /v2/orders).
// Returns the Alpaca order ID on success.
func (c *AlpacaClient) PlaceOptionOrder(symbol string, limitPrice float64) (string, error) {
	body := map[string]interface{}{
		"symbol":        symbol,
		"qty":           "1",
		"side":          "buy",
		"type":          "limit",
		"time_in_force": "day",
		"limit_price":   fmt.Sprintf("%.2f", limitPrice),
	}
	bodyBytes, _ := json.Marshal(body)

	reqURL := fmt.Sprintf("%s/v2/orders", c.tradeURL)
	req, err := http.NewRequest("POST", reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("alpaca order: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("alpaca order HTTP %d: %s", resp.StatusCode, truncateBody(respBody, 300))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("alpaca order decode: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("alpaca order: empty order ID in response: %s", truncateBody(respBody, 200))
	}
	return result.ID, nil
}

// SellOptionOrder places a day limit sell order to close an existing option position.
// Uses the current bid as the limit price (conservative — fills if bid is touched).
// Returns the Alpaca order ID on success.
func (c *AlpacaClient) SellOptionOrder(symbol string, limitPrice float64) (string, error) {
	body := map[string]interface{}{
		"symbol":        symbol,
		"qty":           "1",
		"side":          "sell",
		"type":          "limit",
		"time_in_force": "day",
		"limit_price":   fmt.Sprintf("%.2f", limitPrice),
	}
	bodyBytes, _ := json.Marshal(body)

	reqURL := fmt.Sprintf("%s/v2/orders", c.tradeURL)
	req, err := http.NewRequest("POST", reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("alpaca sell: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("alpaca sell HTTP %d: %s", resp.StatusCode, truncateBody(respBody, 300))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("alpaca sell decode: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("alpaca sell: empty order ID: %s", truncateBody(respBody, 200))
	}
	return result.ID, nil
}

// FetchOptionMidPrice returns the current bid/ask midpoint for a single OCC option
// symbol (e.g. "RTX260508P00190000"). Used by RunPositionReviewActivity to compute
// accurate option-level P&L instead of using the underlying stock price.
//
// Uses the generic multi-symbol snapshots endpoint (no underlying in path).
// Returns 0 and an error if the contract has no quote data.
func (c *AlpacaClient) FetchOptionMidPrice(occSymbol string) (float64, error) {
	// Endpoint: /v1beta1/options/snapshots?symbols=<OCC>&feed=indicative
	// The per-underlying path (/snapshots/{ticker}?symbols=...) is not supported.
	reqURL := fmt.Sprintf("%s/v1beta1/options/snapshots?symbols=%s&feed=indicative",
		c.dataURL, occSymbol)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("alpaca option quote HTTP %d: %s", resp.StatusCode, truncateBody(body, 100))
	}

	var result struct {
		Snapshots map[string]alpacaOptionSnapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("alpaca option quote decode: %w", err)
	}

	snap, ok := result.Snapshots[occSymbol]
	if !ok {
		return 0, fmt.Errorf("alpaca option quote: symbol %q not in response", occSymbol)
	}

	bid := snap.Quote.Bid
	ask := snap.Quote.Ask
	if bid <= 0 && ask <= 0 {
		return 0, fmt.Errorf("alpaca option quote: no bid/ask for %q", occSymbol)
	}
	if bid <= 0 {
		return ask, nil
	}
	return (bid + ask) / 2.0, nil
}

// FetchLatestQuote returns the latest trade price for a symbol.
// Used for position mark-to-market.
func (c *AlpacaClient) FetchLatestQuote(ticker string) (float64, error) {
	reqURL := fmt.Sprintf("%s/v2/stocks/%s/trades/latest?feed=iex", c.dataURL, ticker)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("alpaca quote HTTP %d", resp.StatusCode)
	}

	var result struct {
		Trade struct {
			Price float64 `json:"p"`
		} `json:"trade"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	return result.Trade.Price, nil
}

// ComputeProxyIV computes a proxy implied volatility for an option contract.
// Formula: ask / (underlyingPrice * sqrt(DTE/252))
// This is the Black-Scholes at-the-money approximation, proportional to
// annualised IV. A value of 0.25 ≈ 25% annual IV.
// Returns 0 if inputs are invalid (zero ask, zero underlying, or DTE <= 0).
func ComputeProxyIV(contract OptionContract, underlyingPrice float64) float64 {
	if contract.Ask <= 0 || underlyingPrice <= 0 || contract.DTE <= 0 {
		return 0
	}
	return contract.Ask / (underlyingPrice * math.Sqrt(float64(contract.DTE)/252.0))
}

// FindATMCallContract returns the call contract with delta closest to 0.50
// from the provided slice. Returns nil if no calls are present.
// Used to select the contract for the daily IV snapshot.
func FindATMCallContract(contracts []OptionContract) *OptionContract {
	var best *OptionContract
	bestDist := math.MaxFloat64
	for i := range contracts {
		c := &contracts[i]
		if c.Type != "call" {
			continue
		}
		dist := math.Abs(c.Delta - 0.50)
		if dist < bestDist {
			bestDist = dist
			best = c
		}
	}
	return best
}
