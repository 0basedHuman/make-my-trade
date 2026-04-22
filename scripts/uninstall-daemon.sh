#!/bin/bash
# scripts/uninstall-daemon.sh
# Removes the launchd service and restores default power settings.

PLIST_NAME="com.makemytrade.worker"
PLIST_DST="$HOME/Library/LaunchAgents/$PLIST_NAME.plist"

echo "=== Uninstalling MakeMyTrade worker daemon ==="

launchctl stop "$PLIST_NAME" 2>/dev/null || true
launchctl unload "$PLIST_DST" 2>/dev/null || true
rm -f "$PLIST_DST"
echo "Plist removed."

# Restore macOS default power settings
sudo pmset -c sleep 1 displaysleep 10
sudo pmset -b sleep 5 displaysleep 2
echo "Power settings restored to defaults."

echo "Done. Worker will no longer start automatically."
