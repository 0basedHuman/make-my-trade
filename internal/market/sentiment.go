// internal/market/sentiment.go
//
// WHAT: Contextual news sentiment for a ticker.
//
// WHY:  Sentiment provides soft context around a trade — e.g., a bullish setup
//       with strong negative earnings sentiment is worth flagging in diagnostics.
//       It is NOT a gate. It does NOT open or block trades. Ever.
//
// HOW:  Returns a SentimentContext struct for diagnostic logging.
//       This is a placeholder returning "unavailable" until a FinBERT-ready
//       news API (e.g., Finnhub sentiment, NewsAPI + VADER, or on-device FinBERT)
//       is integrated. The struct is designed to be wire-compatible with that future work.
//
// INVARIANT:
//   Sentiment NEVER appears in a gate condition.
//   Sentiment NEVER determines confirmed/watch_only status.
//   Sentiment appears only in log output and diagnostic structs.

package market

import "time"

// SentimentSignal is the coarse-grained sentiment bucket.
type SentimentSignal string

const (
	SentimentPositive    SentimentSignal = "positive"
	SentimentNeutral     SentimentSignal = "neutral"
	SentimentNegative    SentimentSignal = "negative"
	SentimentUnavailable SentimentSignal = "unavailable"
)

// SentimentContext is contextual sentiment data for one ticker.
// It is ALWAYS diagnostic — never a gate condition.
type SentimentContext struct {
	Ticker    string
	Signal    SentimentSignal
	Source    string    // "news", "earnings", "unavailable"
	Note      string    // brief human-readable summary ≤80 chars
	FetchedAt time.Time
}

// FetchSentimentContext returns contextual sentiment for the given ticker.
// Currently returns SentimentUnavailable until a news sentiment API is configured.
// Callers must log this but MUST NOT use it in any gate or confirmation decision.
func FetchSentimentContext(ticker string) SentimentContext {
	return SentimentContext{
		Ticker:    ticker,
		Signal:    SentimentUnavailable,
		Source:    "unavailable",
		Note:      "news sentiment API not configured; FinBERT-ready struct",
		FetchedAt: time.Now(),
	}
}
