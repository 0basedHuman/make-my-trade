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
| 2026-05-06 | Wire RSVE into live pipeline; remove Claude from confirmation; expose gate diagnostics in API; backtest 2023→now; clean unused files; create context management files | RSVE wired into activities+handlers; Claude removed from confirmation (rsve_deterministic); `rsve_gates[]` in API; backtest 87 signals (+0.3% overall, 75+ score +5.3%); deleted 6 unused dirs; CLAUDE.md + checkpoint.md + history.md created | Y | Deploy + `cmd/weeklyreview/` |

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
