# Current Refactor Status

## Completed: Mechanical Exits + 7–14 DTE Enforcement (2026-04-24)

### Objective
Enforce 7–14 DTE options with target DTE=10, and add automatic mechanical exit rules
so the app doesn't rely solely on scheduled Claude review to close trades.

### What was done

#### 1. `strategy_rules.yaml` — `risk:` block added
```yaml
risk:
  option_lifecycle:
    dte_min: 7
    dte_max: 14
    target_dte: 10
    avoid_dte_below: 4
    contracts_per_trade: 1
  mechanical_exits:
    enabled: true
    stop_loss_pct: 30
    take_profit_pct: 50
    trail_start_pct: 35
    trail_giveback_pct: 20
    force_eod_exit: true
    max_hold_days: 1
```

#### 2. `internal/strategy/rules.go` — new structs
- `RiskConfig`, `OptionLifecycleConfig`, `MechanicalExitsConfig`
- Added `Risk RiskConfig` to `Rules` struct and `DefaultRules()`

#### 3. `migrations/000007_risk_state.up.sql` + `down.sql` (new)
- `peak_option_price`, `trailing_active`, `last_option_price`,
  `last_risk_check_at`, `hold_overnight_approved`, `hold_overnight_approved_at`
  added to `paper_positions` with `IF NOT EXISTS` guards
- Index: `idx_paper_positions_open_risk` on `(status, last_risk_check_at) WHERE status='open'`

#### 4. `internal/market/alpaca.go` — `SelectBestContract` redesigned
- New signature: `SelectBestContract(contracts, direction, ContractSelectionOpts)`
- `ContractSelectionOpts`: `DTEMin`, `DTEMax`, `TargetDTE`, `AvoidDTEBelow`, `DeltaMin`, `DeltaMax`
- Hard filters: type, avoid_dte_below, dte range
- Composite score: `dteDist*4.0 + deltaDist*2.0 + spreadPenalty*1.0 + liqPenalty*0.1`
- Closest to target DTE wins when delta is in-band

#### 5. `internal/risk/options.go` (new package)
- `EvaluateMechanicalExit(pos, currentPremium, rules, nowPT)` — pure, no side effects
- Priority: stop loss → take profit → trailing giveback → EOD exit
- EOD cutoff: 12:45 PT; skipped if `hold_overnight_approved=true`
- Exit reason constants: `PREMIUM_STOP_LOSS`, `PREMIUM_TAKE_PROFIT`,
  `PREMIUM_TRAILING_GIVEBACK`, `EOD_EXIT_NO_HOLD_APPROVAL`

#### 6. `internal/store/store.go` — risk-state helpers
- Extended `PaperPosition` struct with risk-state columns
- Added `RiskablePosition` struct (for risk-check queries)
- `GetOpenPositionsForRiskCheck`, `UpdatePositionRiskState`, `SetHoldOvernightApproved`

#### 7. `internal/workflow/activities.go` — 4 changes
- `RunMechanicalRiskCheckActivity`: loads positions → fetches mid prices → evaluates
  mechanical exits → sells → persists risk state
- `runMechanicalChecks(ctx, pool, alpaca, rules, nowPT, source)`: shared helper
- `RunEODPositionReviewActivity`: runs mechanical checks first, asks Claude for hold
  approval, force-exits unapproved positions at EOD
- `RunPositionReviewActivity`: calls `runMechanicalChecks` before Claude review
- `SelectBestContract` calls now use `ContractSelectionOpts` with global risk DTE defaults

#### 8. `internal/workflow/daily.go` — 2 changes
- `DailyPositionReview` calls `RunEODPositionReviewActivity` instead of `RunPositionReviewActivity`
- New `MechanicalRiskCycle` workflow calling `RunMechanicalRiskCheckActivity`

#### 9. `cmd/worker/main.go` — registration + schedule
- Registered `MechanicalRiskCycle`, `RunMechanicalRiskCheckActivity`, `RunEODPositionReviewActivity`
- New schedule `makemytrade-mechanical-risk` with cron `*/10 6-12 * * 1-5`

#### 10. `web/static/app.js` — full risk state in UI
- Shows: entry premium, current premium, P/L%, peak premium, stop level, TP level,
  trailing status with floor, next trigger, hold overnight approval

#### 11. `internal/api/handlers.go`
- `selectBestContract` helper uses `market.ContractSelectionOpts` with global risk DTE defaults

#### 12. Tests
- `internal/risk/options_test.go` — 15 tests for `EvaluateMechanicalExit`
- `internal/market/contract_selector_test.go` — 9 tests for `SelectBestContract`
- All tests pass: `go test ./...` ✅

