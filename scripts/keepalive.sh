#!/bin/bash
# scripts/keepalive.sh
#
# Wrapper launched by launchd. Responsibilities:
#   1. Prevents idle sleep via caffeinate (no sudo needed)
#   2. Ensures Docker services are running
#   3. Waits for Postgres + Temporal to be ready before starting the worker
#   4. Runs the compiled worker binary (exec replaces this shell → same PID)
#
# launchd's KeepAlive=true restarts this script if the worker crashes.

PROJECT_DIR="/Users/harsh/Documents/make-my-trade"
cd "$PROJECT_DIR" || exit 1

# ── 1. Load .env so TEMPORAL_HOST, DB_URL etc. are visible to the worker ──
if [ -f .env ]; then
    set -a
    # shellcheck disable=SC1091
    source .env
    set +a
fi

echo "[keepalive] $(date): starting"

# ── 2. Prevent idle sleep while this script (and later the worker) run ────
# caffeinate -i  → prevent system idle sleep
# caffeinate -w $$ → release the assertion when PID $$ exits
# After `exec` below, $$ becomes the worker's PID — caffeinate follows it.
caffeinate -i -w $$ &

# ── 3. Bring up Docker services (Postgres, Redis, Temporal) ───────────────
echo "[keepalive] Starting Docker services..."
docker compose up -d
echo "[keepalive] Docker compose up returned"

# ── 4. Wait for Postgres (port 5432) ─────────────────────────────────────
echo "[keepalive] Waiting for Postgres..."
for i in $(seq 1 60); do
    nc -z localhost 5432 2>/dev/null && break
    sleep 2
done
echo "[keepalive] Postgres ready"

# ── 5. Wait for Temporal server (port 7233) ──────────────────────────────
echo "[keepalive] Waiting for Temporal..."
for i in $(seq 1 60); do
    nc -z localhost 7233 2>/dev/null && break
    sleep 2
done
echo "[keepalive] Temporal ready"

# ── 6. Run the worker — exec replaces this shell process ─────────────────
# launchd tracks this PID; KeepAlive restarts when it exits.
echo "[keepalive] Launching worker binary..."
exec "$PROJECT_DIR/bin/worker"
