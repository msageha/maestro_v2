#!/bin/bash
# Task heartbeat wrapper script for Workers

set -euo pipefail

# Parse arguments
TASK_ID=""
WORKER_ID=""
EPOCH=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --task-id)
            TASK_ID="$2"
            shift 2
            ;;
        --worker-id)
            WORKER_ID="$2"
            shift 2
            ;;
        --epoch)
            EPOCH="$2"
            shift 2
            ;;
        *)
            echo "Unknown option: $1" >&2
            exit 1
            ;;
    esac
done

if [[ -z "$TASK_ID" || -z "$WORKER_ID" || -z "$EPOCH" ]]; then
    echo "Usage: $0 --task-id <id> --worker-id <id> --epoch <n>" >&2
    exit 1
fi

# Find maestro directory from current location
MAESTRO_DIR=""
current_dir="$(pwd)"
while [[ "$current_dir" != "/" ]]; do
    if [[ -d "$current_dir/.maestro" ]]; then
        MAESTRO_DIR="$current_dir/.maestro"
        break
    fi
    current_dir="$(dirname "$current_dir")"
done

if [[ -z "$MAESTRO_DIR" ]]; then
    echo "Error: .maestro/ directory not found" >&2
    exit 1
fi

# PID file for single-instance guarantee
PID_FILE="$MAESTRO_DIR/tmp/heartbeat_${TASK_ID}.pid"
mkdir -p "$MAESTRO_DIR/tmp"

# Check if another heartbeat is already running
if [[ -f "$PID_FILE" ]]; then
    OLD_PID=$(cat "$PID_FILE")
    if kill -0 "$OLD_PID" 2>/dev/null; then
        echo "Heartbeat already running for task $TASK_ID (PID: $OLD_PID)" >&2
        exit 0
    fi
fi

# Write our PID
echo $$ > "$PID_FILE"

# Cleanup on exit
cleanup() {
    rm -f "$PID_FILE"
}
trap cleanup EXIT INT TERM

# Heartbeat loop with jitter
while true; do
    # Send heartbeat
    if ! maestro task heartbeat --task-id "$TASK_ID" --worker-id "$WORKER_ID" --epoch "$EPOCH"; then
        echo "Heartbeat failed for task $TASK_ID" >&2
        # Continue trying - don't exit on failure
    fi

    # Sleep with jitter (30-34 seconds)
    INTERVAL=$((30 + RANDOM % 5))
    sleep "$INTERVAL"
done