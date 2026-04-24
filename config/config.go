// Package config loads all environment variables at startup and exposes them
// as a typed Config struct. Every other package receives *Config as a
// parameter — nothing else in the codebase calls os.Getenv() directly.
//
// Inputs:  environment variables (loaded from .env by godotenv)
// Outputs: *Config struct consumed by cmd/server, cmd/worker, and all
//          internal packages that need credentials or settings
//
// What calls this: cmd/server/main.go and cmd/worker/main.go, once each
// at process startup before any other initialisation.
//
// What breaks if wrong: missing required variable → immediate panic with
// a clear message naming the missing key. Wrong value (e.g. non-numeric
// PORT) → panic with a conversion error. Both are intentional — fail
// loudly at startup rather than silently mid-run.

package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config holds every runtime setting MakeMyTrade needs.
// Fields are grouped by concern to mirror the .env file structure.
// All values are typed — callers never parse strings themselves.
type Config struct {

	// ── Environment ──────────────────────────────────────────────────────────
	Env string // "development" or "production"

	// ── HTTP server ──────────────────────────────────────────────────────────
	Port int // listens on this port; default 8080

	// ── Database ─────────────────────────────────────────────────────────────
	// Full Postgres connection string including credentials and DB name.
	// Used by pgx/v5 to open the connection pool.
	DBURL string

	// ── Redis ─────────────────────────────────────────────────────────────────
	// Connection URL for signal cache and pub/sub notifications.
	RedisURL string

	// ── Temporal ─────────────────────────────────────────────────────────────
	TemporalHost      string // gRPC address, e.g. "localhost:7233"
	TemporalNamespace string // "default" for local development

	// ── Market data — Alpaca ─────────────────────────────────────────────────
	// Paper keys are used for all order execution in v1.
	// Live keys are stored but gated behind LiveTradeEnabled.
	AlpacaAPIKey      string
	AlpacaSecretKey   string
	AlpacaPaperAPIKey string
	AlpacaPaperSecret string
	AlpacaBaseURL     string // paper or live base URL
	AlpacaDataURL     string // market data endpoint (same for both)

	// ── News and sentiment — Finnhub ─────────────────────────────────────────
	// Provides news, earnings calendar, analyst ratings, social sentiment.
	FinnhubAPIKey string

	// ── Macro data — FRED ────────────────────────────────────────────────────
	// Federal Reserve Economic Data: Fed funds rate, CPI, VIX history.
	FREDAPIKey string

	// ── Claude API ───────────────────────────────────────────────────────────
	AnthropicAPIKey        string
	ClaudeInputTokenBudget int // max input tokens per research packet
	ClaudeMaxOutputTokens  int // max tokens Claude may return per call

	// ── Trading mode ─────────────────────────────────────────────────────────
	// PaperTradeEnabled: paper orders execute automatically, always true.
	// LiveTradeEnabled:  real money orders; must be explicitly enabled.
	// These are independent — both can be true simultaneously.
	PaperTradeEnabled bool
	LiveTradeEnabled  bool
}

// Load reads .env from the working directory, then reads all required
// environment variables into a Config struct.
//
// What can go wrong: .env file missing (non-fatal warning, continues),
// required env var missing (fatal panic), numeric var non-parseable (fatal panic).
// Callers should treat the returned *Config as read-only.
func Load() *Config {
	// godotenv.Load() is non-fatal if .env is missing — the process may
	// already have environment variables injected (e.g. in CI or production).
	// We log a warning so the absence is visible but not a crash.
	if err := godotenv.Load(); err != nil {
		fmt.Println("config: .env file not found, reading from environment directly")
	}

	return &Config{
		Env:  getenvOr("ENV", "development"),
		Port: mustGetInt("PORT"),

		DBURL:    mustGetenv("DB_URL"),
		RedisURL: mustGetenv("REDIS_URL"),

		TemporalHost:      mustGetenv("TEMPORAL_HOST"),
		TemporalNamespace: getenvOr("TEMPORAL_NAMESPACE", "default"),

		AlpacaAPIKey:      mustGetenv("ALPACA_API_KEY"),
		AlpacaSecretKey:   mustGetenv("ALPACA_SECRET_KEY"),
		AlpacaPaperAPIKey: mustGetenv("ALPACA_PAPER_API_KEY"),
		AlpacaPaperSecret: mustGetenv("ALPACA_PAPER_SECRET_KEY"),
		AlpacaBaseURL:     mustGetenv("ALPACA_BASE_URL"),
		AlpacaDataURL:     mustGetenv("ALPACA_DATA_URL"),

		FinnhubAPIKey: mustGetenv("FINNHUB_API_KEY"),
		FREDAPIKey:    mustGetenv("FRED_API_KEY"),

		AnthropicAPIKey:        mustGetenv("ANTHROPIC_API_KEY"),
		ClaudeInputTokenBudget: mustGetInt("CLAUDE_INPUT_TOKEN_BUDGET"),
		ClaudeMaxOutputTokens:  mustGetInt("CLAUDE_MAX_OUTPUT_TOKENS"),

		PaperTradeEnabled: mustGetBool("PAPER_TRADE_ENABLED"),
		LiveTradeEnabled:  mustGetBool("LIVE_TRADE_ENABLED"),
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// mustGetenv returns the value of the named environment variable.
// Panics with a clear message if the variable is missing or empty.
// Use this for every variable that is required for the app to function.
func mustGetenv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("config: required environment variable %q is not set", key))
	}
	return v
}

// getenvOr returns the value of the named variable, or fallback if missing.
// Use this for optional variables that have sensible defaults.
func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// mustGetInt parses the named variable as an integer.
// Panics if missing, empty, or not a valid integer.
func mustGetInt(key string) int {
	v := mustGetenv(key)
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("config: %q must be an integer, got %q", key, v))
	}
	return n
}

// mustGetBool parses the named variable as a boolean.
// Accepts "true"/"false" (case-insensitive).
// Panics if missing, empty, or not a valid boolean.
func mustGetBool(key string) bool {
	v := mustGetenv(key)
	b, err := strconv.ParseBool(v)
	if err != nil {
		panic(fmt.Sprintf("config: %q must be true or false, got %q", key, v))
	}
	return b
}
