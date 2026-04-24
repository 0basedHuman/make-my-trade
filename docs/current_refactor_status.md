# Current Refactor Status

## Completed: v7 Lifecycle Correctness Fixes (2026-04-24)

### Objective
Consolidate buy/sell lifecycle into a single execution path and ensure Claude
receives full market + contract context before confirming entries.

### What was done

#### 1. `internal/claude/client.go` — OptionVolume added to ConfirmationContract
- Added `OptionVolume int \`json:"option_volume"\`` to `ConfirmationContract`
- Claude now sees both open_interest and today's option volume for every candidate

#### 2. `internal/workflow/activities.go` — Market context + single buy/sell owner
- Added VIX + BTC ROC20 fetch before Claude call in `RunOpeningConfirmationActivity`
- Added SPY trend derivation from first-5m bar
- Populated `EntryConfirmationPayload.MarketContext` with VIX, BTCRoc20, SPXTrend
- Populated `ConfirmationContract.OptionVolume` from selected contract
- Replaced manual `CreatePaperPosition → PlaceOptionOrder` block with `execution.BuyOptionPosition`
- Replaced manual `SellOptionOrder → ClosePosition` block in `RunPositionReviewActivity` with `execution.SellOptionPosition`
- If sell fails: records `sell_failed` event and keeps position open (no premature close)
- Updated `RunOpeningConfirmationActivity` HOW comment to reflect correct flow:
  fetch/filter/select contract → send contract to Claude → Claude confirms → buy same contract

#### 3. `internal/api/handlers.go` — Single execution path for all API buy/sell paths
- Added `execution` import
- Replaced `placeAlpacaOptionOrder` helper with slim `selectBestContract` (returns symbol + price only)
- `run-confirmation` handler: uses `execution.BuyOptionPosition` (contract pre-selected via `selectBestContract`)
- `force-confirm` handler: uses `execution.BuyOptionPosition`
- Retry path (position exists, re-place order): uses `selectBestContract` + direct `PlaceOptionOrder` (no new DB row)
- `RunPositionReview` handler: uses `execution.SellOptionPosition` with same fail-safe behavior

### Execution lifecycle ownership (verified)
- `internal/execution/options.go` is the sole owner of buy/sell lifecycle
- `CreatePaperPosition`, `SellOptionOrder`, `ClosePosition` are gone from workflow and API
- Only exception: retry path in handlers.go calls `PlaceOptionOrder` directly when the DB position already exists

### Correct confirmation flow (as implemented)
```
entry_ready
  → shortlist by score
  → fetch option chain + filter quality + select best contract  ← BEFORE Claude
  → if no contract → watch_only
  → populate ConfirmationCandidate.Contract with selected contract
  → fetch VIX + BTC ROC20 + SPY trend
  → Claude.ConfirmEntry(payload with market context + contract)
  → CONFIRM → execution.BuyOptionPosition (same contract Claude saw)
  → REJECT  → watch_only
```

### Files changed
- `internal/claude/client.go`
- `internal/workflow/activities.go`
- `internal/api/handlers.go`

### Previous completed work (v7 Architecture Upgrade, 2026-04-23)
See git history. Summary: 5 new indicators, 4 scoring sleeves, Claude as final
authority, execution service, migration 000006, contract-before-Claude rewrite.

## Remaining risks
- `execution.BuyOptionPosition` still places Alpaca order BEFORE creating the DB
  position (order in execution/options.go: PlaceOptionOrder → CreatePaperPosition).
  If Alpaca succeeds but DB insert fails, the order is orphaned. Consider reversing
  to DB-first inside the execution service in a future pass.
- `SellOptionPosition` closes DB position before confirming Alpaca fill. For paper
  trading this is acceptable, but worth revisiting if fill confirmation matters.
- `selectBestContract` in API re-fetches the chain at buy time — chain may differ
  slightly from the analysis chain. For paper trading this is acceptable.

## Exact next step
1. Watch 14:00 UTC analysis log for `activity: RunDailyAnalysis done`
2. Watch 14:45 UTC confirmation log for:
   `activity: RunOpeningConfirmation done — confirmed=X watch_only=Y shortlisted=W`
   `activity: claude_confirmed_contract: TICKER SYMBOL conf=X reason="..."`
3. Verify `paper_positions` row has `option_symbol`, `claude_confirm_confidence`,
   `claude_confirm_reason` populated after a confirmed entry
