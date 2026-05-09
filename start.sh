#!/bin/bash
# start.sh — start the MakeMyTrade stack
#
# Usage:  bash start.sh
# Stop:   bash stop.sh

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROJECT_DIR"

APP_VERSION="v13"

set -a; source .env; set +a

mkdir -p bin logs

echo ""
echo "=== MakeMyTrade $APP_VERSION ==="
echo ""

# ── 1. Prevent idle sleep ────────────────────────────────────────────────────
pkill -f "caffeinate.*makemytrade" 2>/dev/null || true
nohup caffeinate -i -w $$ > /dev/null 2>&1 &
CAFF_PID=$!
echo $CAFF_PID > logs/caffeinate.pid
echo "[✓] Sleep prevention active (PID $CAFF_PID)"

# ── 2. Rebuild binaries ──────────────────────────────────────────────────────
echo "[~] Building binaries ($APP_VERSION)..."
go build -o bin/server ./cmd/server/ &
go build -o bin/worker ./cmd/worker/ &
wait
echo "[✓] Binaries built"

# ── 3. Docker services ───────────────────────────────────────────────────────
echo "[~] Starting Docker services..."
docker compose up -d > /dev/null 2>&1

echo -n "[~] Waiting for Postgres..."
until nc -z localhost 5432 2>/dev/null; do printf "."; sleep 1; done
echo " ready"

echo -n "[~] Waiting for Temporal..."
until nc -z localhost 7233 2>/dev/null; do printf "."; sleep 1; done
echo " ready"

# ── 4. Kill previous instances ───────────────────────────────────────────────
if [ -f logs/server.pid ]; then
    kill "$(cat logs/server.pid)" 2>/dev/null || true
    rm -f logs/server.pid
fi
if [ -f logs/worker.pid ]; then
    kill "$(cat logs/worker.pid)" 2>/dev/null || true
    rm -f logs/worker.pid
fi
sleep 1

# ── 5. Start server ──────────────────────────────────────────────────────────
nohup bin/server > logs/server.log 2>&1 &
SERVER_PID=$!
echo $SERVER_PID > logs/server.pid

echo -n "[~] Waiting for server..."
for i in $(seq 1 30); do
    if curl -sf http://localhost:8080/health > /dev/null 2>&1; then
        echo " ready"
        break
    fi
    printf "."
    sleep 1
    if [ "$i" -eq 30 ]; then
        echo " TIMEOUT"
        echo "[!] Server did not become healthy in 30s — check logs/server.log"
        tail -20 logs/server.log
        exit 1
    fi
done
echo "[✓] Server running (PID $SERVER_PID)"

# ── 6. Start worker ──────────────────────────────────────────────────────────
nohup bin/worker > logs/worker.log 2>&1 &
WORKER_PID=$!
echo $WORKER_PID > logs/worker.pid
sleep 2
if ! kill -0 "$WORKER_PID" 2>/dev/null; then
    echo "[!] Worker crashed immediately — check logs/worker.log"
    tail -20 logs/worker.log
    exit 1
fi
echo "[✓] Worker running (PID $WORKER_PID)"

echo ""
echo "=== $APP_VERSION running ==="
echo ""
echo "  Dashboard  → http://localhost:8080"
echo "  Temporal   → http://localhost:8088"
echo ""
echo "  Logs:   tail -f logs/server.log"
echo "          tail -f logs/worker.log"
echo "  Stop:   bash stop.sh"
echo ""
