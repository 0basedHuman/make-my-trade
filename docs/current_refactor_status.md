# Current Refactor Status

## Completed: v7 Architecture Upgrade (2026-04-23)

### Objective
Turn make-my-trade into a proper autonomous paper options trading engine where
Claude is the final authority at opening confirmation time.

### What was done

#### 1. strategy_rules.yaml → v7
- Bumped version 6 → 7
- Added `trade_frequency` block: `max_entry_ready_to_confirm=5`, `max_new_positions_per_day=3`, `min_entry_ready_score=68`, `min_claude_confidence=0.65`
- Added `claude_confirmation` block: `enabled=true`, `min_confidence=0.65`, `deterministic_auto_reject_is_hard_block=true`
- Expanded `feature_windows` with v7 indicator periods: `realized_vol_short/long`, `momentum_short/long`, `entropy`, `bollinger`

#### 2. internal/strategy/rules.go → v7
- Added `TradeFrequencyConfig` struct
- Added `ClaudeConfirmationConfig` struct
- Extended `FeatureWindowsConfig` with 6 new period fields
- Added both to `Rules` struct and `DefaultRules()`

#### 3. internal/indicators/indicators.go — 5 new functions
- `RealizedVolatility(closes, period)` — annualized log-return stddev
- `VolScaledMomentum(closes, period)` — return / realized_vol
- `ShannonEntropy(closes, period)` — sign-based entropy of daily returns
- `BollingerWidth(closes, period, numStdDev)` — (upper-lower)/sma
- `SqueezeRatio(bars, period)` — Bollinger width / Keltner width

#### 4. internal/strategy/engine.go — v7 Features + 4 scoring sleeves
- Added 12 new fields to `Features` struct (RealVol20/40, VolScaledMom63/126, Entropy30, BollingerWidth20, SqueezeRatio20, Return63d/126d + has* bools)
- Extended `computeFeatures` to populate all new fields from YAML periods
- `scoreMomentumAlignment`: sleeve 1 (vol-scaled momentum bonus +0.15) + sleeve 2 (entropy gate)
- `scoreTrendStructure`: sleeve 3 (SMA/EMA divergence, return63d vs return126d ±0.10)
- `scoreEntryQuality`: sleeve 4 (squeeze ratio +0.10 bonus, Bollinger width expansion +0.08, overextension penalty)
- Added `ShortlistEntryReady(analyses, maxCount, minScore)` — filters entry_ready, sorts by FinalScore, caps count

#### 5. internal/claude/client.go — new confirmation structs
- `EntryConfirmationPayload` + `ConfirmationCandidate`, `DailyContext`, `OpeningContext`, `ConfirmationContract`, `RiskContext`, `DeterministicSignals`, `HardBlockSummary`
- `EntryConfirmationDecision`, `EntryConfirmationResponse`
- `ConfirmEntry()` method with dedicated system prompt (Claude as final authority)

#### 6. internal/execution/options.go — new shared execution service
- `BuyInput`, `BuyResult`, `SellInput` structs
- `BuyOptionPosition(ctx, pool, alpaca, in)` — PlaceOptionOrder → CreatePaperPosition → UpdatePositionAlpacaOrderID → UpdatePositionOptionDetails
- `SellOptionPosition(ctx, pool, alpaca, in)` — optional mid-fetch → SellOptionOrder → ClosePosition

#### 7. internal/market/alpaca.go — new exported function
- `FilterChainQuality(contracts, minOI, minVolume, maxSpreadPct)` — exported chain quality filter; handlers.go delegates to it

#### 8. internal/workflow/activities.go — RunOpeningConfirmationActivity rewritten (v7)
New flow:
1. Load entry_ready candidates (ordered by score DESC)
2. Shortlist top N by `MinEntryReadyScore` threshold
3. Non-shortlisted → watch_only immediately
4. For shortlisted: run deterministic EvaluateConfirmation (evidence only)
5. Hard block fired → watch_only (DeterministicAutoRejectIsHardBlock)
6. Build EntryConfirmationPayload, call Claude.ConfirmEntry
7. Claude CONFIRM + confidence ≥ min → fetch chain, select best contract, CreatePaperPosition + PlaceOptionOrder
8. Claude REJECT → watch_only
9. Update daily_summaries

#### 9. internal/api/handlers.go
- `filterChainQuality` now delegates to `market.FilterChainQuality`
- Added `candidateStatusLabel()` helper
- Added `StatusLabel` field to `CandidateResponse` JSON
- `status_label` set for all paths: "WAITING FOR CLAUDE CONFIRMATION" for entry_ready, "PAPER POSITION OPEN" for confirmed

#### 10. migrations/000006_position_enrichment.up.sql (NEW)
- Adds: `strike`, `expiration`, `dte_at_entry`, `exit_order_id`, `exit_order_status`, `claude_confirm_confidence`, `claude_confirm_reason` to paper_positions

#### 11. Tests (NEW)
- `internal/indicators/indicators_v7_test.go` — 5 functions tested
- `internal/strategy/shortlist_test.go` — 3 test cases for ShortlistEntryReady

### Files changed
- `strategy_rules.yaml` (v6→v7)
- `internal/strategy/rules.go`
- `internal/indicators/indicators.go`
- `internal/indicators/indicators_v7_test.go` (NEW)
- `internal/strategy/engine.go`
- `internal/strategy/shortlist_test.go` (NEW)
- `internal/claude/client.go`
- `internal/execution/options.go` (NEW)
- `internal/market/alpaca.go`
- `internal/workflow/activities.go`
- `internal/api/handlers.go`
- `migrations/000006_position_enrichment.up.sql` (NEW)
- `migrations/000006_position_enrichment.down.sql` (NEW)

### Previous completed work (v0.3-option-pnl-fix)
- Option P&L tracking via FetchOptionMidPrice
- SellOptionOrder added to alpaca.go
- RunPositionReview handler added
- PARTIAL_TAKE_PROFIT → EXIT mapping for 1-contract positions
- RTX position closed at +74.4%

## Remaining work
- None critical. System can now:
  1. Run daily analysis at 14:00 UTC (7 AM PDT) — 26 tickers → deterministic engine → Claude review
  2. Run shortlisted confirmation at 14:45 UTC → Claude is final authority
  3. Create paper positions automatically for confirmed entries
  4. Review positions at 20:45 UTC with correct option P&L
  5. Exit winning positions when review triggers EXIT
  6. Run weekly review Sunday 15:00 UTC

## Exact next step
1. Restart server + worker to pick up new binary
2. Run migration 000006: `psql $DB_URL < migrations/000006_position_enrichment.up.sql`
3. Watch 14:45 UTC confirmation log for:
   `activity: RunOpeningConfirmation done — confirmed=X watch_only=Y total_entry_ready=Z shortlisted=W`
4. Verify Claude ConfirmEntry is called (look for `claude confirm-entry: sending N candidates`)
