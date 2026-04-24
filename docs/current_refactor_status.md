# Current Refactor Status

## Completed: Option P&L tracking fix (2026-04-23)

### Problem fixed
`RunPositionReviewActivity` was computing P&L from underlying stock price, not option premium.
For puts: stock falling = option winning, so `(stock_now - stock_entry)/stock_entry` was negative
when the position was actually +51.8% profitable. RTX put was not exiting despite being up 50%+.

### What was done
1. Migration 000005 (`option_symbol TEXT`, `option_premium NUMERIC(12,4)`) — columns already existed,
   schema confirmed applied.
2. `store.go` — added `UpdatePositionOptionDetails(positionID, symbol, premium)`;
   added `OptionSymbol`/`OptionPremium` to `ReviewablePosition`; updated `GetOpenPositionsForReview`.
3. `market/alpaca.go` — added `FetchOptionMidPrice(occSymbol)` using
   `/v1beta1/options/snapshots?symbols=<OCC>` endpoint.
4. `handlers.go` — after placing Alpaca order, calls `UpdatePositionOptionDetails` to persist
   OCC symbol and limit price paid.
5. `activities.go` — new P&L logic:
   - If `OptionSymbol != ""` and `OptionPremium > 0`: fetch option mid-price, compute
     `(mid - premium) / premium * 100` — correct for both calls and puts.
   - Fallback (legacy positions): use stock price delta, inverted for puts.
   - `PositionInput.EntryPrice` now sends option premium (not stock price) to Claude.
6. RTX position backfilled: `option_symbol='RTX260508P00190000'`, `option_premium=6.58`.

### Files changed
- `migrations/000005_option_pnl.up.sql` (NEW)
- `migrations/000005_option_pnl.down.sql` (NEW)
- `internal/store/store.go` — UpdatePositionOptionDetails, ReviewablePosition, GetOpenPositionsForReview
- `internal/market/alpaca.go` — FetchOptionMidPrice
- `internal/api/handlers.go` — placeAlpacaOptionOrder: save option details
- `internal/workflow/activities.go` — RunPositionReviewActivity: correct P&L computation

### Current RTX position state
- Symbol: RTX260508P00190000 (put, strike $190, expiry 2026-05-08)
- Entry premium: $6.58 | Current mid: ~$9.99 | P&L: ~+51.8%
- Next position review: 20:45 UTC (1:45 PM PDT) via Temporal schedule
- Expected: Claude will see +51.8% and recommend EXIT/TAKE_PROFIT

### Version
Saved as `v0.3-option-pnl-fix`

## Previous completed work

### Strategy core rewrite (v0.2-strategy-rewrite)
- `strategy_rules.yaml` → version 6: unified families block, weighted scoring, penalties
- `internal/strategy/rules.go` → complete rewrite matching YAML v6
- `internal/strategy/engine.go` → new layered scoring engine

### ForceConfirm date rollover fix
- `internal/api/handlers.go` — removed date dependency in fallback lookup

### Other fixes
- `.gitignore`, `.env.example`, `./version` snapshot tool
- Worker/server DST comment fix
- Alpaca dual-URL fix (data vs broker)

## Remaining work
- None critical for autonomous operation. System should now:
  1. Run daily analysis at 14:00 UTC (7 AM PDT)
  2. Run confirmation at 14:45 UTC
  3. Create paper positions automatically
  4. Review positions at 20:45 UTC with correct P&L
  5. Exit winning positions when review triggers EXIT
  6. Run weekly review Sunday 15:00 UTC

## Exact next step
Wait for 20:45 UTC review to fire. Verify in logs:
  `activity: option P&L RTX RTX260508P00190000: mid=X.XX premium=6.58 pnl=XX.X%`
  Followed by: position closed or HOLD decision from Claude.
