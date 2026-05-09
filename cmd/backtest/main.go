// cmd/backtest/main.go
//
// Backtests the RSVE-O binary gate strategy from 2023-01-01 to today.
// Fetches real historical OHLCV data from Alpaca.
// Replays EvaluateRSVE day by day — same gates as live.
// Simulates entry/exit against actual subsequent price bars.
//
// No historical option chain data available, so P&L is tracked two ways:
//  1. Underlying % move (exact)
//  2. Estimated option P&L using 2x leverage (delta ~0.5, 10 DTE ATM)
//
// Run: go run ./cmd/backtest/

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// ── Config ────────────────────────────────────────────────────────────────────

const (
	btStart       = "2023-01-01"
	optionLeverage = 2.0 // delta ~0.5 ATM: underlying 1% ≈ option 2%
	defaultHoldDays = 10 // 10 DTE target matches live strategy

	// Trailing stop mirrors strategy_rules.yaml mechanical exits.
	// trailing_start_pct=35% option ≈ ~17% underlying at delta 0.5.
	// We use 4%/2% underlying-level thresholds (same as existing backtest).
	trailArmPct      = 4.0
	trailGivebackPct = 2.0
)

var watchlist = []string{
	"AAPL", "COIN", "NFLX", "HOOD", "CRWV", "GOOGL", "MSFT", "TSLA", "META", "AMZN",
	"HD", "AMD", "SPY", "TQQQ", "SMCI", "SNOW", "BA", "PANW", "CRWD", "LMT", "RTX",
	"XOM", "OXY", "LLY", "FCX", "NVDA",
}

// ── Data types ────────────────────────────────────────────────────────────────

type datedBar struct {
	Date string
	indicators.Bar
}

type vixPoint struct {
	Date  string
	Value float64
}

type signal struct {
	Date       string
	Ticker     string
	Direction  string
	Score      float64
	RejectGate string // empty when AllPass=true
	EntryPrice float64
	StopLoss   float64
	Target1    float64
	Target2    float64
	HoldDays   int
	VIX        float64
	BarIdx     int
}

