#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$ROOT_DIR/../.." && pwd)

TMP_ROOT=${TMP_ROOT:-/private/tmp}
BINARY=${BINARY:-$TMP_ROOT/buckit-fastopen}
ADDRESS=${ADDRESS:-127.0.0.1:9000}
CONSOLE_ADDRESS=${CONSOLE_ADDRESS:-127.0.0.1:9001}
CONFIG_DIR=${CONFIG_DIR:-$TMP_ROOT/buckit-fastopen-config}
LOG_DIR=${LOG_DIR:-$TMP_ROOT/buckit-fastopen-logs}
PID_FILE=${PID_FILE:-$LOG_DIR/server.pid}
HEALTH_URL=${HEALTH_URL:-http://$ADDRESS/minio/health/ready}
MINIO_ROOT_USER=${MINIO_ROOT_USER:-buckitadmin}
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD:-buckitadmin}
MINIO_PROMETHEUS_AUTH_TYPE=${MINIO_PROMETHEUS_AUTH_TYPE:-public}
MINIO_CI_CD=${MINIO_CI_CD:-1}
MINIO_STORAGE_CLASS_STANDARD=${MINIO_STORAGE_CLASS_STANDARD:-EC:4}

data_dirs=(
  "$TMP_ROOT/buckit-fastopen-1"
  "$TMP_ROOT/buckit-fastopen-2"
  "$TMP_ROOT/buckit-fastopen-3"
  "$TMP_ROOT/buckit-fastopen-4"
  "$TMP_ROOT/buckit-fastopen-5"
  "$TMP_ROOT/buckit-fastopen-6"
  "$TMP_ROOT/buckit-fastopen-7"
  "$TMP_ROOT/buckit-fastopen-8"
  "$TMP_ROOT/buckit-fastopen-9"
  "$TMP_ROOT/buckit-fastopen-10"
  "$TMP_ROOT/buckit-fastopen-11"
  "$TMP_ROOT/buckit-fastopen-12"
  "$TMP_ROOT/buckit-fastopen-13"
  "$TMP_ROOT/buckit-fastopen-14"
  "$TMP_ROOT/buckit-fastopen-15"
  "$TMP_ROOT/buckit-fastopen-16"
)

usage() {
  cat <<'EOF'
usage: testing/fastopen-local/rig.sh <command>

commands:
  build       build the local test binary from the current repo state
  init-dirs   create the local erasure directories
  wipe        stop the server and remove local dirs/config/logs
  start-off   start Buckit with BUCKIT_FAST_GET=0
  start-on    start Buckit with BUCKIT_FAST_GET=1
  stop        stop the running local Buckit process
  status      print PID, health, and log path
  health      wait until /minio/health/ready returns 200
  print-dirs  print the configured local data directories
EOF
}

init_dirs() {
  mkdir -p "$CONFIG_DIR" "$LOG_DIR"
  mkdir -p "${data_dirs[@]}"
}

build_binary() {
  (
    cd "$REPO_ROOT"
    CGO_ENABLED=0 go build -tags kqueue -trimpath -o "$BINARY" .
  )
  printf 'binary=%s\n' "$BINARY"
}

is_running() {
  [[ -f "$PID_FILE" ]] || return 1
  local pid
  pid=$(cat "$PID_FILE")
  kill -0 "$pid" 2>/dev/null
}

stop_server() {
  if ! [[ -f "$PID_FILE" ]]; then
    return 0
  fi

  local pid
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
}

wait_healthy() {
  for _ in $(seq 1 100); do
    code=$(curl -sS -o /dev/null -w '%{http_code}' "$HEALTH_URL" || true)
    if [[ "$code" == "200" ]]; then
      return 0
    fi
    sleep 0.2
  done
  echo "server did not become healthy: $HEALTH_URL" >&2
  return 1
}

start_server() {
  local fast_get=$1
  init_dirs
  stop_server

  if [[ ! -x "$BINARY" ]]; then
    echo "missing binary: $BINARY" >&2
    echo "run: testing/fastopen-local/rig.sh build" >&2
    exit 1
  fi

  local stamp log_file
  stamp=$(date +%Y%m%d-%H%M%S)
  log_file="$LOG_DIR/server-fastget-${fast_get}-${stamp}.log"

  local -a server_cmd=(
    "$BINARY"
    --config-dir "$CONFIG_DIR"
    server
    --address "$ADDRESS"
    --console-address "$CONSOLE_ADDRESS"
    "${data_dirs[@]}"
  )

  (
    cd "$REPO_ROOT"
    export BUCKIT_FAST_GET="$fast_get"
    export MINIO_ROOT_USER
    export MINIO_ROOT_PASSWORD
    export MINIO_PROMETHEUS_AUTH_TYPE
    export MINIO_CI_CD
    export MINIO_STORAGE_CLASS_STANDARD
    export MINIO_CONFIG_ENV_FILE=
    if command -v setsid >/dev/null 2>&1; then
      setsid "${server_cmd[@]}" >"$log_file" 2>&1 < /dev/null &
    else
      nohup "${server_cmd[@]}" >"$log_file" 2>&1 < /dev/null &
    fi
    echo $! >"$PID_FILE"
  )

  wait_healthy

  printf 'pid=%s\n' "$(cat "$PID_FILE")"
  printf 'fast_get=%s\n' "$fast_get"
  printf 'address=%s\n' "$ADDRESS"
  printf 'health=%s\n' "$HEALTH_URL"
  printf 'storage_class_standard=%s\n' "$MINIO_STORAGE_CLASS_STANDARD"
  printf 'log=%s\n' "$log_file"
}

wipe_rig() {
  stop_server
  rm -rf "$CONFIG_DIR" "$LOG_DIR"
  rm -rf "${data_dirs[@]}"
}

status() {
  if is_running; then
    printf 'running=yes\n'
    printf 'pid=%s\n' "$(cat "$PID_FILE")"
  else
    printf 'running=no\n'
  fi
  printf 'health_url=%s\n' "$HEALTH_URL"
  curl -sS -o /dev/null -w 'health_http=%{http_code}\n' "$HEALTH_URL" || true
  printf 'log_dir=%s\n' "$LOG_DIR"
  printf 'binary=%s\n' "$BINARY"
}

print_dirs() {
  printf '%s\n' "${data_dirs[@]}"
}

cmd=${1:-}
case "$cmd" in
  build)
    build_binary
    ;;
  init-dirs)
    init_dirs
    print_dirs
    ;;
  wipe)
    wipe_rig
    ;;
  start-off)
    start_server 0
    ;;
  start-on)
    start_server 1
    ;;
  stop)
    stop_server
    ;;
  status)
    status
    ;;
  health)
    wait_healthy
    ;;
  print-dirs)
    print_dirs
    ;;
  *)
    usage
    exit 1
    ;;
esac
