#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=${ROOT_DIR:-/root/singletrip-bench}
WARP=${WARP:-$ROOT_DIR/bin/warp}
HOST=${HOST:-127.0.0.1:9000}
ACCESS_KEY=${ACCESS_KEY:-buckitadmin}
SECRET_KEY=${SECRET_KEY:-buckitadmin}
BUCKET=${BUCKET:-singletrip-warp-2m}
OBJECTS=${OBJECTS:-2000}
CONCURRENT=${CONCURRENT:-48}
DURATION=${DURATION:-30s}
OBJ_SIZE=${OBJ_SIZE:-2MiB}
RESULTS_DIR=${RESULTS_DIR:-$ROOT_DIR/results}

mkdir -p "$RESULTS_DIR"

ARM=${1:-}
if [[ -z "$ARM" ]]; then
  echo "usage: $0 load-on|off|on" >&2
  exit 1
fi

COMMON=(
  "--host=$HOST"
  "--access-key=$ACCESS_KEY"
  "--secret-key=$SECRET_KEY"
  "--bucket=$BUCKET"
  "--obj.size=$OBJ_SIZE"
  "--objects=$OBJECTS"
  "--concurrent=$CONCURRENT"
  "--duration=$DURATION"
  "--noclear"
)

case "$ARM" in
  load-on)
    exec "$WARP" get \
      "${COMMON[@]}" \
      "--benchdata=$RESULTS_DIR/warp-singletrip-2m-load-on.csv.zst"
    ;;
  off)
    exec "$WARP" get \
      "${COMMON[@]}" \
      --list-existing \
      "--benchdata=$RESULTS_DIR/warp-singletrip-2m-off.csv.zst"
    ;;
  on)
    exec "$WARP" get \
      "${COMMON[@]}" \
      --list-existing \
      "--benchdata=$RESULTS_DIR/warp-singletrip-2m-on.csv.zst"
    ;;
  *)
    echo "unknown arm: $ARM" >&2
    exit 1
    ;;
esac
