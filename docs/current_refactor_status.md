# Current Refactor Status

## Completed: RunOpeningConfirmationActivity (2026-04-18)

### What was done
Full opening confirmation workflow implemented. `POST /api/run-confirmation` is live.

### Files changed (Session 3 — confirmation workflow)
- `migrations/000003_confirmation_upgrade.up/down.sql` — Added `candidate_status`, `setup_family`, `direction`, `prev_day_volume` columns + indexes to `trade_candidates`.
- `internal/strategy/rules.go` — Added `OpenConfirmationConfig`, `ConfirmationChecks`, `AutoRejectChecks` structs; added `OpenConfirmation` field to `Rules`; updated `DefaultRules()`.
- `internal/strategy/confirmation.go` (NEW) — Pure `EvaluateConfirmation()` evaluator: 5 auto-reject checks + 5 positive signals, all YAML-driven.
- `internal/market/alpaca.go` — Added `FetchIntradayBars()` + `fetchIntradayBatch()` for 1-min bars with RFC3339 timestamps and `1Min` timeframe.
- `internal/store/store.go` — Added `CandidateStatus/SetupFamily/Direction/PrevDayVolume` to `Candidate` and `UpsertCandidateInput`; updated `UpsertCandidate` SQL ($32-$35); updated `GetCandidatesForDate` SELECT/Scan; added `UpdateCandidateStatus`, `GetEntryReadyCandidates`, `ConfirmationStoreInput`, `UpsertTradeConfirmation`.
- `internal/workflow/activities.go` — Replaced `RunOpeningConfirmationActivity` stub with full implementation; updated `UpsertCandidateInput` call to pass lifecycle fields.
- `internal/api/handlers.go` — Added `RunConfirmation` HTTP handler (POST /api/run-confirmation); updated `UpsertCandidateInput` call to pass lifecycle fields.
- `cmd/server/main.go` — Registered `/api/run-confirmation` route.

### End-to-end flow
1. `POST /api/run-analysis` → engine assigns `candidate_status` → `UpsertCandidate` stores it
2. `POST /api/run-confirmation` (after 6:40 AM PT):
   - Loads `entry_ready` candidates from DB
   - Fetches 1-min bars for all tickers + SPY (6:30–6:40 AM PT window)
   - Calls `EvaluateConfirmation()` for each (pure, no I/O)
   - Updates `candidate_status` → "confirmed" or "watch_only"
   - Writes to `trade_confirmations`
   - Updates `daily_summaries.candidates_confirmed`
   - Returns updated `AnalysisResponse` (confirmed candidates appear in Confirmed tab)
3. `GET /api/daily-analysis` → `buildAnalysisResponseFromDB` routes by `candidate_status`