type outcome struct {
	signal
	OutcomeDays  int
	OutcomeType  string // "target1" "target2" "trailing_stop" "stop" "timeout"
	ExitPrice    float64
	UnderlyingPct float64
	EstOptionPct  float64
	Win           bool
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	_ = godotenv.Load("/Users/harsh/Documents/make-my-trade/.env")

	apiKey := os.Getenv("ALPACA_PAPER_API_KEY")
	secretKey := os.Getenv("ALPACA_PAPER_SECRET_KEY")
	dataURL := os.Getenv("ALPACA_DATA_URL")
	fredKey := os.Getenv("FRED_API_KEY")

	if apiKey == "" || secretKey == "" {
		fmt.Fprintln(os.Stderr, "ERROR: ALPACA_PAPER_API_KEY / ALPACA_PAPER_SECRET_KEY not set")
		os.Exit(1)
	}
	if dataURL == "" {
		dataURL = "https://data.alpaca.markets"
	}

	btEnd := time.Now().Format("2006-01-02")
	start, _ := time.Parse("2006-01-02", btStart)
	end, _ := time.Parse("2006-01-02", btEnd)

	fmt.Printf("=== RSVE-O BACKTEST: %s → %s ===\n\n", btStart, btEnd)

	// ── Step 1: Fetch bars ─────────────────────────────────────────────────────

	allTickers := unique(append(watchlist, "SPY"))
	fmt.Printf("Fetching historical bars for %d tickers...\n", len(allTickers))
	tickerBars, err := fetchBarsWithDates(apiKey, secretKey, dataURL, allTickers, start, end)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR fetching bars: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  SPY bars: %d\n", len(tickerBars["SPY"]))

	// ── Step 2: Fetch VIX ─────────────────────────────────────────────────────

	vixMap := make(map[string]float64)
	if fredKey != "" {
		fmt.Println("Fetching VIX from FRED...")
		vixData, vixErr := fetchFREDVIX(fredKey, btStart)
		if vixErr != nil {
			fmt.Printf("  WARN: VIX fetch failed (%v) — using rolling vol proxy\n", vixErr)
		} else {
			for _, v := range vixData {
				vixMap[v.Date] = v.Value
			}
			fmt.Printf("  VIX data points: %d\n", len(vixMap))
		}
	}

	// ── Step 3: Build trading calendar from SPY ────────────────────────────────

	spyBars := tickerBars["SPY"]
	const minBarsWarmup = 130
	if len(spyBars) < minBarsWarmup+10 {
		fmt.Fprintf(os.Stderr, "ERROR: only %d SPY bars — not enough history\n", len(spyBars))
		os.Exit(1)
	}

	// ── Step 4: Load RSVE config ───────────────────────────────────────────────

	rules, rulesErr := strategy.LoadRules("strategy_rules.yaml")
	if rulesErr != nil {
		fmt.Printf("WARN: could not load strategy_rules.yaml (%v) — using defaults\n", rulesErr)
		rules = strategy.DefaultRules()
	}
	rsveConfig := rules.RSVE

	// Build date → bar index per ticker
	tickerDateIdx := make(map[string]map[string]int, len(allTickers))
	for ticker, bars := range tickerBars {
		m := make(map[string]int, len(bars))
		for i, b := range bars {
			m[b.Date] = i
		}
		tickerDateIdx[ticker] = m
	}

	// ── Step 5: Replay RSVE day by day ────────────────────────────────────────

	fmt.Println("Replaying RSVE gates day by day...")
	var signals []signal

	for spyIdx := minBarsWarmup; spyIdx < len(spyBars); spyIdx++ {
		date := spyBars[spyIdx].Date
		spyIndicatorBars := toIndicatorBars(spyBars[:spyIdx+1])
		vix := lookupVIX(vixMap, spyBars, spyIdx)

		for _, ticker := range watchlist {
			bars, ok := tickerBars[ticker]
			if !ok {
				continue
			}
			barIdx, ok2 := tickerDateIdx[ticker][date]
			if !ok2 || barIdx < minBarsWarmup {
				continue
			}

			tickerIndicatorBars := toIndicatorBars(bars[:barIdx+1])

			rsveResult := strategy.EvaluateRSVE(strategy.RSVEInput{
				Ticker:           ticker,
				Date:             date,
				Bars:             tickerIndicatorBars,
				SPYBars:          spyIndicatorBars,
				VIX:              vix,
				EarningsDaysAway: -1, // unknown in backtest — gate passes
				IVRank:           -1,
				BidAskSpreadPct:  -1,
				OpenInterest:     -1,
			}, rsveConfig)

			if !rsveResult.AllPass {
				continue
			}

			entry := bars[barIdx].Close
			atr := atrLast(tickerIndicatorBars, 14)

			var stop, t1, t2 float64
			if rsveResult.Direction == "bullish" {
				stop = entry - 1.5*atr
				t1 = entry + 2.5*atr
				t2 = entry + 4.0*atr
			} else {
				stop = entry + 1.5*atr
				t1 = entry - 2.5*atr
				t2 = entry - 4.0*atr
			}

			signals = append(signals, signal{
				Date:       date,
				Ticker:     ticker,
				Direction:  rsveResult.Direction,
				Score:      rsveResult.Score,
				EntryPrice: entry,
				StopLoss:   stop,
				Target1:    t1,
				Target2:    t2,
				HoldDays:   defaultHoldDays,
				VIX:        vix,
				BarIdx:     barIdx,
			})
		}
	}

	fmt.Printf("  Total RSVE signals: %d\n\n", len(signals))

	// ── Step 6: Simulate outcomes ─────────────────────────────────────────────

	fmt.Println("Simulating trade outcomes...")
	var outcomes []outcome
	for _, sig := range signals {
		outcomes = append(outcomes, simulateOutcome(sig, tickerBars[sig.Ticker]))
	}

	// ── Step 7: Print results ─────────────────────────────────────────────────

	printResults(outcomes, btStart, btEnd)
}

