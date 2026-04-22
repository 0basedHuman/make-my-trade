#!/bin/bash
# scripts/install-daemon.sh
#
# One-time setup. Run this once from the project root:
#   bash scripts/install-daemon.sh
#
# What it does:
#   1. Builds the worker binary
#   2. Installs the launchd plist into ~/Library/LaunchAgents/
#   3. Loads the service (starts it immediately + marks it to start at every login)
#   4. Configures macOS power settings so the system never idle-sleeps on AC
#      and sleeps after 1 hour on battery
#
# To uninstall:
#   bash scripts/uninstall-daemon.sh

set -e

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PLIST_NAME="com.makemytrade.worker"
PLIST_SRC="$PROJECT_DIR/scripts/$PLIST_NAME.plist"
PLIST_DST="$HOME/Library/LaunchAgents/$PLIST_NAME.plist"
LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"

echo "=== MakeMyTrade worker daemon installer ==="
echo "Project: $PROJECT_DIR"
echo ""

# ── Step 1: Build worker binary ────────────────────────────────────────────
echo "[1/4] Building worker binary..."
cd "$PROJECT_DIR"
mkdir -p bin
go build -o bin/worker ./cmd/worker/
echo "      bin/worker built OK"

# ── Step 2: Create logs dir ────────────────────────────────────────────────
mkdir -p "$PROJECT_DIR/logs"
echo "[2/4] Log directory: $PROJECT_DIR/logs/"

# ── Step 3: Install and load the plist ────────────────────────────────────
echo "[3/4] Installing launchd plist..."
mkdir -p "$LAUNCH_AGENTS_DIR"
cp "$PLIST_SRC" "$PLIST_DST"

# Unload first if already installed (safe no-op if not loaded)
launchctl unload "$PLIST_DST" 2>/dev/null || true
launchctl load "$PLIST_DST"
echo "      Loaded: $PLIST_DST"

# ── Step 4: Power settings ────────────────────────────────────────────────
# On AC: never sleep (display can still sleep after 15 min — saves power).
# On battery: sleep after 60 min idle (avoids draining battery overnight
#             if you forget to plug in).
# Note: caffeinate in keepalive.sh also blocks idle sleep while the
#       worker is running, so this is a belt-and-suspenders measure.
echo "[4/4] Configuring power management..."
sudo pmset -c sleep 0 displaysleep 15   # on AC: no system sleep, display sleeps after 15 min
sudo pmset -b sleep 60 displaysleep 5   # on battery: sleep after 60 min idle
echo "      Power settings updated"

echo ""
echo "=== Done ==="
echo ""
echo "The worker is now running and will restart automatically at every login."
echo ""
echo "Useful commands:"
echo "  tail -f $PROJECT_DIR/logs/worker.log        # live worker logs"
echo "  launchctl list | grep makemytrade            # check service status (0 = running)"
echo "  launchctl stop $PLIST_NAME                   # stop the worker"
echo "  launchctl start $PLIST_NAME                  # start the worker"
echo "  bash scripts/uninstall-daemon.sh             # remove completely"
echo ""
echo "After every 'make build', the worker binary is updated automatically"
echo "on next restart. To apply immediately:"
echo "  launchctl stop $PLIST_NAME   # launchd restarts it in 15 s"
