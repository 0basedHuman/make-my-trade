# Make-My-Trade — Claude Code Working Guide

## Project intent

This repository is an **options-first paper-trading decision engine** for short-horizon long options on liquid US equities and index ETFs.

The system evaluates market data, regime context, option-chain quality, and bounded AI review to decide whether any ticker qualifies for a paper trade in:

- long calls
- long puts

Target trade duration:
- typically 7–14 DTE
- intraday confirmation required before final trade activation where applicable

This repository is **not** for:
- live trading
- share trading
- debit spreads or multi-leg execution
- naked option selling
- discretionary freeform narrative trading
- strategy logic living primarily in prompts

## Product boundary

This engine must remain:

- paper trading only
- long calls and long puts only
- setup-first, options-second
- deterministic preprocessing before model review
- schema-bounded output
- YAML-driven strategy
- conservative about weak setups
- able to emit valid no-trade days

No human confirmation is required for **paper trades**.
Human confirmation is still required for any future live execution system.

## Source-of-truth order

When changing behavior, use this order:

1. `strategy_rules.yaml`
2. `decision_schema.json`
3. `runtime_payload.json`
4. `prompts/decision_prompt.md`
5. `CLAUDE.md`

If strategy behavior changes, update YAML first.

## Docs to read by task type

When resuming work:
- read `docs/current_refactor_status.md`

When working on weekly review, paper-trade performance analysis, or tuning proposals:
- read `docs/weekly_review_protocol.md`

## Current architecture mental model

Treat this app as a layered pipeline:

1. deterministic data fetch and preprocessing
2. deterministic setup classification
3. option-chain quality filtering
4. bounded Claude review
5. lifecycle state persistence
6. deterministic confirmation promotion
7. paper position creation and management
8. weekly review and tuning proposals

## Required lifecycle states

The current candidate lifecycle states are:

- `rejected`
- `structural_candidate`
- `entry_ready`
- `confirmed`
- `watch_only`
- `blocked_by_event`

Interpretation:

- `rejected`
  - fails quality requirements

- `structural_candidate`
  - watchlist only
  - not tradable
  - no final options output

- `entry_ready`
  - pre-open candidate
  - near tradable
  - still not final

- `confirmed`
  - first state allowed to become a real paper-trade output

- `watch_only`
  - failed confirmation or otherwise downgraded after being close

- `blocked_by_event`
  - valid structure but operationally blocked by earnings or binary event risk

## Critical behavior rules

Always enforce these rules:

- Structural candidates are watchlist only.
- Event-blackout names must not look tradable.
- Only confirmed setups may produce final options output.
- Chain quality must be checked before final option output.
- Paper trades require no human approval once the setup is confirmed.
- Deterministic confirmation and lifecycle transitions must remain code-driven.
- Claude is a bounded reviewer and explainer, not the primary strategy engine.
- Prompts must not replace YAML-defined strategy behavior.
- Do not invent missing values.
- Do not force trades on empty days.
- Do not auto-tune strategy rules directly from trade PnL.

## Autonomous paper-trading requirement

The app must be able to run for one month without human intervention.

That means the system must support this automated flow:

1. Analysis
   - run deterministic daily analysis
   - fetch chain data
   - build runtime payload
   - send bounded payload to Claude reviewer
   - persist candidate rows

2. Confirmation
   - evaluate deterministic opening confirmation
   - promote `entry_ready -> confirmed`
   - or demote to `watch_only`

3. Auto paper entry
   - when a candidate becomes `confirmed`, automatically create a paper position
   - no human approval required
   - operation must be idempotent

4. Daily monitoring
   - review each open paper position daily
   - decide:
     - HOLD
     - TIGHTEN_STOP
     - TAKE_PROFIT
     - EXIT
     - WATCH_CLOSELY

5. Weekly review
   - aggregate one week of paper-trade outcomes
   - summarize:
     - confirmed trades
     - exits
     - false positives
     - false negatives
     - regime fit
     - setup-family performance
   - propose explicit strategy updates
   - do not auto-apply them

