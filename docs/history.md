# Project History

Claude reads this file to understand **why** things were built, what was tried, and what was decided.
Rules: append only — never overwrite rows. Compress "User Prompt" to ≤15 words.

---

## Session Log

| Date | User Prompt (compressed) | Claude Achieved | Finished | Next Steps |
|------|--------------------------|-----------------|----------|------------|
| 2026-04-22 | Build options paper-trading engine from scratch | Project scaffolding: Go + Temporal + Postgres/TimescaleDB + Redis + Claude API; core domain types, DB schema, workflow skeleton | Y | Build trading pipeline stages |
| 2026-04-24 | Fix trading-day schedule — correct PT times and cron wiring | 7 scheduled workflows wired in worker; corrected cron times to PT; `internal/workflow/daily.go` restructured | Y | Mechanical exits |
| 2026-04-24 | Add mechanical exits and 7–14 DTE enforcement | Stop 0.70x / TP 1.50x / trailing 1.35x-0.80x every 10 min; DTE filter; `migrations/000007_risk_state`; `internal/risk/options.go` | Y | Bearish setup support |
| 2026-04-24 | Add bearish_exhaustion_reversal setup + opening confirmation | Bearish setup family; deterministic opening confirmation (signals soft-min); lifecycle promoted entry_ready→confirmed | Y | Market signal fixes |
| 2026-04-28 | Fix market signals — chain-based P/C ratios wrong | Alpaca chain P/C ratio rewrite; signal collection rework in `activities.go`; FINRA/CBOE/Yahoo market data added | Y | RSVE gate engine |
| 2026-05-05 | Build RSVE-O 13-gate engine; validate via dryscan | `internal/strategy/rsve.go` (13 gates, both branches); `rsve_test.go`; `cmd/dryscan/`; YAML rsve block; dryscan validated (0 setups, market extended — correct) | Y | Wire RSVE into live pipeline |
| 2026-05-06 | Wire RSVE into live pipeline; remove Claude from confirmation; expose gate diagnostics in API; backtest 2023→now; clean unused files; create context management files | RSVE wired into activities+handlers; Claude removed from confirmation (rsve_deterministic); `rsve_gates[]` in API; backtest 87 signals (+0.3% overall, 75+ score +5.3%); deleted 6 unused dirs; CLAUDE.md + checkpoint.md + history.md created | Y | Deploy |
| 2026-05-07 | RSVE-O PRO refactor — 4-status enum, option gate split, PRO score weights, 2 new patterns | New 4-status enum (rejected/stock_signal_passed/option_blocked/paper_trade_created); gates 9–12 option split; PRO score weights; support_resistance_retest + AnalyzeORBPattern added; rsve_compliance_test.go (15 tests) | Y | Phase 2 behavioral changes |
| 2026-05-09 | Phase 2 — legacy YAML cleanup, tighten confirmation, remove EOD exit, remove min-hold, move pattern before breakout | deprecated_legacy_strategy_families disabled; required+optional confirmation logic; EOD forced exit removed; min-hold removed; pattern analysis before breakout gate; phase2_compliance_test.go | Y | Phase 3 Claude removal |
| 2026-05-09 | Phase 3 — remove Claude from all trading paths; add structure invalidation exit rule | Claude removed from RunOpeningConfirmationActivity; FirstPositionReviewCycle + ContinuationReviewCycle removed from worker; STRUCTURE_INVALIDATION exit rule #6; migration 000010 | Y | Production hardening |
| 2026-05-11 | Production hardening v14 — 6 upgrades | fill.go (mid/haircut/reject model); walkforward.go + sharpe.go research framework; sentiment stub; /api/rejection-analytics; BreakoutLevel→StructureInvalidationLevel wired end-to-end; strategy_rules.yaml legacy block deleted; weekly_review_protocol.md deleted; checkpoint.md + history.md updated | Y | Monitor live runs |
| 2026-05-12 | No trade fired — investigate root cause + supervisor | Stale workers diagnosed + killed; Go supervisor built (cmd/supervisor); start.sh + stop.sh deleted; make start/stop/app-logs | Y | Transition stack to supervisor; monitor 06:45 |

---

## Key Decisions Log

| Date | Decision | Why |
|------|----------|-----|
| 2026-04-24 | Paper trades require no human approval | Autonomous 1-month operation requirement |
| 2026-04-24 | 7–14 DTE only | Balances time decay cost vs. enough time for move to develop |
| 2026-04-28 | Use chain-level P/C ratio not strike-level aggregation | Strike-level was double-counting and noisy |
| 2026-05-05 | RSVE all-or-nothing gate logic (not scoring) | Scoring lets weak setups through; binary gates enforce hard quality floor |
| 2026-05-06 | Remove Claude from opening confirmation entirely | Deterministic gates are faster, cheaper, and more consistent; Claude was not adding signal over RSVE |
| 2026-05-06 | Bearish backtest 41.2% win / -2.2% — defer tightening | Not enough sample size to tune confidently; flag for next weekly review |
| 2026-05-06 | RSVE score 75+ bucket → +5.3% expectancy | Score IS useful as a shortlist filter in live trading even though it is not a gate |
| 2026-05-07 | 4-status enum replaces binary pass/fail | Separates "stock looks good but option data missing" from "option gates failed" — actionable distinction for diagnostics |
| 2026-05-09 | EOD forced exit removed | 21–45 DTE swings should hold overnight; forced EOD exit was destroying P&L on valid multi-day setups |
| 2026-05-09 | Weekly review feature removed | Never built; all references removed from YAML, CLAUDE.md, docs; weekly_review_protocol.md deleted |
| 2026-05-11 | Fill model: reject spread >8% at entry | Paper trading with ask-price fill on wide spreads overstated P&L; haircut/reject model gives realistic slippage |
| 2026-05-11 | Structure invalidation uses pattern level not entry price | Entry price is not a structural level; pattern detector already computes bull_flag low / triangle support — use that instead |
