// cmd/dryscan/main.go
//
// WHAT: Dry-run RSVE-O scanner — evaluates all watchlist tickers using the
//       binary gate strategy and prints a gate diagnostic table.
//       No database writes, no paper orders, no Claude calls.
//
// WHY:  Fast iteration loop. Run this to see which tickers pass all 13 RSVE-O
//       gates today, why others were rejected, and which gate blocked them.
//       Useful for validating gate thresholds without touching the live pipeline.
//
// HOW:  1. Load strategy_rules.yaml for gate thresholds.
//       2. Fetch 130+ bars of daily OHLCV from Alpaca for each ticker + SPY.
//       3. Fetch VIX from FRED.
//       4. Run EvaluateRSVE for each ticker.
//       5. Print a compact table: ticker | direction | status | reject gate | score.
//       6. For tickers that fail, optionally print full gate diagnostics (-verbose).
//
// Run:
//   go run ./cmd/dryscan/                     # compact table
//   go run ./cmd/dryscan/ -verbose            # full gate table for each ticker
//   go run ./cmd/dryscan/ -ticker AAPL        # single ticker
//   go run ./cmd/dryscan/ -rules /path/to/strategy_rules.yaml

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/yourname/makemytrade/internal/indicators"
	"github.com/yourname/makemytrade/internal/strategy"
)

// ── Watchlist ─────────────────────────────────────────────────────────────────

var defaultWatchlist = []string{
	"AAPL", "COIN", "NFLX", "HOOD", "GOOGL", "MSFT", "TSLA", "META", "AMZN",
	"HD", "AMD", "SPY", "SMCI", "SNOW", "PANW", "CRWD", "LLY", "NVDA",
}

// ── Config ────────────────────────────────────────────────────────────────────

const (
	minBarsNeeded  = 130 // EMA50 needs 50, BBWidthPct needs 20+63, EMA100 needs 100+buffer
	defaultDataURL = "https://data.alpaca.markets"
)

// ── CLI flags ─────────────────────────────────────────────────────────────────

var (
	flagVerbose = flag.Bool("verbose", false, "print full gate table for every ticker")
	flagTicker  = flag.String("ticker", "", "scan a single ticker only")
	flagRules   = flag.String("rules", "strategy_rules.yaml", "path to strategy_rules.yaml")
	flagDate    = flag.String("date", "", "scan date YYYY-MM-DD (default: today)")
)

// ── Bar type (matches Alpaca JSON) ────────────────────────────────────────────

type alpacaBar struct {
	T  string  `json:"t"`
	O  float64 `json:"o"`
	H  float64 `json:"h"`
	L  float64 `json:"l"`
	C  float64 `json:"c"`
	V  float64 `json:"v"`
}

