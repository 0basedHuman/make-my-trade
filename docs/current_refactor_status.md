# Current Refactor Status

## Completed: bearish_exhaustion_reversal Family (2026-04-24)

### Objective
Add a new optional PUT-only setup family that detects overextended bullish names
near short-term exhaustion, but only confirms after intraday rejection.

### What was done

#### 1. `strategy_rules.yaml` — new family block + 2 pattern scores
- `bearish_exhaustion_reversal` family added after `bearish_momentum_breakdown`
- `max_scan_status: "structural_candidate"` — engine hard-cap (never entry_ready from daily scan)
- Preconditions: `close_above_ema20: true`, `rsi_min_precondition: 72`, `atr_extension_min: 1.8`
- RSI bands inverted: ideal 75–85 (higher = more overbought = better for this family)
- Hold window: 1–7 days (short; reversals tend to be fast)
- DTE 5–14, delta 0.35–0.55 (slightly OTM puts)
- Added to `pattern_scores.bearish`: `overextension_exhaustion: 3`, `rejection_wick_reversal: 3`

#### 2. `internal/strategy/rules.go` — struct additions
- `FamilyPreconditions`: `RSIMinPrecondition float64`, `ATRExtensionMin float64`
- `FamilyConfig`: `MaxScanStatus string`
- `DefaultRules()`: added `bearish_exhaustion_reversal` entry + 2 new pattern scores

#### 3. `internal/strategy/engine.go` — 8 targeted changes
- `Features`: added `ATR14`, `ATRExtension`, `HasATRExtension`
- `computeFeatures()`: computes `ATRExtension = (close - EMA20) / ATR14`
- `checkPreconditions()`: added `rsi_min_precondition` and `atr_extension_min` checks
- `familyOrder`: added `"bearish_exhaustion_reversal"` (5th family)
- `scoreFamily()`: `MaxScanStatus` cap applied after status assignment
- `scoreTrendStructure()`: special case for exhaustion reversal (ATR extension score, more = better)
- `scoreEntryQuality()`: new `name string` param; exhaustion reversal inverts logic (more extension = better entry)
- `detectPatterns()`: new `f Features` param; detects `overextension_exhaustion` (>= 20% upper wick + ATR >= 1.8) and `rejection_wick_reversal` (>= 40% upper wick)
- `computePenalties()`: `RSIOverextendedBullish` penalty exempted for this family
- Layer 5 reason codes: `bearish_exhaustion_reversal` gets `rsi_extended`, `above_ema20`, `bearish_reversal_setup` instead of `below_ema20`

### Lifecycle flow (as deployed)
```
06:25 Daily scan → if close > EMA20, RSI >= 72, ATRExtension >= 1.8
                 → scores to structural_candidate (max_scan_status cap)
                 → watchlisted for opening confirmation

06:42 Opening confirmation activity:
      TODO: detect intraday rejection for structural_candidate with
            family == "bearish_exhaustion_reversal":
              - first 10/30/60m close below VWAP or OR midpoint
              - relative volume >= 1.3
              - QQQ/SPY not strongly bullish
              - no hard blocks (spread, VWAP hold, no wick)
            If rejection confirmed → promote to entry_ready → Claude payload

07:45 Continuation review:
      Same rejection check on fresh 6:30→7:45 intraday bars (TODO).
```

### Remaining work
- `RunOpeningConfirmationActivity` and `RunContinuationReviewActivity` do NOT yet
  check for `bearish_exhaustion_reversal` structural candidates.
  Currently only `entry_ready` candidates are sent to Claude.
  TODO: add intraday rejection detection pass for this family before building payload.
- The `max_scan_status` cap means these names will never auto-confirm without
  the activity-level promotion code being written.

### Files changed
- `strategy_rules.yaml`
- `internal/strategy/rules.go`
- `internal/strategy/engine.go`

---

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
