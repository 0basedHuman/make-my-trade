// internal/market/fill.go
//
// WHAT: Quote-realistic option fill model for paper trading.
//
// WHY:  Naive backtests assume perfect mid-price fills, which never happen in
//       practice. Wide-spread options cost 2-4x their theoretical slippage.
//       This module encodes realistic fill assumptions so that paper-trade P&L
//       reflects what a real trader would experience.
//
// HOW:  Entry: mid when spread ≤4%; proportional haircut toward ask when 4-8%;
//       reject when >8% or bid==0 or inverted quote.
//       Exit:  bid × (1 - 0.5%) — conservative bid-side with slippage.
//
// Fill quality score (0-100):
//   spread ≤1%  → 100  (institutional quality)
//   spread ≤2%  → 90
//   spread ≤4%  → 80   (acceptable retail)
//   spread ≤6%  → 60   (poor; take haircut)
//   spread ≤8%  → 40   (at the gate limit)
//
// WHAT BREAKS: If bid>ask (inverted quote from stale data), the fill is
//              rejected. Callers must handle Rejected=true by skipping the trade.
//
// VERIFY: go test ./internal/market/ -run TestFill -v

package market

// FillResult is the output of ComputeEntryFill and ComputeExitFill.
type FillResult struct {
	Price        float64 // chosen fill price
	Mid          float64 // (bid+ask)/2
	Bid          float64
	Ask          float64
	SpreadPct    float64 // (ask-bid)/ask*100; 0 if ask==0
	QualityScore float64 // 0–100
	SlippageEst  float64 // price - mid (positive = paid more than mid)
	Mode         string  // "mid", "haircut", "bid", or ""
	Rejected     bool
	RejectReason string
}

// ComputeEntryFill computes a realistic paper-entry fill price.
//   - bid ≤ 0                    → reject (zero_bid)
//   - ask ≤ 0                    → reject (zero_ask)
//   - ask < bid                  → reject (inverted_spread)
//   - spread > 8%                → reject (spread_too_wide; belt-and-suspenders)
//   - spread ≤ 4%                → use mid
//   - spread 4%–8%               → mid + proportional haircut toward ask
func ComputeEntryFill(bid, ask float64) FillResult {
	if bid <= 0 {
		return FillResult{Rejected: true, RejectReason: "zero_bid", Bid: bid, Ask: ask}
	}
	if ask <= 0 {
		return FillResult{Rejected: true, RejectReason: "zero_ask", Bid: bid, Ask: ask}
	}
	if ask < bid {
		return FillResult{Rejected: true, RejectReason: "inverted_spread", Bid: bid, Ask: ask}
	}

	mid := (bid + ask) / 2.0
	spreadPct := (ask - bid) / ask * 100.0

	if spreadPct > 8.0 {
		return FillResult{
			Rejected:     true,
			RejectReason: "spread_too_wide",
			Bid:          bid,
			Ask:          ask,
			Mid:          mid,
			SpreadPct:    spreadPct,
		}
	}

	var price float64
	var mode string
	if spreadPct <= 4.0 {
		price = mid
		mode = "mid"
	} else {
		// Linear interpolation: at 4% excess=0 (use mid); at 8% excess=1 (use ask).
		excess := (spreadPct - 4.0) / 4.0
		price = mid + excess*(ask-mid)
		mode = "haircut"
	}

	return FillResult{
		Price:        price,
		Mid:          mid,
		Bid:          bid,
		Ask:          ask,
		SpreadPct:    spreadPct,
		QualityScore: fillQualityScore(spreadPct),
		SlippageEst:  price - mid,
		Mode:         mode,
	}
}

// ComputeExitFill computes a realistic paper-exit fill price.
// Exits always fill on the bid side with 0.5% slippage to model market-order urgency.
func ComputeExitFill(bid, ask float64) FillResult {
	if bid <= 0 {
		return FillResult{Rejected: true, RejectReason: "zero_bid", Bid: bid, Ask: ask}
	}
	mid := (bid + ask) / 2.0
	spreadPct := 0.0
	if ask > 0 {
		spreadPct = (ask - bid) / ask * 100.0
	}
	const exitSlippage = 0.005
	price := bid * (1.0 - exitSlippage)
	return FillResult{
		Price:        price,
		Mid:          mid,
		Bid:          bid,
		Ask:          ask,
		SpreadPct:    spreadPct,
		QualityScore: fillQualityScore(spreadPct),
		SlippageEst:  price - mid, // negative: sold below mid
		Mode:         "bid",
	}
}

func fillQualityScore(spreadPct float64) float64 {
	switch {
	case spreadPct <= 1.0:
		return 100
	case spreadPct <= 2.0:
		return 90
	case spreadPct <= 4.0:
		return 80
	case spreadPct <= 6.0:
		return 60
	default:
		return 40
	}
}
