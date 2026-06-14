#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=${ROOT_DIR:-$HOME/buckit-fastopen-ec2}
LOG_DIR=${LOG_DIR:-$ROOT_DIR/logs}
PID_FILE=${PID_FILE:-$LOG_DIR/buckit.pid}

if [[ ! -f "$PID_FILE" ]]; then
  exit 0
fi

pid=$(cat "$PID_FILE")
if kill -0 "$pid" 2>/dev/null; then
  kill "$pid"
  for _ in $(seq 1 50); do
    if ! kill -0 "$pid" 2>/dev/null; then
      break
    fi
    sleep 0.2
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null || true
  fi
fi
rm -f "$PID_FILE"