### Key behavior
- `blocked_by_event` candidates never reach confirmation (they're never `entry_ready`)
- Options output surfaced only for `confirmed` (handler gating unchanged)
- Confirmation is idempotent: re-running updates existing `trade_confirmations` rows via `ON CONFLICT (candidate_id)`

## Completed: State machine + routing bug fixes (2026-04-18)

### Bugs fixed
**Bug 1 — `awaiting_open_confirmation` on wrong status** (`internal/strategy/engine.go`):
- Root cause: `awaiting_open_confirmation` reason code was added to `structural_candidate` rows (where entry rules failed), not `entry_ready`.
- Fix: `entry_ready` rows get `awaiting_open_confirmation` (they passed all pre-open rules). `structural_candidate` rows get specific failure codes (`volume_weak`, `rsi_extended`, `rsi_too_weak`, `entry_too_extended`) via new `entryFailReasons()` helper.
- Added `entryFailReasons []string` field to `familyResult` struct. Each family block now populates it.

**Bug 2a — structural_candidate + earnings not becoming blocked_by_event** (`engine.go`):
- Root cause: only `entry_ready` was overridden to `blocked_by_event` on earnings risk. `structural_candidate` kept its status.
- Fix: both `entry_ready` AND `structural_candidate` become `blocked_by_event` when `earningsRisk == true`. Both get `Eligible = false`.

**Bug 2b — DB path routing used wrong fallback** (`internal/api/handlers.go:buildAnalysisResponseFromDB`):
- Root cause: when no Claude JSON available (blocked_by_event/entry_ready rows), `DecisionStatus` fell back to hardcoded `"structural_candidate"` instead of `c.CandidateStatus`.
- Fix: fallback now uses `c.CandidateStatus` (engine-computed DB column). True last-resort fallback is `"rejected"`.
- Also: `cr.SetupFamily`, `cr.BaseTarget`, `cr.StretchTarget` now initialized from DB columns so cards render correctly without Claude decision.
- Also: routing switch now sets `WhatIsMissing` for `entry_ready`, `structural_candidate`, `blocked_by_event`.

**Bug 3 — Score "0" showing for rows without Claude decision** (`web/static/index.html`):
- Root cause: `scoreVisible` was `true` for `entry_ready` rows, but `score` was 0 when no Claude decision yet. Score bar rendered "0".
- Fix: score bar only renders when `scoreVisible && score > 0`.

### Files changed
- `internal/strategy/engine.go` — `entryFailReasons()` helper; `familyResult.entryFailReasons []string`; fixed status/reason code assignment; fixed structural_candidate → blocked_by_event override.
- `internal/api/handlers.go` — Fixed `buildAnalysisResponseFromDB` fallback + field init + `WhatIsMissing` in routing.
- `web/static/index.html` — Score bar guard: `scoreVisible && score > 0`.

### Verification mapping
| Ticker | Expected status | Reason |
|--------|----------------|--------|
| AAPL | `entry_ready` | Passes all pre-open gates, no earnings |
| HOOD | `entry_ready` | Same |
| COIN | `entry_ready` | Same |
| FCX  | `blocked_by_event` | Earnings within 5-day blackout |
| BA   | `blocked_by_event` | Earnings within 5-day blackout |
| LMT  | `blocked_by_event` | Earnings within 5-day blackout |

## Completed: Autonomous operation (2026-04-18)

### What was done
Full autonomous one-month operation implemented. All four required workflows are live
with Temporal schedules. Paper entry is automatic and idempotent. No human approval
needed for any step.

### Files changed (Session 4 — autonomous operation)
- `migrations/000004_auto_entry.up/down.sql` (NEW) — UNIQUE(candidate_id) on paper_positions; option_type/setup_family columns; weekly_reviews table.
- `internal/store/store.go` — Added: `CreatePaperPosition` (ON CONFLICT idempotent), `HasOpenPositionForTicker`, `InsertPositionEvent`, `ClosePosition`, `UpsertPositionReview`, `ReviewablePosition`, `GetOpenPositionsForReview`, `WeeklyReviewInput`, `InsertWeeklyReview`, `GetClosedPositionsInRange`.
- `internal/claude/client.go` — Added `GenerateText(systemPrompt, userMessage string) (string, error)` for free-text review responses.
- `internal/workflow/activities.go` — Extended `RunOpeningConfirmationActivity` with auto paper entry step (step 7); replaced `RunPositionReviewActivity` stub with full implementation (quote fetch → PnL → Claude review → persist + execute EXIT); added `RunWeeklyReviewActivity` + helpers `mapPositionAction`, `formatPaperPositionList`, `formatReviewablePositionList`.
- `internal/workflow/daily.go` — Added `WeeklyReviewCycle` workflow.
- `cmd/worker/main.go` — Registered `WeeklyReviewCycle` workflow + `RunWeeklyReviewActivity`; added `registerSchedules()` that creates 4 Temporal schedules (daily-research 14:00 UTC, open-confirmation 14:45 UTC, position-review 20:45 UTC, weekly-review 15:00 UTC Sunday).
- `internal/api/handlers.go` — Added open-ticker suppression in `runPipeline` (Step 6 loop); added auto paper entry block in `RunConfirmation` handler (mirrors activity path).

### Autonomous flow (complete)
1. `DailyResearchCycle` (6:00 AM PT weekdays) → analysis + candidate lifecycle states
2. `OpeningConfirmationCycle` (6:45 AM PT weekdays) → promote entry_ready → confirmed/watch_only + **create paper_positions** automatically
3. `DailyPositionReview` (12:45 PM PT weekdays) → fetch quotes → Claude HOLD/EXIT decision → persist position_reviews → execute EXITs
4. `WeeklyReviewCycle` (7:00 AM PT Sunday) → aggregate week data → Claude weekly review → persist weekly_reviews

### Key behavior
- Auto paper entry is idempotent: ON CONFLICT (candidate_id) on paper_positions means re-running confirmation never creates duplicate rows.
- Open ticker suppression: `runPipeline` skips tickers with `status='open'` in paper_positions before analysis.
- EXIT execution: `RunPositionReviewActivity` calls `ClosePosition` + inserts `position_closed` event when Claude returns `exit_now`.
- Temporal schedule registration is also idempotent: "already exists" errors are logged and ignored on worker restart.

### Remaining work
- None for autonomous operation phase.
- Optional future: daily_summaries.open_positions update in position-review activity.
- Optional future: HOLD_TIGHTEN_STOP action updates paper_positions.stop_loss.

### Verification steps
```
# 1. Confirmed creates exactly one paper position
psql $DB_URL -c "SELECT id, ticker, status, entry_price FROM paper_positions ORDER BY opened_at DESC LIMIT 5;"

# 2. Rerun confirmation doesn't duplicate
# POST /api/run-confirmation twice → same position count

# 3. Open ticker suppressed in next day's analysis
psql $DB_URL -c "SELECT ticker FROM paper_positions WHERE status='open';"
# Those tickers should be absent from next runPipeline's prescreened results log

# 4. Position review writes rows
psql $DB_URL -c "SELECT ticker, review_date, suggested_action, action_executed FROM position_reviews ORDER BY review_date DESC LIMIT 10;"

# 5. Weekly review persisted
psql $DB_URL -c "SELECT week_start, week_end, LEFT(summary, 200) FROM weekly_reviews ORDER BY week_start DESC LIMIT 3;"

# 6. Temporal schedules registered
# Temporal UI → Schedules → should show 4 schedules
```
