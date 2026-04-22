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

When working on architecture or migration:
- read `docs/refactor_to_options_engine.md`

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