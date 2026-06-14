#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=${ROOT_DIR:-$HOME/buckit-fastopen-ec2}
BIN_DIR=${BIN_DIR:-$ROOT_DIR/bin}
LOG_DIR=${LOG_DIR:-$ROOT_DIR/logs}
CONFIG_DIR=${CONFIG_DIR:-$ROOT_DIR/config}
RESULTS_DIR=${RESULTS_DIR:-$ROOT_DIR/results}
DATA_ROOT=${DATA_ROOT:-/mnt}
DRIVE_COUNT=${DRIVE_COUNT:-4}
DRIVE_PATHS=${DRIVE_PATHS:-}
NODE_NAME=${NODE_NAME:?set NODE_NAME}
CLUSTER_HOSTS=${CLUSTER_HOSTS:-}
LOCAL_PRIVATE_IP=${LOCAL_PRIVATE_IP:-}
PEER_NAME=${PEER_NAME:-}
PEER_PRIVATE_IP=${PEER_PRIVATE_IP:-}

ensure_python() {
  if command -v python3 >/dev/null 2>&1; then
    return 0
  fi
  if command -v dnf >/dev/null 2>&1; then
    sudo dnf install -y python3
    return 0
  fi
  if command -v yum >/dev/null 2>&1; then
    sudo yum install -y python3
    return 0
  fi
  echo "python3 is required" >&2
  exit 1
}

default_cluster_hosts() {
  if [[ -n "$CLUSTER_HOSTS" ]]; then
    printf '%s\n' "$CLUSTER_HOSTS"
    return 0
  fi
  if [[ -z "$LOCAL_PRIVATE_IP" || -z "$PEER_NAME" || -z "$PEER_PRIVATE_IP" ]]; then
    echo "set CLUSTER_HOSTS or LOCAL_PRIVATE_IP/PEER_NAME/PEER_PRIVATE_IP" >&2
    exit 1
  fi
  printf '%s\n' "${NODE_NAME}=${LOCAL_PRIVATE_IP},${PEER_NAME}=${PEER_PRIVATE_IP}"
}

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

install_hosts() {
  local tmp
  tmp=$(mktemp)
  cp /etc/hosts "$tmp"
  python3 - "$tmp" "$(default_cluster_hosts)" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
entries = []
for item in sys.argv[2].split(","):
    name, ip = item.split("=", 1)
    entries.append((name.strip(), ip.strip()))
lines = path.read_text(encoding="utf-8").splitlines()
filtered = []
cluster_names = {name for name, _ in entries}
for line in lines:
    stripped = line.strip()
    if not stripped:
        filtered.append(line)
        continue
    parts = stripped.split()
    names = set(parts[1:])
    if cluster_names.intersection(names):
        continue
    filtered.append(line)
for name, ip in entries:
    filtered.append(f"{ip} {name}")
path.write_text("\n".join(filtered) + "\n", encoding="utf-8")
PY
  sudo cp "$tmp" /etc/hosts
  rm -f "$tmp"
}

mkdir -p "$BIN_DIR" "$LOG_DIR" "$CONFIG_DIR" "$RESULTS_DIR"
IFS=',' read -r -a drive_paths <<<"$(resolve_drive_paths)"
for drive_path in "${drive_paths[@]}"; do
  sudo mkdir -p "$drive_path"
  sudo chown "$(id -u)":"$(id -g)" "$drive_path"
done

ensure_python
install_hosts

cat <<EOF
root_dir=$ROOT_DIR
bin_dir=$BIN_DIR
results_dir=$RESULTS_DIR
node_name=$NODE_NAME
cluster_hosts=$(default_cluster_hosts)
data_root=$DATA_ROOT
drive_count=${#drive_paths[@]}
drive_paths=$(IFS=,; echo "${drive_paths[*]}")
EOF