// ── Outcome simulation ────────────────────────────────────────────────────────

func simulateOutcome(sig signal, bars []datedBar) outcome {
	o := outcome{signal: sig}
	maxIdx := len(bars) - 1
	startIdx := sig.BarIdx + 1

	trailingArmed := false
	trailingPeak := 0.0

	for dayOffset := 1; dayOffset <= sig.HoldDays && startIdx+dayOffset-1 <= maxIdx; dayOffset++ {
		b := bars[startIdx+dayOffset-1]

		if sig.Direction == "bullish" {
			if !trailingArmed {
				if (b.High-sig.EntryPrice)/sig.EntryPrice*100 >= trailArmPct {
					trailingArmed = true
					trailingPeak = b.High
				}
			} else if b.High > trailingPeak {
				trailingPeak = b.High
			}

			if sig.Target2 > sig.Target1 && b.High >= sig.Target2 {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "target2", sig.Target2, true
				break
			}
			if sig.Target1 > sig.EntryPrice && b.High >= sig.Target1 {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "target1", sig.Target1, true
				break
			}
			if trailingArmed && b.Low <= trailingPeak*(1-trailGivebackPct/100) {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "trailing_stop", trailingPeak*(1-trailGivebackPct/100), true
				break
			}
			if sig.StopLoss > 0 && b.Low <= sig.StopLoss {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "stop", sig.StopLoss, false
				break
			}
			if dayOffset == sig.HoldDays {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice = dayOffset, "timeout", b.Close
				o.Win = b.Close > sig.EntryPrice
			}

		} else { // bearish
			if !trailingArmed {
				if (sig.EntryPrice-b.Low)/sig.EntryPrice*100 >= trailArmPct {
					trailingArmed = true
					trailingPeak = b.Low
				}
			} else if b.Low < trailingPeak {
				trailingPeak = b.Low
			}

			if sig.Target2 < sig.Target1 && b.Low <= sig.Target2 {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "target2", sig.Target2, true
				break
			}
			if sig.Target1 < sig.EntryPrice && b.Low <= sig.Target1 {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "target1", sig.Target1, true
				break
			}
			if trailingArmed && b.High >= trailingPeak*(1+trailGivebackPct/100) {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "trailing_stop", trailingPeak*(1+trailGivebackPct/100), true
				break
			}
			if sig.StopLoss > 0 && b.High >= sig.StopLoss {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice, o.Win = dayOffset, "stop", sig.StopLoss, false
				break
			}
			if dayOffset == sig.HoldDays {
				o.OutcomeDays, o.OutcomeType, o.ExitPrice = dayOffset, "timeout", b.Close
				o.Win = b.Close < sig.EntryPrice
			}
		}
	}

	if o.ExitPrice == 0 || o.OutcomeDays == 0 {
		if startIdx <= maxIdx {
			o.ExitPrice = bars[maxIdx].Close
			o.OutcomeDays = maxIdx - sig.BarIdx
			o.OutcomeType = "timeout"
			o.Win = (sig.Direction == "bullish" && o.ExitPrice > sig.EntryPrice) ||
				(sig.Direction == "bearish" && o.ExitPrice < sig.EntryPrice)
		} else {
			o.OutcomeType = "no_data"
			return o
		}
	}

	if sig.Direction == "bullish" {
		o.UnderlyingPct = (o.ExitPrice - sig.EntryPrice) / sig.EntryPrice * 100
	} else {
		o.UnderlyingPct = (sig.EntryPrice - o.ExitPrice) / sig.EntryPrice * 100
	}

	if o.Win {
		o.EstOptionPct = math.Min(o.UnderlyingPct*optionLeverage, 80)
	} else {
		o.EstOptionPct = math.Max(o.UnderlyingPct*optionLeverage, -10)
	}

	return o
}

