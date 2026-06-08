#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=${ROOT_DIR:-/root/singletrip-bench}
BINARY=${BINARY:-$ROOT_DIR/bin/buckit}
CONFIG_DIR=${CONFIG_DIR:-$ROOT_DIR/config}
LOG_DIR=${LOG_DIR:-$ROOT_DIR/logs}
FAST_GET=${FAST_GET:-0}
ADDRESS=${ADDRESS:-:9000}
CONSOLE_ADDRESS=${CONSOLE_ADDRESS:-:9001}
MINIO_ROOT_USER=${MINIO_ROOT_USER:-buckitadmin}
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD:-buckitadmin}

mkdir -p "$CONFIG_DIR" "$LOG_DIR"

if [[ $# -eq 0 ]]; then
  echo "usage: FAST_GET=0|1 $0 /mnt/data01 [/mnt/data02 ...]" >&2
  exit 1
fi

export BUCKIT_FAST_GET="$FAST_GET"
export MINIO_ROOT_USER
export MINIO_ROOT_PASSWORD
export MINIO_CONFIG_ENV_FILE=

exec "$BINARY" \
  --config-dir "$CONFIG_DIR" \
  server \
  --address "$ADDRESS" \
  --console-address "$CONSOLE_ADDRESS" \
  "$@"