### Mechanical exit priority (as deployed)
```
every 10 min (06:00–12:59 PT, Mon–Fri):
  for each open position:
    fetch current option mid price
    1. stop loss:          current < entry × (1 - 0.30)  → EXIT
    2. take profit:        current > entry × (1 + 0.50)  → EXIT
    3. trailing activate:  current > entry × (1 + 0.35)  → set peak, set trailing=true
    4. trailing giveback:  trailing && current < peak × (1 - 0.20) → EXIT
    5. EOD (12:45 PT):     not hold_overnight_approved → EXIT
    6. otherwise:          persist updated risk state, continue

12:45 PT (RunEODPositionReviewActivity):
  run mechanical checks first
  for remaining open positions: ask Claude for hold approval
  positions without approval: force exit
```

### Files changed (this refactor)
- `strategy_rules.yaml`
- `internal/strategy/rules.go`
- `migrations/000007_risk_state.up.sql`
- `migrations/000007_risk_state.down.sql`
- `internal/market/alpaca.go`
- `internal/risk/options.go` (new)
- `internal/risk/options_test.go` (new)
- `internal/market/contract_selector_test.go` (new)
- `internal/store/store.go`
- `internal/workflow/activities.go`
- `internal/workflow/daily.go`
- `cmd/worker/main.go`
- `web/static/app.js`
- `internal/api/handlers.go`

### Remaining work
- Run `migrate up` in the deployed environment to apply `000007_risk_state`
- `RunContinuationReviewActivity` does NOT yet run the exhaustion rejection check
  on fresh 6:30→7:45 bars (exhaustion candidates that didn't reject at 6:42 are
  not re-evaluated at 7:45)
- Consider persisting `EvaluateExhaustionRejection` results to `trade_confirmations`
  for auditability (currently only logged)
- `execution.BuyOptionPosition` places Alpaca order BEFORE DB insert; consider
  reversing to DB-first for orphan-prevention

### Exact next step
Apply DB migration in the target environment:
```sh
migrate -path migrations -database $DATABASE_URL up
```
Then restart the worker so the new schedules (`makemytrade-mechanical-risk`) register.

---

## Completed: bearish_exhaustion_reversal Opening Confirmation Pass (2026-04-24)

### Objective
Wire intraday rejection detection for `bearish_exhaustion_reversal` structural candidates
into the opening-confirmation activity so they can be promoted to `entry_ready` and
reach the Claude payload.

### What was done

#### 1. `internal/store/store.go` — new query
- `GetExhaustionReversalStructuralCandidates`: loads `structural_candidate` rows with
  `setup_family='bearish_exhaustion_reversal'` for a given trade date.

#### 2. `internal/strategy/confirmation.go` — new evaluator
- `ExhaustionRejectionInput` / `ExhaustionRejectionResult` structs
- `EvaluateExhaustionRejection`: deterministic, side-effect-free check:
  - Hard block: any bar with lower wick >= 40% of bar range (bullish recovery = no exhaustion)
  - Hard block: no intraday bars available
  - `RejectionConfirmed = (VWAPBreak || ORMidBreak) && RelVolumeOK && MarketNotBullish`

#### 3. `internal/workflow/activities.go` — two edits in `RunOpeningConfirmationActivity`
- Load exhaustion structural candidates, add their tickers to batch
- After score-based shortlisting: run `EvaluateExhaustionRejection` for each;
  if `RejectionConfirmed`, promote `structural_candidate → entry_ready`

### Files changed
- `internal/store/store.go`
- `internal/strategy/confirmation.go`
- `internal/workflow/activities.go`

---

## Completed: bearish_exhaustion_reversal Family (2026-04-24)

### Objective
Add a new optional PUT-only setup family for overextended bullish names near exhaustion.

### Files changed
- `strategy_rules.yaml`
- `internal/strategy/rules.go`
- `internal/strategy/engine.go`
- `internal/store/store.go`
- `internal/strategy/confirmation.go`
- `internal/workflow/activities.go`

---

## Completed: Trading-Day Schedule Fix (2026-04-24)

### Objective
Fix schedule so opening confirmation fires at 6:42 PT.

### Autonomous trading day (as deployed)
```
06:25 PT  DailyResearchCycle         — overnight scan, classify candidates
06:42 PT  OpeningConfirmationCycle   — 6:30-6:40 candle, Claude entry (cutoff 6:55)
07:15 PT  FirstPositionReviewCycle   — early risk management
07:45 PT  ContinuationReviewCycle    — fresh intraday bars, position tighten/exit
every 10m MechanicalRiskCycle        — hard stops, TP, trailing, EOD exit
12:45 PT  DailyPositionReview        — EOD: Claude hold approval or force exit
Sunday 07:00 PT  WeeklyReviewCycle   — performance review + tuning proposals
```

### Files changed
- `strategy_rules.yaml`
- `internal/strategy/rules.go`
- `cmd/worker/main.go`
- `internal/workflow/daily.go`
- `internal/workflow/activities.go`
