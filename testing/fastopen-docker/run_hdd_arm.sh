#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 5 ]; then
  echo "usage: run_hdd_arm.sh <arm:on|off> <run_tag> <object_count> <key_prefix> <ssh_base_port>" >&2
  exit 1
fi

ARM=$1
RUN_TAG=$2
OBJECT_COUNT=$3
KEY_PREFIX=$4
SSH_BASE_PORT=$5

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$ROOT_DIR/../.." && pwd)
cd "$REPO_ROOT"

export SKIP_BUCKIT_BUILD=1
export HDD_DELAY_MS=8
export CPUS=1.0
export CLUSTER_NAME=buckit-linux-hdd-640k-cold
export NODES=4
export DRIVES=4
export SSH_BASE_PORT

HOST=127.0.0.1:9000
ACCESS_KEY=buckitadmin
SECRET_KEY=buckitadmin
RESULT_DIR=${RESULT_DIR:-/home/rooseveltlai/buckit-linux-results}
SIZE_LABEL=640KiB

ensure_delay_target() {
  sudo modprobe dm_delay 2>/dev/null || true
  if ! sudo dmsetup targets | grep -q '^delay[[:space:]]'; then
    echo "dm-delay target is unavailable on host even after modprobe" >&2
    exit 1
  fi
}

wait_node_ready() {
  for _ in $(seq 1 180); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://${HOST}/minio/health/ready" || true)
    if [ "$code" = "200" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

seed_with_retry() {
  local prefix=$1 keys=$2
  for attempt in $(seq 1 20); do
    if python3 testing/fastopen-local/fastopen_local.py seed \
      --host "${HOST}" \
      --bucket fastopen-bench \
      --access-key "${ACCESS_KEY}" \
      --secret-key "${SECRET_KEY}" \
      --object-size "${SIZE_LABEL}" \
      --object-count "${OBJECT_COUNT}" \
      --key-prefix "${prefix}" \
      --keys-output "${keys}" \
      --overwrite; then
      return 0
    fi
    echo "seed retry ${attempt}/20"
    sleep 3
  done
  return 1
}

KEYS=${RESULT_DIR}/buckit-linux-hdd-640k-cold-${ARM}-${RUN_TAG}.keys
OUT_CSV=${RESULT_DIR}/buckit-linux-hdd-640k-cold-${ARM}-${RUN_TAG}.csv
OUT_JSON=${RESULT_DIR}/buckit-linux-hdd-640k-cold-${ARM}-${RUN_TAG}.json
NODE1_TXT=${RESULT_DIR}/buckit-linux-hdd-640k-cold-${ARM}-${RUN_TAG}.node1.txt

mkdir -p "${RESULT_DIR}"
ensure_delay_target
./testing/fastopen-docker/rig.sh destroy >/dev/null 2>&1 || true
if [ "${ARM}" = "on" ]; then
  ./testing/fastopen-docker/rig.sh start-on
  PATH_ARM=ON
else
  ./testing/fastopen-docker/rig.sh start-off
  PATH_ARM=OFF
fi

wait_node_ready
sleep 10
seed_with_retry "${KEY_PREFIX}" "${KEYS}"

python3 testing/fastopen-local/fastopen_local.py run \
  --host "${HOST}" \
  --bucket fastopen-bench \
  --access-key "${ACCESS_KEY}" \
  --secret-key "${SECRET_KEY}" \
  --keys-file "${KEYS}" \
  --concurrency 1 \
  --cache-profile 1x-all \
  --ordering key-order \
  --metrics-url '' \
  --path-arm "${PATH_ARM}" \
  --object-size-label "${SIZE_LABEL}" \
  --output-csv "${OUT_CSV}" \
  --summary-json "${OUT_JSON}"

sudo docker exec "${CLUSTER_NAME}-node1" sh -lc '
  echo "[dmsetup]"
  dmsetup ls --target delay --noheadings 2>/dev/null | awk "NR==1{print \$1}" | xargs -r dmsetup table
  echo "[cpu.max]"
  cat /sys/fs/cgroup/cpu.max 2>/dev/null || true
' >"${NODE1_TXT}"

./testing/fastopen-docker/rig.sh destroy >/dev/null 2>&1 || true
