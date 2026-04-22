#!/bin/bash
# stop.sh — stop everything started by start.sh

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROJECT_DIR"

echo "Stopping MakeMyTrade..."

[ -f logs/server.pid ]     && kill "$(cat logs/server.pid)"     2>/dev/null && echo "[✓] Server stopped"
[ -f logs/worker.pid ]     && kill "$(cat logs/worker.pid)"     2>/dev/null && echo "[✓] Worker stopped"
[ -f logs/caffeinate.pid ] && kill "$(cat logs/caffeinate.pid)" 2>/dev/null && echo "[✓] Sleep prevention stopped"

rm -f logs/server.pid logs/worker.pid logs/caffeinate.pid

echo "Done. Docker services still running (data preserved)."
echo "To also stop Docker: docker compose down"