type alpacaBarsResp struct {
	Bars          map[string][]alpacaBar `json:"bars"`
	NextPageToken string                 `json:"next_page_token"`
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()
	_ = godotenv.Load()

	apiKey := os.Getenv("ALPACA_PAPER_API_KEY")
	secretKey := os.Getenv("ALPACA_PAPER_SECRET_KEY")
	dataURL := os.Getenv("ALPACA_DATA_URL")
	fredKey := os.Getenv("FRED_API_KEY")

	if apiKey == "" || secretKey == "" {
		fmt.Fprintln(os.Stderr, "ERROR: ALPACA_PAPER_API_KEY / ALPACA_PAPER_SECRET_KEY not set")
		os.Exit(1)
	}
	if dataURL == "" {
		dataURL = defaultDataURL
	}

	// Date range: 200 calendar days back to get minBarsNeeded trading days
	scanDate := time.Now()
	if *flagDate != "" {
		var err error
		scanDate, err = time.Parse("2006-01-02", *flagDate)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: invalid date %q: %v\n", *flagDate, err)
			os.Exit(1)
		}
	}
	endDate := scanDate.Format("2006-01-02")
	startDate := scanDate.AddDate(0, 0, -200).Format("2006-01-02")

	// Load RSVE config
	rules, err := strategy.LoadRules(*flagRules)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: could not load %s (%v) — using defaults\n", *flagRules, err)
		rules = strategy.DefaultRules()
	}
	rsveConfig := rules.RSVE

	// Build ticker list
	tickers := defaultWatchlist
	if *flagTicker != "" {
		tickers = []string{strings.ToUpper(*flagTicker)}
	}
	// SPY always needed for relative strength
	allTickers := unique(append(tickers, "SPY"))

	fmt.Printf("=== RSVE-O DRY SCAN: %s ===\n\n", endDate)
	fmt.Printf("Fetching %d tickers (%s → %s)...\n", len(allTickers), startDate, endDate)

	// Fetch bars
	allBars, err := fetchBatch(dataURL, apiKey, secretKey, allTickers, startDate, endDate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: fetch bars: %v\n", err)
		os.Exit(1)
	}

	// Fetch VIX
	vix := fetchVIX(fredKey, scanDate)
	fmt.Printf("VIX: %.2f\n\n", vix)

	spyBars := toIndicatorBars(allBars["SPY"])

	// Evaluate each ticker
	type tickerResult struct {
		ticker string
		result strategy.RSVEResult
	}
	var results []tickerResult
	var skipped []string

	for _, ticker := range tickers {
		raw, ok := allBars[ticker]
		if !ok || len(raw) < minBarsNeeded {
			skipped = append(skipped, fmt.Sprintf("%s (bars=%d)", ticker, len(raw)))
			continue
		}

		bars := toIndicatorBars(raw)
		input := strategy.RSVEInput{
			Ticker:           ticker,
			Date:             endDate,
			Bars:             bars,
			SPYBars:          spyBars,
			VIX:              vix,
			EarningsDaysAway: -1, // unknown in dry scan
			IVRank:           -1, // unavailable — gates will pass
			BidAskSpreadPct:  -1,
			OpenInterest:     -1,
		}
		r := strategy.EvaluateRSVE(input, rsveConfig)
		results = append(results, tickerResult{ticker: ticker, result: r})
	}

	// Sort: confirmed first, then by score desc, then alphabetical
	sort.Slice(results, func(i, j int) bool {
		ri, rj := results[i].result, results[j].result
		if ri.AllPass != rj.AllPass {
			return ri.AllPass
		}
		if ri.Score != rj.Score {
			return ri.Score > rj.Score
		}
		return results[i].ticker < results[j].ticker
	})

	// Print summary table
	fmt.Printf("%-8s  %-8s  %-20s  %-7s  %s\n",
		"TICKER", "DIR", "STATUS", "SCORE", "REJECT GATE")
	fmt.Println(strings.Repeat("─", 72))

	var confirmed []strategy.RSVEResult
	for _, tr := range results {
		r := tr.result
		scoreStr := "-"
		if r.AllPass {
			scoreStr = fmt.Sprintf("%.0f", r.Score)
			confirmed = append(confirmed, r)
		}
		rejectGate := r.RejectGate
		if r.AllPass {
			rejectGate = "✓ all passed"
		}
		fmt.Printf("%-8s  %-8s  %-20s  %-7s  %s\n",
			tr.ticker, r.Direction, r.Status, scoreStr, rejectGate)
	}

	if len(skipped) > 0 {
		fmt.Printf("\nSkipped (insufficient bars): %s\n", strings.Join(skipped, ", "))
	}

	// Verbose: full gate table for each ticker
	if *flagVerbose {
		fmt.Println()
		for _, tr := range results {
			printGateTable(tr.ticker, tr.result)
		}
	} else if len(confirmed) == 0 {
		fmt.Println("\nNo confirmed setups today.")
	} else {
		fmt.Printf("\n%d confirmed setup(s). Run with -verbose to see all gate details.\n", len(confirmed))
	}
}

// printGateTable prints the full 13-gate diagnostic table for one ticker.
func printGateTable(ticker string, r strategy.RSVEResult) {
	status := "REJECTED"
	if r.AllPass {
		status = fmt.Sprintf("CONFIRMED (score=%.0f)", r.Score)
	}
	fmt.Printf("── %s [%s / %s] ──\n", ticker, r.Direction, status)
	fmt.Printf("  %-24s  %-6s  %-30s  %-30s\n", "GATE", "PASS", "ACTUAL", "THRESHOLD")
	fmt.Println("  " + strings.Repeat("─", 94))
	for _, g := range r.Gates {
		passStr := "✓"
		if !g.Passed {
			passStr = "✗"
			if g.Blocking {
				passStr = "✗ BLOCK"
			}
		}
		fmt.Printf("  %-24s  %-6s  %-30s  %s\n",
			g.Name, passStr, truncate(g.ActualValue, 30), truncate(g.Threshold, 30))
	}
	fmt.Println()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// ── Alpaca data fetcher ───────────────────────────────────────────────────────

func fetchBatch(dataURL, apiKey, secretKey string, tickers []string, start, end string) (map[string][]alpacaBar, error) {
	result := make(map[string][]alpacaBar)

	// Chunk into batches of 10 to avoid URL length limits
	const chunkSize = 10
	for i := 0; i < len(tickers); i += chunkSize {
		end_ := i + chunkSize
		if end_ > len(tickers) {
			end_ = len(tickers)
		}
		chunk := tickers[i:end_]

		bars, err := fetchChunk(dataURL, apiKey, secretKey, chunk, start, end)
		if err != nil {
			// try IEX fallback
			bars, err = fetchChunkWithFeed(dataURL, apiKey, secretKey, chunk, start, end, "iex")
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARN: fetch chunk %v: %v\n", chunk, err)
				continue
			}
		}
		for k, v := range bars {
			result[k] = v
		}
	}
	return result, nil
}

