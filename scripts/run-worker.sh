#!/bin/bash
# Wrapper for the makemytrade Temporal worker.
# Sourced by launchd so env vars from .env are available without hardcoding
# them in the plist. Rebuild bin/worker before reloading the launchd agent:
#   go build -o bin/worker ./cmd/worker/ && launchctl kickstart -k gui/$(id -u)/com.makemytrade.worker

set -e

cd /Users/harsh/Documents/make-my-trade

# Load .env — skip blank lines and comments
set -o allexport
source .env
set +o allexport

exec ./bin/worker