// ── Result printing ───────────────────────────────────────────────────────────

func printResults(outcomes []outcome, start, end string) {
	var valid []outcome
	for _, o := range outcomes {
		if o.OutcomeType != "no_data" {
			valid = append(valid, o)
		}
	}
	if len(valid) == 0 {
		fmt.Println("No outcomes to report.")
		return
	}

	fmt.Printf("\n╔══════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║      RSVE-O BACKTEST  %s → %s          ║\n", start, end)
	fmt.Printf("╚══════════════════════════════════════════════════════════════╝\n\n")

	printGroupStats("OVERALL", valid)

	// By RSVE score bucket (all-pass signals ranked 0-100)
	fmt.Println("\n── By RSVE Score Bucket ────────────────────────────────────────")
	scoreBuckets := map[string][]outcome{
		"score 0-39  (low conviction)":  {},
		"score 40-59 (moderate)":        {},
		"score 60-74 (strong)":          {},
		"score 75+   (high conviction)": {},
	}
	for _, o := range valid {
		switch {
		case o.Score < 40:
			scoreBuckets["score 0-39  (low conviction)"] = append(scoreBuckets["score 0-39  (low conviction)"], o)
		case o.Score < 60:
			scoreBuckets["score 40-59 (moderate)"] = append(scoreBuckets["score 40-59 (moderate)"], o)
		case o.Score < 75:
			scoreBuckets["score 60-74 (strong)"] = append(scoreBuckets["score 60-74 (strong)"], o)
		default:
			scoreBuckets["score 75+   (high conviction)"] = append(scoreBuckets["score 75+   (high conviction)"], o)
		}
	}
	for _, label := range []string{
		"score 0-39  (low conviction)",
		"score 40-59 (moderate)",
		"score 60-74 (strong)",
		"score 75+   (high conviction)",
	} {
		printGroupStats(label, scoreBuckets[label])
	}

	// By direction
	fmt.Println("\n── By Direction ────────────────────────────────────────────────")
	dirGroups := groupBy(valid, func(o outcome) string { return o.Direction })
	for _, d := range []string{"bullish", "bearish"} {
		if g, ok := dirGroups[d]; ok {
			printGroupStats(d, g)
		}
	}

	// By year
	fmt.Println("\n── By Year ─────────────────────────────────────────────────────")
	yearGroups := groupBy(valid, func(o outcome) string {
		if len(o.Date) >= 4 {
			return o.Date[:4]
		}
		return "unknown"
	})
	for _, yr := range []string{"2023", "2024", "2025", "2026"} {
		if g, ok := yearGroups[yr]; ok {
			printGroupStats(yr, g)
		}
	}

	// By VIX regime
	fmt.Println("\n── By VIX Regime ───────────────────────────────────────────────")
	vixGroups := map[string][]outcome{
		"VIX <15  (calm)":      {},
		"VIX 15-20 (normal)":   {},
		"VIX 20-24 (elevated)": {},
		"VIX >=24 (blocked)":   {},
	}
	for _, o := range valid {
		switch {
		case o.VIX < 15:
			vixGroups["VIX <15  (calm)"] = append(vixGroups["VIX <15  (calm)"], o)
		case o.VIX < 20:
			vixGroups["VIX 15-20 (normal)"] = append(vixGroups["VIX 15-20 (normal)"], o)
		case o.VIX < 24:
			vixGroups["VIX 20-24 (elevated)"] = append(vixGroups["VIX 20-24 (elevated)"], o)
		default:
			vixGroups["VIX >=24 (blocked)"] = append(vixGroups["VIX >=24 (blocked)"], o)
		}
	}
	for _, k := range []string{"VIX <15  (calm)", "VIX 15-20 (normal)", "VIX 20-24 (elevated)", "VIX >=24 (blocked)"} {
		printGroupStats(k, vixGroups[k])
	}

	// By ticker
	fmt.Println("\n── By Ticker (sorted by expectancy) ────────────────────────────")
	tickerGroups := groupBy(valid, func(o outcome) string { return o.Ticker })
	type tickerStat struct {
		ticker     string
		count      int
		winRate    float64
		expectancy float64
	}
	var tstats []tickerStat
	for ticker, group := range tickerGroups {
		wins, total := 0, 0.0
		for _, o := range group {
			if o.Win {
				wins++
			}
			total += o.EstOptionPct
		}
		tstats = append(tstats, tickerStat{
			ticker:     ticker,
			count:      len(group),
			winRate:    float64(wins) / float64(len(group)) * 100,
			expectancy: total / float64(len(group)),
		})
	}
	sort.Slice(tstats, func(i, j int) bool { return tstats[i].expectancy > tstats[j].expectancy })
	fmt.Printf("  %-8s %5s %8s %11s\n", "Ticker", "Count", "WinRate", "Expectancy")
	fmt.Printf("  %s\n", strings.Repeat("-", 38))
	for _, ts := range tstats {
		fmt.Printf("  %-8s %5d %7.1f%% %+10.1f%%\n", ts.ticker, ts.count, ts.winRate, ts.expectancy)
	}

	// Outcome type distribution
	fmt.Println("\n── Outcome Type Distribution ───────────────────────────────────")
	outGroups := groupBy(valid, func(o outcome) string { return o.OutcomeType })
	total := float64(len(valid))
	for _, k := range []string{"target2", "target1", "trailing_stop", "stop", "timeout"} {
		if g, ok := outGroups[k]; ok {
			wins := 0
			for _, o := range g {
				if o.Win {
					wins++
				}
			}
			fmt.Printf("  %-14s %4d (%4.1f%%)  wins:%d\n",
				k, len(g), float64(len(g))/total*100, wins)
		}
	}

	// Sample high-conviction trades
	fmt.Println("\n── Sample Trades (score ≥ 75) ──────────────────────────────────")
	printed := 0
	for _, o := range valid {
		if o.Score >= 75 && printed < 20 {
			dir := "▲"
			if o.Direction == "bearish" {
				dir = "▼"
			}
			win := "✓"
			if !o.Win {
				win = "✗"
			}
			fmt.Printf("  %s %s %-5s %s  score:%-5.0f exit:%-14s underlying:%+.1f%%  option:~%+.1f%%\n",
				o.Date, win, o.Ticker, dir, o.Score, o.OutcomeType,
				o.UnderlyingPct, o.EstOptionPct)
			printed++
		}
	}

	fmt.Printf("\n── Notes ────────────────────────────────────────────────────────\n")
	fmt.Println("  • Gates: all 13 RSVE-O binary gates must pass to generate a signal")
	fmt.Println("  • Score: 0-100 ranking only — not a gate. Higher = stronger setup")
	fmt.Println("  • Underlying % = actual stock move from close to exit")
	fmt.Println("  • Option % = estimated 2x leverage (delta 0.5 ATM, 10 DTE)")
	fmt.Println("  • Losses capped at -10% (hard stop rule)")
	fmt.Println("  • Targets: ATR-based (2.5x ATR for T1, 4.0x ATR for T2)")
	fmt.Println("  • Stop: 1.5x ATR from entry")
	fmt.Println("  • Earnings blackout not applied (no historical earnings calendar)")
	fmt.Println("  • IV rank / spread / OI gates skipped (no historical chain data)")
}

