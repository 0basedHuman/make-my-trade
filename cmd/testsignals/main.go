package main

import (
	"fmt"
	"time"
	"github.com/yourname/makemytrade/internal/market"
)

func main() {
	fmt.Println("=== CBOE Equity P/C Ratio ===")
	pc, err := market.FetchEquityPCRatio()
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("Date: %s  P/C: %.2f  Bias: %s\n", pc.Date, pc.PCRatio, pc.Bias)
	}

	fmt.Println("\n=== Finviz: AAPL ===")
	fv, err := market.FetchFinvizSnapshot("AAPL")
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("ShortFloat: %.2f%%  ShortRatio: %.2f  Headlines: %d\n", fv.ShortFloatPct, fv.ShortRatio, len(fv.Headlines))
		for i, h := range fv.Headlines {
			if i >= 3 { break }
			fmt.Printf("  [%d] %s\n", i+1, h)
		}
	}

	fmt.Println("\n=== FINRA Short Interest: AAPL ===")
	time.Sleep(300 * time.Millisecond)
	fi, err := market.FetchFinraShortInterest("AAPL")
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("Date: %s  Shares: %d  Change: %.2f%%  Trend: %s\n", fi.SettlementDate, fi.ShortShares, fi.ChangePercent, fi.Trend)
	}

	fmt.Println("\n=== Yahoo P/C Ratio: AAPL ===")
	yh, err := market.FetchYahooPCRatio("AAPL")
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("CallOI: %d  PutOI: %d  P/C: %.2f  Bias: %s\n", yh.CallOI, yh.PutOI, yh.PCRatio, yh.Bias)
	}
}