func fetchChunk(dataURL, apiKey, secretKey string, tickers []string, start, end string) (map[string][]alpacaBar, error) {
	return fetchChunkWithFeed(dataURL, apiKey, secretKey, tickers, start, end, "sip")
}

func fetchChunkWithFeed(dataURL, apiKey, secretKey string, tickers []string, start, end, feed string) (map[string][]alpacaBar, error) {
	result := make(map[string][]alpacaBar)
	pageToken := ""

	for {
		params := url.Values{
			"symbols":    {strings.Join(tickers, ",")},
			"timeframe":  {"1Day"},
			"start":      {start},
			"end":        {end},
			"limit":      {"1000"},
			"feed":       {feed},
			"adjustment": {"split"},
		}
		if pageToken != "" {
			params.Set("page_token", pageToken)
		}

		reqURL := dataURL + "/v2/stocks/bars?" + params.Encode()
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("APCA-API-KEY-ID", apiKey)
		req.Header.Set("APCA-API-SECRET-KEY", secretKey)

		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return nil, err
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			msg := string(body)
			if strings.Contains(msg, "subscription does not permit querying recent SIP data") && feed == "sip" {
				return nil, fmt.Errorf("sip_unavailable: %s", msg)
			}
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
		}

		var parsed alpacaBarsResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
		for sym, bars := range parsed.Bars {
			result[sym] = append(result[sym], bars...)
		}

		if parsed.NextPageToken == "" {
			break
		}
		pageToken = parsed.NextPageToken
	}
	return result, nil
}

// ── VIX fetch (FRED) ──────────────────────────────────────────────────────────

type fredResp struct {
	Observations []struct {
		Date  string `json:"date"`
		Value string `json:"value"`
	} `json:"observations"`
}

func fetchVIX(fredKey string, asOf time.Time) float64 {
	if fredKey == "" {
		fmt.Fprintln(os.Stderr, "WARN: FRED_API_KEY not set — VIX defaulting to 15.0")
		return 15.0
	}

	// Look back 10 calendar days to get the most recent trading day VIX.
	start := asOf.AddDate(0, 0, -10).Format("2006-01-02")
	end := asOf.Format("2006-01-02")

	params := url.Values{
		"series_id":        {"VIXCLS"},
		"observation_start": {start},
		"observation_end":   {end},
		"api_key":          {fredKey},
		"file_type":        {"json"},
		"sort_order":       {"desc"},
		"limit":            {"5"},
	}
	resp, err := http.Get("https://api.stlouisfed.org/fred/series/observations?" + params.Encode())
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: VIX fetch error: %v — defaulting to 15.0\n", err)
		return 15.0
	}
	defer resp.Body.Close()

	var data fredResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || len(data.Observations) == 0 {
		fmt.Fprintf(os.Stderr, "WARN: VIX parse error — defaulting to 15.0\n")
		return 15.0
	}
	for _, obs := range data.Observations {
		if obs.Value == "." {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(obs.Value, "%f", &v); err == nil {
			return v
		}
	}
	return 15.0
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func toIndicatorBars(raw []alpacaBar) []indicators.Bar {
	out := make([]indicators.Bar, len(raw))
	for i, b := range raw {
		out[i] = indicators.Bar{
			Open:   b.O,
			High:   b.H,
			Low:    b.L,
			Close:  b.C,
			Volume: b.V,
		}
	}
	return out
}

func unique(ss []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
