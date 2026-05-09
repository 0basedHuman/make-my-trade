#!/bin/bash
# stop.sh — stop everything started by start.sh

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROJECT_DIR"

echo "Stopping MakeMyTrade..."

stop_pid() {
    local name="$1"
    local pidfile="$2"
    if [ -f "$pidfile" ]; then
        local pid
        pid=$(cat "$pidfile")
        if kill "$pid" 2>/dev/null; then
            echo "[✓] $name stopped (PID $pid)"
        else
            echo "[~] $name was already stopped"
        fi
        rm -f "$pidfile"
    fi
}

stop_pid "Server"           logs/server.pid
stop_pid "Worker"           logs/worker.pid
stop_pid "Sleep prevention" logs/caffeinate.pid

echo ""
echo "Docker services still running (data preserved)."
echo "To also stop Docker:  docker compose down"
echo ""
