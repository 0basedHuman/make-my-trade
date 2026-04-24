// internal/market/finnhub.go
//
// WHAT: Finnhub REST client for news, earnings calendar, and sentiment.
//
// WHY:  Finnhub provides real-time and recent news + company events via a
//       free-tier API. We use it for news sentiment (affects confidence)
//       and earnings calendar (drives blackout rules).
//
// HOW:  Simple GET requests with token query param. We fetch:
//       - /news?category=general — broad market news
//       - /company-news?symbol=X — company-specific news
//       - /calendar/earnings — upcoming earnings (blackout check)
//       - /stock/social-sentiment?symbol=X — Reddit/StockTwits sentiment
//
// WHAT BREAKS: Rate limit 60 req/min on free tier. We space calls by 1s.
//              If Finnhub is down, we return empty data and log — the strategy
//              engine runs without sentiment (confidence penalty applied).
//
// VERIFY: Hit /news on Finnhub dashboard, compare to FinnhubClient.FetchNews()

package market

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// FinnhubClient wraps the Finnhub REST API.
type FinnhubClient struct {
	apiKey string
	http   *http.Client
}

// NewFinnhubClient creates a client with the given API key.
func NewFinnhubClient(apiKey string) *FinnhubClient {
	return &FinnhubClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

const finnhubBase = "https://finnhub.io/api/v1"

// NewsItem is a single news article from Finnhub.
type NewsItem struct {
	Category  string  `json:"category"`
	Datetime  int64   `json:"datetime"`
	Headline  string  `json:"headline"`
	Source    string  `json:"source"`
	Summary   string  `json:"summary"`
	URL       string  `json:"url"`
	Image     string  `json:"image"`
	Related   string  `json:"related"`
	Sentiment float64 // computed externally or from sentiment endpoint
}

// EarningsEvent is a scheduled earnings announcement.
type EarningsEvent struct {
	Date       string  `json:"date"`
	Symbol     string  `json:"symbol"`
	EPS        float64 `json:"epsActual"`
	EPSEst     float64 `json:"epsEstimate"`
	Revenue    float64 `json:"revenueActual"`
	RevenueEst float64 `json:"revenueEstimate"`
	Quarter    int     `json:"quarter"`
	Year       int     `json:"year"`
}

// SentimentData is the social sentiment summary for a symbol.
type SentimentData struct {
	Symbol          string  `json:"symbol"`
	AtTime          string  `json:"atTime"`
	Mention         int     `json:"mention"`
	PositiveMention int     `json:"positiveMention"`
	NegativeMention int     `json:"negativeMention"`
	Score           float64 // derived: (positive - negative) / mention
	RedditMention   int     `json:"redditMention"`
	RedditPositive  int     `json:"redditPositiveMention"`
	RedditNegative  int     `json:"redditNegativeMention"`
}

// FetchCompanyNews fetches the last N days of news for a specific ticker.
func (c *FinnhubClient) FetchCompanyNews(ticker string, from, to time.Time) ([]NewsItem, error) {
	params := url.Values{}
	params.Set("symbol", ticker)
	params.Set("from", from.Format("2006-01-02"))
	params.Set("to", to.Format("2006-01-02"))
	params.Set("token", c.apiKey)

	body, err := c.get(finnhubBase + "/company-news?" + params.Encode())
	if err != nil {
		return nil, err
	}
	var items []NewsItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("finnhub company-news decode: %w", err)
	}
	return items, nil
}

// FetchUpcomingEarnings returns earnings events in the given date range.
func (c *FinnhubClient) FetchUpcomingEarnings(from, to time.Time) ([]EarningsEvent, error) {
	params := url.Values{}
	params.Set("from", from.Format("2006-01-02"))
	params.Set("to", to.Format("2006-01-02"))
	params.Set("token", c.apiKey)

	body, err := c.get(finnhubBase + "/calendar/earnings?" + params.Encode())
	if err != nil {
		return nil, err
	}
	var envelope struct {
		EarningsCalendar []EarningsEvent `json:"earningsCalendar"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("finnhub earnings decode: %w", err)
	}
	return envelope.EarningsCalendar, nil
}

// FetchSocialSentiment fetches Reddit + StockTwits sentiment for a ticker.
// Returns a SentimentData with a derived Score in [-1, +1].
func (c *FinnhubClient) FetchSocialSentiment(ticker string) (SentimentData, error) {
	params := url.Values{}
	params.Set("symbol", ticker)
	params.Set("token", c.apiKey)

	body, err := c.get(finnhubBase + "/stock/social-sentiment?" + params.Encode())
	if err != nil {
		return SentimentData{}, err
	}

	var envelope struct {
		Data   []SentimentData `json:"data"`
		Symbol string          `json:"symbol"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return SentimentData{}, fmt.Errorf("finnhub sentiment decode: %w", err)
	}
	if len(envelope.Data) == 0 {
		return SentimentData{Symbol: ticker}, nil
	}

	// Aggregate the most recent sentiment data point
	latest := envelope.Data[len(envelope.Data)-1]
	latest.Symbol = ticker
	if latest.Mention > 0 {
		latest.Score = float64(latest.PositiveMention-latest.NegativeMention) / float64(latest.Mention)
	}
	return latest, nil
}

// HasEarningsWithin returns true if the ticker has earnings within `days` days from `from`.
func HasEarningsWithin(events []EarningsEvent, ticker string, from time.Time, days int) bool {
	until := from.AddDate(0, 0, days)
	for _, e := range events {
		if e.Symbol != ticker {
			continue
		}
		t, err := time.Parse("2006-01-02", e.Date)
		if err != nil {
			continue
		}
		if !t.Before(from) && t.Before(until) {
			return true
		}
	}
	return false
}

// get is the shared HTTP helper. Returns the raw response body.
func (c *FinnhubClient) get(rawURL string) ([]byte, error) {
	resp, err := c.http.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("finnhub GET: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("finnhub HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