## Paper-trade rules

For paper trading:

- no user confirmation is required
- confirmed setups should automatically create paper positions
- duplicate entries must be prevented
- while a paper position is open for a ticker, new candidate suggestions for the same ticker should be suppressed or clearly blocked
- all paper trade actions must be persisted in:
  - `paper_positions`
  - `paper_position_events`

## Claude’s role

Claude is a bounded reviewer, not the primary strategy engine.

Claude may:
- review strongest candidates
- compare candidate quality
- explain trade rationale
- explain rejections
- review open paper positions daily
- generate weekly review summaries
- propose explicit tuning ideas

Claude must not:
- invent hard strategy rules outside YAML
- override chain-quality hard failures
- override deterministic confirmation or lifecycle transitions
- invent trades when no-trade conditions apply
- auto-train itself from PnL
- silently mutate strategy behavior

## Strategy tuning policy

Use **weekly review**, not autonomous self-learning.

Allowed:
- summarize performance by setup family
- propose threshold changes
- propose target-model adjustments
- propose confirmation strictness adjustments
- propose chain-quality threshold adjustments

Not allowed:
- automatic fine-tuning from paper PnL
- automatic model retraining
- automatic YAML mutation without explicit review

## Workflow discipline

You must minimize token waste.

Required behavior:
- Prefer reading repo `CLAUDE.md` first.
- If `docs/current_refactor_status.md` exists, read it before broad repo scans.
- Do not repeatedly reread large files.
- Prefer narrow edits over broad rewrites.
- Before large edits, summarize:
  1. exact files to inspect
  2. exact files to change
  3. exact next step
- Before ending a session, update `docs/current_refactor_status.md`.

## Required checkpoint file

Maintain:
- `docs/current_refactor_status.md`

It must contain:
- current objective
- completed work
- files changed
- remaining work
- exact next step

## Engineering rule

If YAML changes but runtime behavior does not, assume the code is still using old logic.
Debug parsing, status assignment, persistence, workflow execution, and UI rendering before changing strategy again.

## Maintenance order

If behavior changes, update files in this order:

1. strategy intent docs if architecture intent changes
2. `strategy_rules.yaml` if rule behavior changes
3. `decision_schema.json` if output contract changes
4. `prompts/decision_prompt.md` if reviewer orchestration changes
5. Go execution paths
6. UI rendering assumptions
7. `CLAUDE.md` only when repo-wide behavior changes

## Definition of done for this phase

This phase is done only when:

- daily analysis runs without manual repo editing
- confirmation runs without manual approval
- confirmed setups create paper positions automatically
- open paper positions are reviewed daily
- new trades for already-open tickers are suppressed
- weekly review summary is generated
- one month of autonomous paper trading is operational

---

## RSVE-O Strategy Summary

RSVE-O (Relative Strength Volatility Expansion — Options) is the **sole gate engine** as of 2026-05-06.
13 binary gates per branch. **All must pass** or ticker is rejected. Score (0-100) ranks survivors only — never a gate.

### Bullish branch gates (in order)
```
1.  vix_regime          VIX < 24
2.  market_uptrend      SPY close > SPY EMA50
3.  no_earnings         earnings > 5 days away
4.  relative_strength   20d RS > +2% vs SPY
5.  ema_structure       EMA20 > EMA50
6.  close_above_ema20   close > EMA20
7.  volume_expansion    RVOL >= 1.2x
8.  vol_squeeze         BB width percentile <= 30% (63d range)
9.  breakout_trigger    close > highest high (prior 20 bars)
10. rsi_range           RSI 40-78
11. iv_rank_ok          IV rank < 70   (skip if -1)
12. spread_quality      spread < 10%   (skip if -1)
13. oi_minimum          OI >= 100      (skip if -1)
```

