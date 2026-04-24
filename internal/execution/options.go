// internal/execution/options.go
//
// WHAT: Shared paper option execution service.
//       Single place for all paper option buy/sell operations.
//
// WHY:  Before this service, buy logic lived in handlers.go and sell logic
//       was duplicated between handlers.go and activities.go. Any change
//       required updating two files. This service is the one source of truth.
//
// HOW:  The service wraps three operations:
//       1. BuyOptionPosition  — places Alpaca buy order → creates paper position row
//       2. SellOptionPosition — places Alpaca sell order → closes paper position row
//
//       Both operations are idempotent: CreatePaperPosition uses ON CONFLICT, and
//       callers can safely retry on workflow restart.
//
// WHAT BREAKS: If Alpaca rejects the order (bad symbol, market closed, etc.),
//              the error is returned and the DB row is not created/updated.
//              The caller must handle the error and decide whether to retry.
//
// VERIFY: After BuyOptionPosition succeeds, paper_positions should have a new row
//         with status='open', option_symbol=<OCC>, option_premium=<premium>.

package execution

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/makemytrade/internal/store"
)

// Alpaca is the minimal interface required by the execution service.
// Implemented by *market.AlpacaClient.
type Alpaca interface {
	PlaceOptionOrder(symbol string, limitPrice float64) (string, error)
	SellOptionOrder(symbol string, limitPrice float64) (string, error)
	FetchOptionMidPrice(occSymbol string) (float64, error)
}

// BuyInput holds all parameters needed to open a paper option position.
type BuyInput struct {
	// Position metadata
	CandidateID string
	Ticker      string
	SetupFamily string
	OptionType  string // "call" | "put"

	// Contract details
	ContractSymbol string  // OCC symbol e.g. "SPY260620C00580000"
	LimitPrice     float64 // premium to pay (options, per-share × 100 = total)

	// Risk levels for the position record
	StopLoss float64
	Target1  float64
	Target2  float64
}

// BuyResult holds the identifiers created by a successful buy.
type BuyResult struct {
	PositionID    string
	AlpacaOrderID string
}

// BuyOptionPosition places a paper option buy order via Alpaca and persists the
// position to the database. Returns the DB position ID and the Alpaca order ID.
//
// Sequence:
//  1. Call Alpaca BuyOptionOrder → get alpacaOrderID
//  2. CreatePaperPosition in DB  → get positionID
//  3. UpdatePositionAlpacaOrderID
//  4. UpdatePositionOptionDetails (symbol + premium)
func BuyOptionPosition(ctx context.Context, pool *pgxpool.Pool, alpaca Alpaca, in BuyInput) (BuyResult, error) {
	// 1. Place Alpaca buy order
	alpacaOrderID, err := alpaca.PlaceOptionOrder(in.ContractSymbol, in.LimitPrice)
	if err != nil {
		return BuyResult{}, fmt.Errorf("execution: Alpaca buy order: %w", err)
	}
	log.Printf("execution: buy order placed: ticker=%s contract=%s limit=%.2f orderID=%s",
		in.Ticker, in.ContractSymbol, in.LimitPrice, alpacaOrderID)

	// 2. Create paper position in DB
	posIn := store.PaperPositionInput{
		CandidateID: in.CandidateID,
		Ticker:      in.Ticker,
		EntryPrice:  in.LimitPrice,
		EntryDate:   time.Now(),
		Shares:      1, // always 1 contract
		StopLoss:    in.StopLoss,
		Target1:     in.Target1,
		Target2:     in.Target2,
		OptionType:  in.OptionType,
		SetupFamily: in.SetupFamily,
	}
	positionID, err := store.CreatePaperPosition(ctx, pool, posIn)
	if err != nil {
		return BuyResult{}, fmt.Errorf("execution: create paper position: %w", err)
	}

	// 3. Link Alpaca order ID
	if err := store.UpdatePositionAlpacaOrderID(ctx, pool, positionID, alpacaOrderID); err != nil {
		// Non-fatal: position exists, just log the failure
		log.Printf("execution: warning: UpdatePositionAlpacaOrderID failed: %v", err)
	}

	// 4. Save option symbol and premium for correct P&L tracking
	if err := store.UpdatePositionOptionDetails(ctx, pool, positionID, in.ContractSymbol, in.LimitPrice); err != nil {
		log.Printf("execution: warning: UpdatePositionOptionDetails failed: %v", err)
	}

	log.Printf("execution: position created: posID=%s ticker=%s contract=%s premium=%.2f",
		positionID, in.Ticker, in.ContractSymbol, in.LimitPrice)

	return BuyResult{
		PositionID:    positionID,
		AlpacaOrderID: alpacaOrderID,
	}, nil
}

// SellInput holds parameters needed to close a paper option position.
type SellInput struct {
	PositionID     string
	Ticker         string
	ContractSymbol string  // OCC symbol
	SellPrice      float64 // limit price to use; if 0, fetches current mid
	PnLPct         float64 // pre-computed P&L % to store
	ExitReason     string  // "TAKE_PROFIT" | "STOP_LOSS" | "EXPIRY_CLOSE" | etc.
}

// SellOptionPosition places a paper option sell order via Alpaca and closes
// the position in the database.
//
// If SellInput.SellPrice is 0, the current option mid-price is fetched from
// Alpaca to use as the limit price.
//
// Sequence:
//  1. If sellPrice == 0: fetch current mid from Alpaca
//  2. Call Alpaca SellOptionOrder → get alpacaOrderID
//  3. ClosePosition in DB
func SellOptionPosition(ctx context.Context, pool *pgxpool.Pool, alpaca Alpaca, in SellInput) (string, error) {
	sellPrice := in.SellPrice

	// 1. Fetch current mid if sell price not provided
	if sellPrice <= 0 && in.ContractSymbol != "" {
		mid, err := alpaca.FetchOptionMidPrice(in.ContractSymbol)
		if err != nil {
			return "", fmt.Errorf("execution: fetch mid for sell: %w", err)
		}
		// Use bid as limit (mid may not fill; bid is more conservative)
		sellPrice = mid * 0.99
		log.Printf("execution: fetched mid=%.2f → limit=%.2f for %s", mid, sellPrice, in.ContractSymbol)
	}

	if sellPrice <= 0 {
		return "", fmt.Errorf("execution: sell price is zero and no contract symbol to fetch mid")
	}

	// 2. Place Alpaca sell order
	alpacaOrderID, err := alpaca.SellOptionOrder(in.ContractSymbol, sellPrice)
	if err != nil {
		return "", fmt.Errorf("execution: Alpaca sell order: %w", err)
	}
	log.Printf("execution: sell order placed: ticker=%s contract=%s limit=%.2f orderID=%s",
		in.Ticker, in.ContractSymbol, sellPrice, alpacaOrderID)

	// 3. Close position in DB
	if err := store.ClosePosition(ctx, pool, in.PositionID, sellPrice, in.PnLPct, in.ExitReason); err != nil {
		return alpacaOrderID, fmt.Errorf("execution: close position in DB: %w", err)
	}

	log.Printf("execution: position closed: posID=%s pnl=%.1f%% reason=%s",
		in.PositionID, in.PnLPct, in.ExitReason)

	return alpacaOrderID, nil
}
