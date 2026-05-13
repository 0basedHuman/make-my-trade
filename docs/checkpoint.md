# Checkpoint — 2026-05-12

## Current state
v14 running. Stale worker bug fixed. Supervisor built. start.sh + stop.sh deleted.

## What is running (pre-supervisor — started before supervisor existed)
- Server: PID in `logs/server.pid`, http://localhost:8080, schema version 10
- Worker: PID 50270 in `logs/worker.pid`, 4 schedules:
  - 06:45 PT DailyResearchCycle
  - 07:00 PT OpeningConfirmationCycle
  - */10m MechanicalRiskCycle
  - 12:45 PT DailyPositionReview

## How to start / stop (new — use this going forward)
```
make start      # builds supervisor + all binaries, starts everything (foreground, Ctrl-C to stop)
make stop       # SIGTERM supervisor → it stops server + worker cleanly
make app-logs   # tail -f logs/server.log logs/worker.log
```
To transition the currently running stack to supervisor:
  kill $(cat logs/server.pid) $(cat logs/worker.pid)
  make start

## What was completed today (2026-05-12)

### Stale worker investigation + fix
- Diagnosed "no trade fired today": DailyResearchCycle DID run (Temporal confirmed: completed in 87s)
- Result was `scanned=26 candidates=0 no_trade=true` — legitimate no-trade day
- Two orphaned worker processes (PIDs 90119, 89583 from Apr 28 go-run sessions) competed on Temporal task queue
- Stale binary used old `gate_btc` column (renamed to `gate_relative_strength`) → all UpsertCandidate calls failed silently
- Fix: killed stale PIDs; supervisor now kills orphans on startup

### Go supervisor (replaces start.sh + stop.sh)
- `cmd/supervisor/main.go` (new, ~180 lines)
- Builds server + worker in parallel, starts Docker, waits for Postgres + Temporal
- Starts server, waits for /health, then starts worker
- SIGTERM/SIGINT → gracefully stops both children (10s timeout → SIGKILL)
- Restarts crashed children up to 3x with exponential backoff, then exits
- Caffeinate watches supervisor PID — dies when supervisor dies
- `Makefile`: added `start`, `stop`, `app-logs` targets
- `start.sh` and `stop.sh` deleted

## Build / test status
- `go build ./...` ✅
- All tests passed from v14

## Open items
- `spy_trend_ok` in daily_summaries is always NULL — not being set in activities.go
- Sentiment stub always returns "unavailable"
- `RunWeeklyReviewActivity` is a disabled no-op stub in activities.go
- Dead code: `engine.go` BTC/RSI fields; `store.SetHoldOvernightApproved` never called

## Exact next step
Transition to supervisor: kill current server+worker, run `make start`.
Then monitor tomorrow's 06:45 scan via `make app-logs`.
Confirm: DailyResearchCycle logs appear, trade_candidates rows stored for 2026-05-13.
