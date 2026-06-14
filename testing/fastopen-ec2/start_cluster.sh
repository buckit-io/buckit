#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=${ROOT_DIR:-$HOME/buckit-fastopen-ec2}
BINARY=${BINARY:-$ROOT_DIR/bin/buckit}
CONFIG_DIR=${CONFIG_DIR:-$ROOT_DIR/config}
LOG_DIR=${LOG_DIR:-$ROOT_DIR/logs}
PID_FILE=${PID_FILE:-$LOG_DIR/buckit.pid}
DATA_ROOT=${DATA_ROOT:-/mnt}
DRIVE_COUNT=${DRIVE_COUNT:-4}
DRIVE_PATHS=${DRIVE_PATHS:-}
CLUSTER_NODES=${CLUSTER_NODES:-buckit-node1,buckit-node2}
FAST_GET=${FAST_GET:-0}
ADDRESS=${ADDRESS:-:9000}
CONSOLE_ADDRESS=${CONSOLE_ADDRESS:-:9001}
MINIO_ROOT_USER=${MINIO_ROOT_USER:-buckitadmin}
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD:-buckitadmin}
MINIO_PROMETHEUS_AUTH_TYPE=${MINIO_PROMETHEUS_AUTH_TYPE:-public}
MINIO_STORAGE_CLASS_STANDARD=${MINIO_STORAGE_CLASS_STANDARD:-EC:2}
MINIO_CI_CD=${MINIO_CI_CD:-1}

mkdir -p "$CONFIG_DIR" "$LOG_DIR"

if [[ ! -x "$BINARY" ]]; then
  echo "missing binary: $BINARY" >&2
  exit 1
fi

if [[ -f "$PID_FILE" ]]; then
  pid=$(cat "$PID_FILE")
  if kill -0 "$pid" 2>/dev/null; then
    echo "already running with pid=$pid" >&2
    exit 1
  fi
  rm -f "$PID_FILE"
fi

resolve_drive_paths() {
  if [[ -n "$DRIVE_PATHS" ]]; then
    printf '%s\n' "$DRIVE_PATHS"
    return 0
  fi
  local paths=()
  local idx
  for idx in $(seq 1 "$DRIVE_COUNT"); do
    paths+=("$DATA_ROOT/buckit-fastopen-$idx")
  done
  local joined=""
  for path in "${paths[@]}"; do
    if [[ -n "$joined" ]]; then
      joined+=","
    fi
    joined+="$path"
  done
  printf '%s\n' "$joined"
}

IFS=',' read -r -a drive_paths <<<"$(resolve_drive_paths)"
IFS=',' read -r -a cluster_nodes <<<"$CLUSTER_NODES"
data_endpoints=()
for node in "${cluster_nodes[@]}"; do
  for drive_path in "${drive_paths[@]}"; do
    data_endpoints+=("http://${node}:9000${drive_path}")
  done
done

log_file="$LOG_DIR/server-fastget-${FAST_GET}-$(date +%Y%m%d-%H%M%S).log"
(
  export BUCKIT_FAST_GET="$FAST_GET"
  export MINIO_ROOT_USER
  export MINIO_ROOT_PASSWORD
  export MINIO_PROMETHEUS_AUTH_TYPE
  export MINIO_STORAGE_CLASS_STANDARD
  export MINIO_CI_CD
  export MINIO_CONFIG_ENV_FILE=
  nohup "$BINARY" \
    --config-dir "$CONFIG_DIR" \
    server \
    --address "$ADDRESS" \
    --console-address "$CONSOLE_ADDRESS" \
    "${data_endpoints[@]}" \
    >"$log_file" 2>&1 < /dev/null &
  echo $! >"$PID_FILE"
)

echo "pid=$(cat "$PID_FILE")"
echo "log=$log_file"
