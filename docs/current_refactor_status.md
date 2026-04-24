# Current Refactor Status

## Completed: Trading-Day Schedule Fix (2026-04-24)

### Objective
Fix the schedule so the app behaves like a logical autonomous paper options
trader. First-10-minute opening confirmation must fire at 6:42 PT, not 7:45 PT.

### What was done

#### 1. `strategy_rules.yaml` — `schedule:` block added
```yaml
schedule:
  timezone: America/Los_Angeles
  daily_scan_time:             "06:25"
  opening_confirmation_time:   "06:42"
  opening_confirmation_cutoff: "06:55"
  first_position_review_time:  "07:15"
  continuation_review_time:    "07:45"
  end_of_day_review_time:      "12:45"
  weekly_review_time:          "07:00"
```
Times are PT-local. Changing a time: edit YAML → delete Temporal schedule → restart worker.

#### 2. `internal/strategy/rules.go` — `ScheduleConfig` struct added
- Mirrors all 7 YAML fields with typed Go struct

#### 3. `cmd/worker/main.go` — 6 schedules, PT-local crons, DST-safe
- `TimeZoneName: "America/Los_Angeles"` on every schedule (Temporal handles DST)
- Cron expressions are now PT-local (not UTC conversions)
- Two new schedules: `makemytrade-first-position-review`, `makemytrade-continuation-review`
- Times wired from `rules.Schedule` YAML fields with hardcoded fallbacks
- Old 4 UTC schedules deleted; 6 new PT schedules registered

#### 4. `internal/workflow/daily.go` — 2 new workflow definitions
- `FirstPositionReviewCycle` — 7:15 PT, calls `RunPositionReviewActivity`
- `ContinuationReviewCycle` — 7:45 PT, calls `RunContinuationReviewActivity`
- Updated header comment with full 6-step schedule

#### 5. `internal/workflow/activities.go` — 4 changes

**Staleness guard in `RunOpeningConfirmationActivity`:**
- If PT time > `opening_confirmation_cutoff` (default 6:55), returns immediately:
  `"opening_confirmation_stale: ...use continuation review instead"`
- Prevents first-10-min entry logic from running late as a delayed opening candle

**New `RunContinuationReviewActivity`:**
- Logs `continuation_review_started` with full `continuation_window=06:30-<now>`
- Delegates to `RunPositionReviewActivity` (position risk management)
- TODO comment: full continuation entry logic (VWAP structure, 60-min high/low,
  second-leg Claude confirmation with fresh payload)

**Log statements added:**
- `schedule_daily_scan_started` at start of `RunDailyAnalysisActivity`
- `schedule_opening_confirmation_started opening_confirmation_window=06:30-06:40`
- `claude_confirmation_time=HH:MM candidates=N vix=X btc_roc=X spy_trend=X`
- `first_position_review_started` at start of `RunPositionReviewActivity`
- `continuation_review_started continuation_window=06:30-HH:MM`

### Autonomous trading day (as deployed)
```
06:25 PT  DailyResearchCycle         — overnight scan, classify candidates
06:42 PT  OpeningConfirmationCycle   — 6:30-6:40 candle, Claude entry (cutoff 6:55)
07:15 PT  FirstPositionReviewCycle   — early risk management on open positions
07:45 PT  ContinuationReviewCycle    — fresh intraday bars, position tighten/exit
12:45 PT  DailyPositionReview        — end-of-day: hold overnight vs exit
Sunday 07:00 PT  WeeklyReviewCycle   — performance review + tuning proposals
```

### Files changed
- `strategy_rules.yaml`
- `internal/strategy/rules.go`
- `cmd/worker/main.go`
- `internal/workflow/daily.go`
- `internal/workflow/activities.go`

### Remaining work / TODOs
- `RunContinuationReviewActivity` currently delegates to `RunPositionReviewActivity`.
  Future: add real continuation entry logic:
  - fetch fresh intraday bars 6:30→7:45
  - evaluate VWAP hold/reclaim, 60-min high/low, higher-low structure
  - check option spread still acceptable
  - if still-valid entry_ready setup and no hard blocks → Claude continuation payload
  - if Claude confirms → execution.BuyOptionPosition (same single lifecycle path)
- `execution.BuyOptionPosition` places Alpaca order BEFORE DB insert (Alpaca-first).
  Consider reversing to DB-first in execution/options.go for orphan-prevention.
- `selectBestContract` in API re-fetches chain at buy time. Acceptable for paper.

### How to change schedule times
1. Edit `schedule:` block in `strategy_rules.yaml`
2. Delete old Temporal schedule(s):
   `go run internal/tools/delete_schedules.go` (or Temporal UI)
3. Restart worker — new schedules auto-register