Bearish branch mirrors with: `market_downtrend`, `relative_weakness`, `close_below_ema20`, `breakdown_trigger`.

Source of truth: `strategy_rules.yaml` → `rsve:` block
Implementation: `internal/strategy/rsve.go`
Tests: `internal/strategy/rsve_test.go`

### Gate-to-DB column mapping (legacy boolean columns)
| RSVE gate | DB column |
|-----------|-----------|
| vix_regime | GateVIX |
| market_uptrend / market_downtrend | GateTrend |
| volume_expansion | GateVolume |
| vol_squeeze | GateMomentum |
| rsi_range | GateRSI |
| relative_strength / relative_weakness | GateBTC |

---

## Daily Workflow Timeline

```
06:25 PT  DailyResearchCycle         RSVE gates all tickers → entry_ready or rejected
06:42 PT  OpeningConfirmationCycle   deterministic signals ≥ 3/5 → confirmed; else → watch_only
07:15 PT  FirstPositionReviewCycle   Claude reviews open positions: HOLD/TIGHTEN/EXIT
07:45 PT  ContinuationReviewCycle    fresh intraday bars, position tighten/exit
every 10m MechanicalRiskCycle        stop 0.70x, TP 1.50x, trailing 1.35x/0.80x, EOD exit 12:45
12:45 PT  DailyPositionReview        Claude EOD hold approval or force exit
Sun 07:00 WeeklyReviewCycle          performance summary + tuning proposals (NOT auto-applied)
```

Confirmation type logged: `rsve_deterministic` (Claude removed from confirmation path 2026-05-06).

---

## Infrastructure Component Status

| Component | Status | File / Notes |
|-----------|--------|--------------|
| Temporal worker | ✅ | `cmd/worker/main.go` |
| API server | ✅ | `cmd/server/main.go` |
| PostgreSQL + TimescaleDB | ✅ | Docker; migrations in `migrations/` |
| Redis | ✅ | Docker |
| Alpaca market data | ✅ | `internal/market/alpaca.go` |
| RSVE gate engine | ✅ | `internal/strategy/rsve.go` |
| Daily analysis pipeline | ✅ | `activities.go` → `RunDailyAnalysisActivity` |
| Opening confirmation | ✅ deterministic | Claude removed 2026-05-06 |
| Mechanical exits | ✅ | `MechanicalRiskCycle` every 10 min |
| Position review (daily) | ✅ Claude | `RunDailyPositionReviewActivity` |
| Weekly review command | ⬜ not yet | `cmd/weeklyreview/` — not written |
| Backtest | ✅ RSVE-based | `cmd/backtest/main.go` |

---

## Context Management

### Files to read on every session start (in order)
1. `CLAUDE.md` — this file (boundaries, rules, strategy summary)
2. `docs/checkpoint.md` — what was in progress at last compaction or save
3. `docs/history.md` — full session history table; see what was built and why
4. `docs/current_refactor_status.md` — latest completed work and exact next step

**Do not scan the repo broadly until you have read all four files above.**

### checkpoint.md rules
- Location: `docs/checkpoint.md`
- **On each new session start:** overwrite with a clean snapshot of current state before doing any work.
- Contents: current task, files being changed, build status, exact next step, open questions.
- **Auto-save at ~70% context:** proactively update `docs/checkpoint.md` mid-session without being asked.
- **At 95% token exhaustion (hourly or weekly limit):** APPEND to `docs/checkpoint.md` — do NOT overwrite.
  Use a `---` separator with a timestamp so previous checkpoint is preserved as fallback.
  This captures the last known-good progress before tokens run out.

### history.md rules
- Location: `docs/history.md`
- **At end of each session:** APPEND a new row to the table — never overwrite existing rows.
- Compress "User Prompt" to ≤15 words. Keep "Claude Achieved" factual and ≤20 words.
- This is the project's institutional memory. Read it when you need to understand why something was built.
- Do not summarize or reformat old rows — only add new ones.