#!/bin/bash
# start.sh
#
# Run once:  bash start.sh
# Stops:     bash stop.sh
#
# Starts everything and keeps it running in the background.
# Logs go to logs/server.log and logs/worker.log.
# After closing the lid and reopening, nothing to do — processes resume automatically.
# After a full Mac shutdown/restart: just run bash start.sh again.

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROJECT_DIR"

set -a; source .env; set +a

mkdir -p bin logs

echo "=== MakeMyTrade ==="

# ── 1. Prevent idle sleep ────────────────────────────────────────────────────
# Kills any existing caffeinate tied to this project, starts a fresh one.
pkill -f "caffeinate.*makemytrade" 2>/dev/null || true
nohup caffeinate -i > /dev/null 2>&1 &
echo $! > logs/caffeinate.pid
echo "[✓] Sleep prevention active (PID $(cat logs/caffeinate.pid))"

# ── 2. Build binaries only if missing ───────────────────────────────────────
if [ ! -f bin/server ] || [ ! -f bin/worker ]; then
    echo "[~] Binaries not found — building (one-time, takes ~1 min)..."
    go build -o bin/server ./cmd/server/ &
    go build -o bin/worker ./cmd/worker/ &
    wait
    echo "[✓] Binaries built"
else
    echo "[✓] Binaries ready"
fi

# ── 3. Start Docker services ─────────────────────────────────────────────────
echo "[~] Starting Docker services..."
docker compose up -d > /dev/null 2>&1
until nc -z localhost 5432 2>/dev/null; do sleep 1; done
until nc -z localhost 7233 2>/dev/null; do sleep 1; done
echo "[✓] Postgres + Temporal ready"

# ── 4. Stop any previous server/worker ──────────────────────────────────────
[ -f logs/server.pid ] && kill "$(cat logs/server.pid)" 2>/dev/null || true
[ -f logs/worker.pid ] && kill "$(cat logs/worker.pid)" 2>/dev/null || true
sleep 1

# ── 5. Start server ──────────────────────────────────────────────────────────
nohup bin/server > logs/server.log 2>&1 &
echo $! > logs/server.pid
echo "[✓] Server started (PID $(cat logs/server.pid)) → logs/server.log"

# ── 6. Start worker ──────────────────────────────────────────────────────────
nohup bin/worker > logs/worker.log 2>&1 &
echo $! > logs/worker.pid
echo "[✓] Worker started (PID $(cat logs/worker.pid)) → logs/worker.log"

sleep 2

echo ""
echo "=== All running ==="
echo ""
echo "  Server  → http://localhost:8080"
echo "  Temporal → http://localhost:8088"
echo ""
echo "  Live logs:   tail -f logs/server.log"
echo "               tail -f logs/worker.log"
echo "  Stop all:    bash stop.sh"
echo ""
echo "Lid close / open: nothing to do — processes resume automatically."
echo "After Mac restart: run bash start.sh again."
