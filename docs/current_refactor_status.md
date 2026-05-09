# Current Refactor Status

## Completed: RSVE-O PRO Phase 3 — Claude Removal + Structure Invalidation (2026-05-09)

### Objective
Complete removal of all Claude references from trading paths; add structure_invalidation exit rule; remove FirstPositionReviewCycle and ContinuationReviewCycle.

### All requirements completed

#### 1. Fix `RunOpeningConfirmationActivity` (activities.go)
- Removed `ccfg := rules.ClaudeConfirmation` — no longer pulls Claude config block
- Auto-reject is now always a hard block (removed `ccfg.DeterministicAutoRejectIsHardBlock` conditional)
- Log `"contract_selected_before_claude"` → `"contract_selected"`
- Removed `softMin` (old 3-of-5 signals count) — replaced with `ev.result.Status != "confirmed"`
- Comment updated: removed "BEFORE Claude call" language

#### 2. Remove `FirstPositionReviewCycle` and `ContinuationReviewCycle` (cmd/worker/main.go)
- Removed workflow registrations for both cycles
- Removed activity registrations for `RunPositionReviewActivity` and `RunContinuationReviewActivity`
- Removed both schedule entries from `registerSchedules()`
- Comment updated: 6 schedules → 4 schedules

#### 3. Add `structure_invalidation` exit rule (risk/options.go)
- Added `ExitReasonStructureInvalidation = "STRUCTURE_INVALIDATION"` constant
- Added `Direction`, `BreakoutLevel`, `UnderlyingClose` fields to `PositionRiskState`
- Rule #6: bullish → exit when underlying < BreakoutLevel; bearish → exit when underlying > BreakoutLevel
- Rule skips when `UnderlyingClose == 0` (caller didn't populate; existing behavior preserved)
- Premium stop loss / TP / trail still have priority (evaluated first in rule order)

#### 4. Wire structure invalidation into `runMechanicalChecks` (activities.go)
- Fetches underlying quote alongside option mid-price (best-effort, non-blocking)
- Populates `Direction` from `p.OptionType` ("call"→"bullish", "put"→"bearish")
- `BreakoutLevel = p.EntryPrice` (underlying at entry ≈ breakout level)
- Added `FetchLatestQuote` to `execution.Alpaca` interface

#### 5. Set `claude_confirmation.enabled: false` in strategy_rules.yaml
- YAML had `enabled: true` — now `false`; compliance test now passes

#### 6. New tests
- `risk/options_test.go`: 5 structure invalidation tests (bullish fires, bearish fires, holds above, zero-skip, stop has priority)
- `strategy/phase2_compliance_test.go`: 2 new tests (claude_confirmation not used as gate, confirmation status determines outcome)

### Build / test status
- `go build ./...` ✅
- `go test ./...` ✅ (all pass)

### Files changed
- `internal/workflow/activities.go`
- `cmd/worker/main.go`
- `internal/workflow/daily.go`
- `internal/risk/options.go`
- `internal/risk/options_test.go`
- `internal/execution/options.go`
- `internal/strategy/phase2_compliance_test.go`
- `strategy_rules.yaml`

### Remaining work
1. `docs/rsve_strategy.md` — architecture reference doc (not blocking)
2. Dead code cleanup: `engine.go` BTC/RSI fields, `store.SetHoldOvernightApproved` (never called now)
3. `FirstPositionReviewCycle` and `ContinuationReviewCycle` workflow functions in `daily.go` — dead code, safe to delete when convenient

### Active schedule (4 cycles)
- `06:25 PT` DailyResearchCycle — overnight RSVE scan
- `06:42 PT` OpeningConfirmationCycle — deterministic confirmation, auto paper entry
- `every 10m` MechanicalRiskCycle — stop/TP/trail/time-stop/structure_invalidation
- `12:45 PT` DailyPositionReview — EOD mechanical check + overnight hold log

---

## Completed: RSVE-O PRO Phase 2 Refactor (2026-05-09)

### Objective
5 targeted behavioral changes: legacy YAML cleanup, tighten opening confirmation, remove forced EOD exit, remove min-hold rule, move pattern analysis before breakout gate.

### All requirements completed

#### 1. Legacy YAML cleanup
- `deprecated_legacy_strategy_families:` wrapper added to `strategy_rules.yaml` with `enabled: false`
- `rules.go`: `DeprecatedLegacyFamiliesConfig` struct added; `Rules.Families` now `yaml:"-"` (not parsed from YAML); `LoadRules` only populates `Families` if `enabled=true`
- Result: `Rules.Families` is nil at runtime → `engine.go` legacy family scoring is unreachable

#### 2. Tighten opening confirmation
- `rules.go`: Added `ConfirmationRequiredChecks` struct and `RequiredChecks`/`MinOptionalSignals` fields to `OpenConfirmationConfig`
- `confirmation.go`: Added `ConfirmationDiagnostics` struct; new required+optional logic: `level_holds` AND `market_aligned` both required, then ≥1 of `volume`/`no_rejection_wick`/`opening_range_midpoint` must pass
- `strategy_rules.yaml`: Added `required_checks:` and `min_optional_signals: 1` under `open_confirmation:`
- Legacy mode preserved when `RequiredChecks` not set

#### 3. Remove forced EOD exit
- `risk/options.go`: Removed `ExitReasonEODNoHoldApproval`, `HoldOvernightApproved` from `PositionRiskState`, EOD exit rule 6, `ForceEODExitUnlessHoldConfirmed` from `MechanicalExitsConfig`
- `activities.go` `RunEODPositionReviewActivity`: simplified to run mechanical checks + log overnight holds; no forced exits
- `strategy_rules.yaml`: Removed `force_eod_exit_unless_hold_confirmed: true`
- Result: 21-45 DTE swings hold overnight by default; only mechanical invalidation causes same-day exits

#### 4. Remove min-hold rule
- `risk/options.go`: Removed `inProtectedWindow` logic; all `!inProtectedWindow &&` guards removed; stop loss now fires on day 0 for hard invalidations
- `rules.go`/`strategy_rules.yaml`: Removed `MinHoldDays`/`min_hold_days`

#### 5. Move pattern analysis before breakout gate (rsve.go)
- `AnalyzePatterns` call moved from after gate 8 to between gate 7 (vol_squeeze) and gate 8 (breakout_trigger)
- `PatternAnalysis.PatternState` field added to `patterns.go`
- After gate 8, `PatternState` classified: `"no_pattern"` / `"pattern_forming"` (detected, breakout not yet confirmed) / `"pattern_breakout_confirmed"` (detected AND breakout passed)

### Build / test status
- `go build ./...` ✅
- `go test ./...` ✅ (all pass)

### New tests added
- `confirmation_test.go` (4 tests: required pass + optional, level_holds fails, market_aligned fails, both required no optionals)
- `phase2_compliance_test.go` (6 tests: legacy families disabled in YAML, RSVE config loads, PatternState no_pattern, breakout confirmed, option unavailable, no min-hold config)
- `risk/options_test.go` (3 new tests replacing 3 EOD tests: overnight holds by default, same-day stop fires, same-day trailing fires, same-day valid holds)

### Files changed
- `internal/strategy/rules.go`
- `internal/strategy/confirmation.go`
- `internal/strategy/patterns.go`
- `internal/strategy/rsve.go`
- `internal/strategy/confirmation_test.go` (new)
- `internal/strategy/phase2_compliance_test.go` (new)
- `internal/risk/options.go`
- `internal/risk/options_test.go`
- `internal/workflow/activities.go`
- `strategy_rules.yaml`

### Remaining work
1. Layer 2 intraday VWAP confirmation in `RunOpeningConfirmationActivity` (uses `AnalyzeORBPattern` with 5m bars)
2. `docs/rsve_strategy.md` — pattern analysis reference doc (not blocking)
3. Dead code cleanup: `engine.go` BTC/RSI fields, `store.SetHoldOvernightApproved` can be deprecated

### Guardrails verified preserved
- Paper only, no Claude in automated paths ✅
- Score ranking-only (never a gate) ✅
- DTE 21-45, target 30 ✅
- All 12 RSVE gates intact ✅
- `go build ./...` and `go test ./...` clean ✅

---

## Completed: RSVE-O PRO Refactor (2026-05-07)

### Objective
Three-layer PRO refactor: new 4-status enum, option gate behavior change (-1 → stock_signal_passed not auto-pass), updated PRO score weights, 2 new patterns, activities.go status mapping.

### All requirements completed

#### 1. New 4-status enum in rsve.go
- `rejected` — stock gates failed
- `stock_signal_passed` — stock gates pass, option data unavailable (any -1)
- `option_blocked` — stock gates pass, option data present but fails
- `paper_trade_created` — all 12 gates pass including options → AllPass=true

#### 2. Option gate behavior change
- `-1` option fields: early exit as `stock_signal_passed`, option gates not evaluated
- When all option data present: gates 9-12 run with separate `optBlocked` tracking
- `EvaluateRSVE` branch selection now uses `statusRank` (paper_trade_created=3 > stock_signal_passed=2 > option_blocked=1 > rejected=0)

#### 3. PRO score weights (computeRSVERankScore)
- RS: 0-25 (was 0-30)
- Volatility compression (BB): 0-20 (was 0-25)
- Breakout strength: 0-20 (NEW — measures close vs pivot %)
- Volume expansion: 0-15 (was 0-25)
- Pattern quality: 0-15 (unchanged)
- Options liquidity: 0-5 (NEW — only when all option data present)

#### 4. Two new patterns in patterns.go
- `support_resistance_retest` (bullish + bearish) — daily bars, retests broken S/R level
- `AnalyzeORBPattern` — standalone function for 5m intraday bars (Opening Range Breakout)
- `support_resistance_retest` wired into `AnalyzePatterns` dispatcher
- Added to `strategy_rules.yaml` allowed_patterns list

#### 5. activities.go status mapping
- `paper_trade_created` → `entry_ready` (goes through intraday confirmation)
- `stock_signal_passed` → `stock_signal_passed` (monitoring only)
- `option_blocked` → `option_blocked` (monitoring only)
- Log format updated with new status counts

#### 6. store.go upsert protection
- Status protection list expanded: added `stock_signal_passed`, `option_blocked`

#### 7. Tests updated (all pass)
- `makeValidBullishInput()`: now provides valid option data by default (IVRank=50, Spread=5%, OI=1000, OptionVol=100)
- All "confirmed" status assertions → "paper_trade_created"
- `TestPattern_OptionDataUnavailable_StockGatesPass`: updated for `stock_signal_passed` behavior
- `TestCompliance_RejectedCandidate_HasFullDiagnostics`: expects 8 gates (stock-blocked)
- `TestRSVE_DiagnosticsComplete`: uses permissive config to get 12 gates

### Build / test status
- `go build ./...` ✅
- `go test ./...` ✅ (all pass)

### Files changed this session
- `internal/strategy/rsve.go` (new 4-status enum, option gate split, PRO score weights)
- `internal/strategy/patterns.go` (support_resistance_retest + AnalyzeORBPattern)
- `internal/strategy/rsve_test.go` (updated makeValidBullishInput, status assertions)
- `internal/strategy/rsve_compliance_test.go` (status assertions, gate count)
- `internal/strategy/patterns_test.go` (updated option unavailability test)
- `internal/workflow/activities.go` (new status mapping, log format)
- `internal/store/store.go` (expanded status protection in upsert)
- `strategy_rules.yaml` (added support_resistance_retest to allowed_patterns)

### Remaining work
1. Layer 2 intraday VWAP confirmation in `RunOpeningConfirmationActivity` (uses `AnalyzeORBPattern` with 5m bars)
2. DB migration for new status values (optional — they're VARCHAR strings, schema-compatible)
3. `cmd/dryscan` output: show `stock_signal_passed` count in summary
4. `docs/rsve_strategy.md` — pattern analysis reference doc (not blocking)

### Exact next step
Layer 2 intraday confirmation: add `AnalyzeORBPattern` + VWAP check to `RunOpeningConfirmationActivity` in activities.go. Uses `FetchOpening5MinBars` (already in alpaca.go) and `FetchIntradayBars` for VWAP. This converts `stock_signal_passed` candidates that have good opening range structure into `entry_ready` (allowing confirmation to `paper_trade_created`).

---

## Completed: RSVE-O Mandatory Refactor (2026-05-07)

### Objective
Apply all 10 mandatory RSVE-O corrections: remove Claude from all trading paths, 21-45 DTE, 8-gate architecture, rename gate_btc, updated liquidity thresholds, score ranking-only, new mechanical exits, compliance tests.

### All requirements completed

#### 1. Removed Claude from ALL trading-path workflows
- `RunDailyAnalysisActivity`: Removed BTC ROC fetch, shortlisting, opening bars fetch, Claude `DecideOptions` call. Now counts entry_ready results directly from RSVE.
- `RunPositionReviewActivity`: Removed Claude position review. All positions default to HOLD; mechanical exits handle hard cases.
- `RunEODPositionReviewActivity`: Removed Claude hold approval. Replaced with deterministic: auto-hold if `days_held < max_hold_days (5)`, else force exit.
- `RunContinuationReviewActivity`: Delegates to RunPositionReviewActivity (inherits fix).
- `RunOpeningConfirmationActivity`: Already deterministic from previous session.
- Claude retained only for `RunWeeklyReviewActivity` (offline, not a trading path).

#### 2. DTE range: 21-45, target 30
- `strategy_rules.yaml`: `rsve.options.dte_min=21`, `risk.option_lifecycle.dte_min=21`, `avoid_dte_below=21`
- `rules.go` `DefaultRSVEConfig()` and `DefaultRules()` updated to match.
- Compliance tests: `TestCompliance_DTE_*` (3 tests)

#### 3. 8 logical gate groups, 12 binary checks
- `rsve.go`: Removed `gateCloseVsEMA20` (old gate 6) and `gateRSIRange` (old gate 10)
- Added `gateOptionVolume` as gate 12
- `RSVEBranchConfig`: Removed `RSIMin`/`RSIMax` fields
- YAML comment updated to reflect 8 groups

#### 4. Removed BTC logic entirely
- `strategy_rules.yaml`: `btc_roc20_min=-999` (disabled), removed `btc_regime_not_negative` from all family preconditions
- `handlers.go`: Removed BTC ROC fetch, `btcROC := 0.0`
- `activities.go`: Removed BTC ROC fetch, `btcROC := 0.0`
- `engine.go`: BTC gate still in code but disabled via YAML (future cleanup)

#### 5. Updated liquidity thresholds
- OI >= 500 (was 100), option volume >= 50 (new gate), spread <= 8% (was 10%), IV rank <= 70 (boundary fix: was `<`, now `<=`)
- Updated in: `rsve.go`, `rules.go`, `strategy_rules.yaml`, `store.go`, `handlers.go`

#### 6. Removed unsupported performance claims
- No "+5.3% expectancy" claims anywhere in code

#### 7. Score is ranking-only
- `rsve.go`: Score components are RS magnitude (0-30), BB squeeze (0-25), volume ratio (0-25), EMA gap (0-20)
- Status is always "confirmed" | "rejected" — score never gates
- Compliance tests: `TestCompliance_Score_*`

#### 8. gate_btc → gate_relative_strength rename
- `store.go`: `GateBTC` → `GateRelativeStrength` in struct + SQL
- `handlers.go`: Updated variable and struct field
- `activities.go`: Updated struct field
- Migration: `migrations/000009_rename_gate_btc.up.sql`

#### 9. New mechanical exit rules
- `risk/options.go`: Stop 25% (was 30%), TP 70% (was 50%), trailing start 35% (was 8%), trailing giveback 10%
- Added: `ExitReasonTimeStop` — exits after `time_stop_days (2)` if no trailing activated (breakout failed)
- Added: `ExitReasonMaxHoldDays` — hard time limit at `max_hold_days (5)`
- `strategy_rules.yaml` and `rules.go` `MechanicalExitsConfig` updated with `time_stop_days: 2`

#### 10. Compliance tests
- `internal/strategy/rsve_compliance_test.go` (15 tests, all pass):
  - DTE 21/45/30 enforced
  - OI < 500 rejects
  - Option volume < 50 rejects
  - Spread > 8% rejects
  - IV rank > 70 rejects
  - Boundary values (exactly 500, 50, 8%, 70) pass
  - Score ranking-only
  - Rejected candidates emit full 12-gate diagnostics

### Build / test status
- `go build ./...` ✅
- `go test ./...` ✅ (all pass: indicators, market, risk, strategy)

### Files changed this session
- `internal/strategy/rsve.go` (rewrite: 8 gate groups, removed RSI/close_vs_ema20, added option_volume)
- `internal/strategy/rules.go` (DTEMin=21, new thresholds, MinOptionVolume, TimeStopDays, removed RSIMin/RSIMax from branch config)
- `internal/strategy/rsve_test.go` (fixed RSIMax references, OptionVolume=-1 in helper, gate count 13→12)
- `internal/strategy/rsve_compliance_test.go` (new — 15 compliance tests)
- `strategy_rules.yaml` (DTE, thresholds, BTC disabled, removed RSI/close_vs_ema20 gates, new mechanical exits)
- `internal/workflow/activities.go` (remove Claude from 3 activities, BTC ROC → 0, GateBTC → GateRelativeStrength)
- `internal/api/handlers.go` (BTC ROC → 0, GateBTC → GateRelativeStrength, gateRSI → false)
- `internal/risk/options.go` (new exit rules: 25%/70%/35%, TimeStop, MaxHoldDays)
- `internal/store/store.go` (GateBTC → GateRelativeStrength in struct + all SQL)
- `migrations/000009_rename_gate_btc.up.sql` (new)
- `migrations/000009_rename_gate_btc.down.sql` (new)

### Deployed: v13 (2026-05-07 08:34 PT)
- Migration 000009 applied: `gate_btc` → `gate_relative_strength` ✅
- Server running (PID in logs/server.pid): `schema at version 9 — all migrations applied` ✅
- Worker running (PID in logs/worker.pid): all 7 Temporal schedules updated ✅
- Health: `{"db":"ok","redis":"ok","status":"ok"}` ✅

### Remaining work
1. Future cleanup (not blocking): remove dead `BTCRoc20Min`, `BTCNotNegative`, `GateRSI` from `engine.go`

### Exact next step
No critical work remaining. Optional cleanup:
- Remove dead BTC/RSI fields from `engine.go`
- Write `docs/rsve_strategy.md` (pattern analysis reference doc)

---

## Completed: Chart Pattern Analysis — Bull/Bear Flag, Triangles, Base Breakout (2026-05-07)

### Objective
Add structural chart-pattern detection as a ranking bonus to RSVE-O.

### All requirements completed

#### New files
- `internal/strategy/patterns.go` — `AnalyzePatterns()`, 6 pattern detectors (bull_flag, bear_flag, ascending_triangle, descending_triangle, base_breakout, base_breakdown), `PatternResult`, `PatternAnalysis` types
- `internal/strategy/patterns_test.go` — 25 tests (all pass)

#### Modified files
- `strategy_rules.yaml` — added `pattern_analysis:` subsection under `rsve:`
- `internal/strategy/rules.go` — added `PatternAnalysisConfig` struct + field in `RSVEConfig`, updated `DefaultRSVEConfig()`
- `internal/strategy/rsve.go` — `RSVEResult.PatternAnalysis` field, `gatePatternRequired()`, `AnalyzePatterns()` call in `evaluateBranch()`, `computeRSVERankScore()` accepts pattern context, score bonus capped at 100

#### Architecture
- Pattern analysis runs after stock gate 8 (breakout_trigger), before option gates
- `required_for_trade: false` (default) → ranking boost only; pattern gate never emitted
- `required_for_trade: true` → `pattern_required` gate added between stock and option gates
- Score bonus: `pa.BestPattern.QualityScore * cfg.PatternScoreWeight` (default 15 pts max), capped at 100

### Build / test status
- `go build ./...` ✅
- `go test ./...` ✅ (all 25 pattern tests + all compliance tests pass)

---

## Completed: Wire RSVE — dryscan + rsve.go (2026-05-05)
[see checkpoint.md for details]

## Completed: Market Signal Fixes — Chain-Based P/C Ratios (2026-04-28)
## Completed: Mechanical Exits + 7-14 DTE Enforcement (2026-04-24)
## Completed: bearish_exhaustion_reversal + Opening Confirmation (2026-04-24)
## Completed: Trading-Day Schedule Fix (2026-04-24)
