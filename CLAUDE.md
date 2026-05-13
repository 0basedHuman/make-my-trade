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

## Current architecture mental model

Treat this app as a layered pipeline:

1. deterministic data fetch and preprocessing
2. deterministic RSVE gate evaluation (13 binary gates)
3. option-chain quality filtering + quote-realistic fill model
4. lifecycle state persistence
5. deterministic confirmation promotion (required + optional signals)
6. paper position creation and management (auto, no approval needed)
7. mechanical exit management (stop/TP/trail/time-stop/structure_invalidation)

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
- propose explicit tuning ideas when asked

Claude must not:
- invent hard strategy rules outside YAML
- override chain-quality hard failures
- override deterministic confirmation or lifecycle transitions
- invent trades when no-trade conditions apply
- auto-train itself from PnL
- silently mutate strategy behavior

## Strategy tuning policy

Manual review only — no autonomous self-learning.

Allowed:
- summarize performance by setup family
- propose threshold changes
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
- open paper positions are reviewed daily (mechanical exits every 10m + EOD check)
- new trades for already-open tickers are suppressed
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

## Daily Workflow Timeline (4 active Temporal schedules)

```
06:45 PT  DailyResearchCycle         RSVE 13-gate evaluation → entry_ready or rejected
07:00 PT  OpeningConfirmationCycle   required+optional signals → confirmed or watch_only; auto paper entry
every 10m MechanicalRiskCycle        stop/TP/trail/time-stop/structure_invalidation
12:45 PT  DailyPositionReview        EOD mechanical check + overnight hold log
```

Confirmation type logged: `rsve_deterministic`. No Claude in any trading path.

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
| Position review (EOD) | ✅ mechanical | `RunDailyPositionReviewActivity` — no Claude |
| Walk-forward research | ✅ | `cmd/wfresearch/main.go` |
| Backtest | ✅ RSVE-based | `cmd/backtest/main.go` |

---

## Context Management

### Files to read on every session start (in order) — MANDATORY

**CRITICAL: Do not do ANY work until all four files have been read. No exceptions.**

1. `CLAUDE.md` — this file
2. `docs/checkpoint.md` — what was running and what changed last session
3. `docs/history.md` — full session log; why things were built
4. `docs/current_refactor_status.md` — completed work + exact next step

These files ARE the project's state. Skipping them means working from stale context and breaking things that were already fixed.

### Files to update at every session END — MANDATORY

**CRITICAL: Never end a session without updating all three. This is how future sessions know what happened.**

1. `docs/checkpoint.md` — OVERWRITE entirely with: what is running, what changed, exact next step
2. `docs/history.md` — APPEND one new row (user prompt ≤15 words, achieved ≤20 words); never rewrite old rows
3. `docs/current_refactor_status.md` — PREPEND new completed section at top

**Also update at ~70% context** — do not wait for session end.

### What breaks if these rules are skipped

This happened 2026-05-07 to 2026-05-11 (5 sessions with no updates):
- strategy_rules.yaml accumulated 14k chars of dead deprecated config
- CLAUDE.md referenced weekly review, removed cycles, wrong timeline
- weekly_review_protocol.md existed for a feature that was never built
- Strategy printout was wrong; architecture model was wrong
- Required a full manual audit to repair

### checkpoint.md format
- OVERWRITE (not append) at session start and ~70% context
- Contents: what is running (PIDs, schema version), what changed this session, build status, exact next step, open items
- At 95% token exhaustion: APPEND with `---` separator to preserve last-known-good state

### history.md format
- APPEND only — one new row per session
- Compress "User Prompt" to ≤15 words
- "Claude Achieved" factual, ≤20 words