func printGroupStats(label string, outcomes []outcome) {
	if len(outcomes) == 0 {
		fmt.Printf("  %-36s  no data\n", label)
		return
	}
	wins, losses := 0, 0
	totalWinPct, totalLossPct, totalOptionPct := 0.0, 0.0, 0.0
	for _, o := range outcomes {
		if o.Win {
			wins++
			totalWinPct += o.EstOptionPct
		} else {
			losses++
			totalLossPct += o.EstOptionPct
		}
		totalOptionPct += o.EstOptionPct
	}
	n := float64(len(outcomes))
	avgWin, avgLoss := 0.0, 0.0
	if wins > 0 {
		avgWin = totalWinPct / float64(wins)
	}
	if losses > 0 {
		avgLoss = totalLossPct / float64(losses)
	}
	fmt.Printf("  %-36s  n=%-5d  win=%-5.1f%%  avgW=%+6.1f%%  avgL=%+6.1f%%  expectancy=%+6.1f%%\n",
		label, len(outcomes), float64(wins)/n*100, avgWin, avgLoss, totalOptionPct/n)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func groupBy(outcomes []outcome, key func(outcome) string) map[string][]outcome {
	m := make(map[string][]outcome)
	for _, o := range outcomes {
		m[key(o)] = append(m[key(o)], o)
	}
	return m
}

func toIndicatorBars(dbs []datedBar) []indicators.Bar {
	out := make([]indicators.Bar, len(dbs))
	for i, db := range dbs {
		out[i] = db.Bar
	}
	return out
}

func atrLast(bars []indicators.Bar, period int) float64 {
	if len(bars) < 2 {
		return bars[len(bars)-1].Close * 0.015
	}
	var trs []float64
	for i := 1; i < len(bars); i++ {
		hl := bars[i].High - bars[i].Low
		hc := math.Abs(bars[i].High - bars[i-1].Close)
		lc := math.Abs(bars[i].Low - bars[i-1].Close)
		trs = append(trs, math.Max(hl, math.Max(hc, lc)))
	}
	if len(trs) < period {
		return bars[len(bars)-1].Close * 0.015
	}
	recent := trs[len(trs)-period:]
	sum := 0.0
	for _, v := range recent {
		sum += v
	}
	return sum / float64(period)
}

// lookupVIX returns VIX for a date; walks back up to 5 days for weekends/holidays;
// falls back to realized vol on SPY if FRED key unavailable.
func lookupVIX(vixMap map[string]float64, spyBars []datedBar, spyIdx int) float64 {
	date := spyBars[spyIdx].Date
	if len(vixMap) > 0 {
		d, _ := time.Parse("2006-01-02", date)
		for i := 0; i < 6; i++ {
			if v, ok := vixMap[d.AddDate(0, 0, -i).Format("2006-01-02")]; ok && v > 0 {
				return v
			}
		}
	}
	if spyIdx < 21 {
		return 18.0
	}
	var returns [20]float64
	for i := 0; i < 20; i++ {
		prev := spyBars[spyIdx-20+i].Close
		curr := spyBars[spyIdx-20+i+1].Close
		if prev > 0 {
			returns[i] = math.Log(curr / prev)
		}
	}
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= 20
	variance := 0.0
	for _, r := range returns {
		d := r - mean
		variance += d * d
	}
	return math.Sqrt(variance/20) * math.Sqrt(252) * 100
}

func unique(s []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ── Alpaca fetch ──────────────────────────────────────────────────────────────

type alpacaBarRaw struct {
	Time   string  `json:"t"`
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume float64 `json:"v"`
}

type alpacaBarsResp struct {
	Bars          map[string][]alpacaBarRaw `json:"bars"`
	NextPageToken *string                   `json:"next_page_token"`
}

func fetchBarsWithDates(apiKey, secretKey, dataURL string, tickers []string, start, end time.Time) (map[string][]datedBar, error) {
	result := make(map[string][]datedBar)
	const batchSize = 20
	for i := 0; i < len(tickers); i += batchSize {
		e := i + batchSize
		if e > len(tickers) {
			e = len(tickers)
		}
		batch := tickers[i:e]
		fmt.Printf("  Fetching batch %d/%d: %v\n", i/batchSize+1, (len(tickers)+batchSize-1)/batchSize, batch)
		bars, err := fetchBatch(apiKey, secretKey, dataURL, batch, start, end)
		if err != nil {
			return nil, err
		}
		for ticker, sl := range bars {
			result[ticker] = sl
		}
	}
	return result, nil
}

func fetchBatch(apiKey, secretKey, dataURL string, tickers []string, start, end time.Time) (map[string][]datedBar, error) {
	result := make(map[string][]datedBar)
	pageToken := ""
	client := &http.Client{Timeout: 30 * time.Second}

	for {
		params := url.Values{}
		params.Set("symbols", strings.Join(tickers, ","))
		params.Set("timeframe", "1Day")
		params.Set("start", start.Format("2006-01-02"))
		params.Set("end", end.Format("2006-01-02"))
		params.Set("limit", "10000")
		params.Set("feed", "sip")
		params.Set("adjustment", "split")
		params.Set("sort", "asc")
		if pageToken != "" {
			params.Set("page_token", pageToken)
		}

		reqURL := fmt.Sprintf("%s/v2/stocks/bars?%s", strings.TrimRight(dataURL, "/"), params.Encode())
		req, _ := http.NewRequest("GET", reqURL, nil)
		req.Header.Set("APCA-API-KEY-ID", apiKey)
		req.Header.Set("APCA-API-SECRET-KEY", secretKey)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("alpaca request: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			// Retry with IEX feed if SIP subscription is blocked
			if strings.Contains(string(body), "subscription does not permit") {
				params.Set("feed", "iex")
				req2, _ := http.NewRequest("GET", fmt.Sprintf("%s/v2/stocks/bars?%s", strings.TrimRight(dataURL, "/"), params.Encode()), nil)
				req2.Header.Set("APCA-API-KEY-ID", apiKey)
				req2.Header.Set("APCA-API-SECRET-KEY", secretKey)
				resp2, err2 := client.Do(req2)
				if err2 != nil {
					return nil, fmt.Errorf("alpaca iex fallback: %w", err2)
				}
				body, _ = io.ReadAll(resp2.Body)
				resp2.Body.Close()
				if resp2.StatusCode != 200 {
					return nil, fmt.Errorf("alpaca HTTP %d: %s", resp2.StatusCode, string(body))
				}
			} else {
				return nil, fmt.Errorf("alpaca HTTP %d: %s", resp.StatusCode, string(body))
			}
		}

		var parsed alpacaBarsResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("alpaca decode: %w", err)
		}
		for ticker, rawBars := range parsed.Bars {
			for _, rb := range rawBars {
				dateStr := rb.Time
				if len(dateStr) > 10 {
					dateStr = dateStr[:10]
				}
				result[ticker] = append(result[ticker], datedBar{
					Date: dateStr,
					Bar:  indicators.Bar{Open: rb.Open, High: rb.High, Low: rb.Low, Close: rb.Close, Volume: rb.Volume},
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

// ── FRED VIX fetch ────────────────────────────────────────────────────────────

type fredResp struct {
	Observations []struct {
		Date  string `json:"date"`
		Value string `json:"value"`
	} `json:"observations"`
}

func fetchFREDVIX(apiKey, startDate string) ([]vixPoint, error) {
	params := url.Values{}
	params.Set("series_id", "VIXCLS")
	params.Set("observation_start", startDate)
	params.Set("api_key", apiKey)
	params.Set("file_type", "json")

	resp, err := http.Get("https://api.stlouisfed.org/fred/series/observations?" + params.Encode())
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var fr fredResp
	if err := json.Unmarshal(body, &fr); err != nil {
		return nil, err
	}
	var out []vixPoint
	for _, obs := range fr.Observations {
		if obs.Value == "." || obs.Value == "" {
			continue
		}
		var v float64
		fmt.Sscanf(obs.Value, "%f", &v)
		if v > 0 {
			out = append(out, vixPoint{Date: obs.Date, Value: v})
		}
	}
	return out, nil
}
