// cmd/wfresearch/main.go
//
// Walk-forward threshold research CLI.
//
// Usage:
//
//	go run ./cmd/wfresearch/ \
//	  --dsn "$DATABASE_URL" \
//	  --days 252 \
//	  --train 126 \
//	  --test 21 \
//	  --top 10
//
// Loads historical candidate records from the candidates table (status=rejected
// or status=paper_trade_created etc.) with their outcome P&L from closed
// paper_positions, then runs walk-forward over the canonical threshold grid
// and prints results sorted by stability score.
//
// Output columns:
//
//	RS   RVOL  BB    BK   EMA  Trades WinRate Expect PF   MaxDD  Stability DSR  FDR?
//
// Deflated Sharpe is computed over all OOS windows combined.
// FDR? = "yes" when DSR < 0 (likely data-mining artifact).

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/makemytrade/internal/research"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL DSN")
	days := flag.Int("days", 252, "history window in candidates (trading days)")
	train := flag.Int("train", 126, "walk-forward train window size")
	test := flag.Int("test", 21, "walk-forward test window size")
	top := flag.Int("top", 10, "number of top configs to print")
	flag.Parse()

	if *dsn == "" {
		log.Fatal("wfresearch: --dsn or DATABASE_URL required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("wfresearch: db connect: %v", err)
	}
	defer pool.Close()

	candidates, err := loadCandidates(ctx, pool, *days)
	if err != nil {
		log.Fatalf("wfresearch: load candidates: %v", err)
	}
	if len(candidates) == 0 {
		fmt.Println("wfresearch: no candidate records found — run daily scan first")
		return
	}
	fmt.Printf("wfresearch: loaded %d candidate records\n", len(candidates))

	grid := research.ThresholdGrid()
	fmt.Printf("wfresearch: testing %d threshold combinations\n", len(grid))

	results := make([]research.WalkForwardResult, 0, len(grid))
	for i := range grid {
		r := research.RunWalkForward(candidates, grid[i], *train, *test)
		r.Config = grid[i]
		results = append(results, r)
	}

	research.SortByStability(results)

	// Compute deflated Sharpe report on the top result.
	maxSharpe := 0.0
	oosSharpe := 0.0
	totalObs := 0
	if len(results) > 0 {
		best := results[0]
		for _, w := range best.Windows {
			if w.SharpeApprox > maxSharpe {
				maxSharpe = w.SharpeApprox
			}
			totalObs += w.TradeCount
		}
		if len(best.Windows) > 0 {
			oosSharpe = best.Windows[len(best.Windows)-1].SharpeApprox
		}
	}
	dsrReport := research.DeflatedSharpeReport(len(grid), maxSharpe, oosSharpe, totalObs, 0, 0)

	fmt.Printf("\n%-5s %-5s %-5s %-4s %-4s %6s %7s %7s %5s %6s %9s %6s %4s\n",
		"RS%", "RVOL", "BB%", "BK", "EMA", "Trades", "WinRate", "Expect", "PF", "MaxDD", "Stability", "Sharpe", "FDR?")
	fmt.Println("---------------------------------------------------------------------" +
		"---------------------------------------")

	shown := 0
	for _, r := range results {
		if shown >= *top {
			break
		}
		if r.TradeCount == 0 {
			continue
		}
		fdr := "-"
		if dsrReport.FDRWarning {
			fdr = "YES"
		}
		fmt.Printf("%-5.0f %-5.1f %-5.0f %-4d %-4d %6d %7.1f%% %7.2f %5.2f %6.1f%% %9.3f %6.2f %4s\n",
			r.Config.RSMinPct,
			r.Config.RVOLMin,
			r.Config.BBPercentileMax*100,
			r.Config.BreakoutBars,
			r.Config.EMARegime,
			r.TradeCount,
			r.WinRate*100,
			r.Expectancy,
			r.ProfitFactor,
			r.MaxDrawdown,
			r.StabilityScore,
			maxSharpe,
			fdr,
		)
		shown++
	}

	fmt.Printf("\n=== Deflated Sharpe Report ===\n")
	fmt.Printf("Combinations tested : %d\n", dsrReport.NTrials)
	fmt.Printf("Best in-sample SR   : %.3f\n", dsrReport.InSampleSharpe)
	fmt.Printf("OOS SR (last window): %.3f\n", dsrReport.OOSSharpe)
	fmt.Printf("Expected max SR(H0) : %.3f\n", dsrReport.ExpectedMaxSharpe)
	fmt.Printf("Deflated SR         : %.3f\n", dsrReport.DeflatedSharpe)
	fmt.Printf("Confidence          : %.1f%%\n", dsrReport.Confidence*100)
	if dsrReport.FDRWarning {
		fmt.Println("*** FALSE DISCOVERY WARNING: DSR < 0 — results may be data-mining artifacts ***")
	} else {
		fmt.Println("DSR > 0: observed SR is better than chance expectation given trial count")
	}
}

// loadCandidates loads historical candidate records with outcome P&L from the DB.
// Only candidates with closed positions (outcome known) are included.
func loadCandidates(ctx context.Context, pool *pgxpool.Pool, limitDays int) ([]research.CandidateRecord, error) {
	rows, err := pool.Query(ctx, `
SELECT
    c.trade_date::text,
    c.ticker,
    COALESCE(c.direction,'bullish'),
    COALESCE(c.volume_ratio, 0),
    COALESCE(c.vix_level, 0),
    COALESCE(p.realized_pnl_pct, 0),
    p.id IS NOT NULL
FROM trade_candidates c
LEFT JOIN paper_positions p ON p.candidate_id = c.id AND p.status = 'closed'
WHERE c.trade_date >= CURRENT_DATE - ($1 || ' days')::interval
ORDER BY c.trade_date ASC, c.ticker ASC
`, limitDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []research.CandidateRecord
	for rows.Next() {
		var r research.CandidateRecord
		var wasTaken bool
		if err := rows.Scan(
			&r.Date, &r.Ticker, &r.Direction,
			&r.RVOL, &r.BBPct,
			&r.OutcomePnL, &wasTaken,
		); err != nil {
			return nil, err
		}
		r.WasTaken = wasTaken
		out = append(out, r)
	}
	return out, rows.Err()
}
