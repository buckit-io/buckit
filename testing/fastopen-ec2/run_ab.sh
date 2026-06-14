#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=${ROOT_DIR:-$HOME/buckit-fastopen-ec2}
BIN_DIR=${BIN_DIR:-$ROOT_DIR/bin}
RUNNER=${RUNNER:-$BIN_DIR/fastopen_local.py}
START_SCRIPT=${START_SCRIPT:-$BIN_DIR/start_cluster.sh}
STOP_SCRIPT=${STOP_SCRIPT:-$BIN_DIR/stop_cluster.sh}
SSH_KEY=${SSH_KEY:-$HOME/.ssh/buckit-fastopen-cluster}
PEER_HOST=${PEER_HOST:-buckit-node2}
REMOTE_HOSTS=${REMOTE_HOSTS:-$PEER_HOST}
HOST=${HOST:-127.0.0.1:9000}
METRICS_URL=${METRICS_URL:-http://127.0.0.1:9000/minio/metrics/v3/api/requests}
ACCESS_KEY=${ACCESS_KEY:-buckitadmin}
SECRET_KEY=${SECRET_KEY:-buckitadmin}
BUCKET=${BUCKET:-fastopen-bench}
OBJECT_SIZE=${OBJECT_SIZE:-640KiB}
OBJECT_COUNT=${OBJECT_COUNT:-200}
CACHE_PROFILE=${CACHE_PROFILE:-1x-all}
ORDERING=${ORDERING:-key-order}
CONCURRENCY=${CONCURRENCY:-1}
REGION=${REGION:-us-east-1}
WAIT_SECONDS=${WAIT_SECONDS:-60}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
RESULT_DIR=${RESULT_DIR:-$ROOT_DIR/results/$RUN_ID}
KEY_PREFIX=${KEY_PREFIX:-obj-ec2-}
KEYS_FILE=${KEYS_FILE:-$RESULT_DIR/keys.txt}

mkdir -p "$RESULT_DIR"

remote_ssh() {
  ssh -i "$SSH_KEY" \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=accept-new \
    "ec2-user@${PEER_HOST}" \
    "$@"
}

remote_all() {
  local cmd=$1
  local remote
  IFS=',' read -r -a remotes <<<"$REMOTE_HOSTS"
  for remote in "${remotes[@]}"; do
    ssh -i "$SSH_KEY" \
      -o BatchMode=yes \
      -o StrictHostKeyChecking=accept-new \
      "ec2-user@${remote}" \
      "$cmd"
  done
}

wait_ready() {
  for _ in $(seq 1 "$WAIT_SECONDS"); do
    code=$(curl -sS -o /dev/null -w '%{http_code}' "http://${HOST}/minio/health/ready" || true)
    if [[ "$code" == "200" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "cluster did not become ready on ${HOST}" >&2
  return 1
}

stop_cluster() {
  "$STOP_SCRIPT" || true
  remote_all "$STOP_SCRIPT" || true
}

start_arm() {
  local fast_get=$1
  stop_cluster
  remote_all "FAST_GET=${fast_get} $START_SCRIPT"
  FAST_GET="$fast_get" "$START_SCRIPT"
  wait_ready
}

run_seed() {
  python3 "$RUNNER" seed \
    --host "$HOST" \
    --bucket "$BUCKET" \
    --access-key "$ACCESS_KEY" \
    --secret-key "$SECRET_KEY" \
    --region "$REGION" \
    --object-size "$OBJECT_SIZE" \
    --object-count "$OBJECT_COUNT" \
    --key-prefix "$KEY_PREFIX" \
    --keys-output "$KEYS_FILE" \
    --overwrite
}

run_arm() {
  local arm=$1
  local lower
  lower=$(printf '%s' "$arm" | tr '[:upper:]' '[:lower:]')
  python3 "$RUNNER" run \
    --host "$HOST" \
    --bucket "$BUCKET" \
    --access-key "$ACCESS_KEY" \
    --secret-key "$SECRET_KEY" \
    --region "$REGION" \
    --keys-file "$KEYS_FILE" \
    --concurrency "$CONCURRENCY" \
    --cache-profile "$CACHE_PROFILE" \
    --ordering "$ORDERING" \
    --path-arm "$arm" \
    --object-size-label "$OBJECT_SIZE" \
    --output-csv "$RESULT_DIR/${lower}.csv" \
    --summary-json "$RESULT_DIR/${lower}.json" \
    --metrics-before "$RESULT_DIR/${lower}.metrics.before.json" \
    --metrics-after "$RESULT_DIR/${lower}.metrics.after.json" \
    --metrics-url "$METRICS_URL"
}

if [[ ! -x "$START_SCRIPT" ]]; then
  echo "missing start script: $START_SCRIPT" >&2
  exit 1
fi
if [[ ! -x "$STOP_SCRIPT" ]]; then
  echo "missing stop script: $STOP_SCRIPT" >&2
  exit 1
fi
if [[ ! -f "$RUNNER" ]]; then
  echo "missing runner: $RUNNER" >&2
  exit 1
fi
if [[ ! -f "$SSH_KEY" ]]; then
  echo "missing SSH key: $SSH_KEY" >&2
  exit 1
fi

remote_all "true"
start_arm 0
run_seed
run_arm OFF
start_arm 1
run_arm ON

python3 - "$RESULT_DIR/off.json" "$RESULT_DIR/on.json" >"$RESULT_DIR/compare.txt" <<'PY'
import json
import sys

off = json.load(open(sys.argv[1], encoding="utf-8"))
on = json.load(open(sys.argv[2], encoding="utf-8"))

def grab(doc, group, metric, field):
    return doc["results"][group][metric][field]

rows = [
    ("TTFB mean", "all_requests", "ttfb_ms", "mean"),
    ("TTFB p50", "all_requests", "ttfb_ms", "p50"),
    ("TTFB p90", "all_requests", "ttfb_ms", "p90"),
    ("total mean", "all_requests", "total_ms", "mean"),
    ("total p50", "all_requests", "total_ms", "p50"),
    ("total p90", "all_requests", "total_ms", "p90"),
]

for label, group, metric, field in rows:
    off_v = grab(off, group, metric, field)
    on_v = grab(on, group, metric, field)
    delta = on_v - off_v
    pct = (delta / off_v * 100.0) if off_v else 0.0
    print(f"{label}: OFF={off_v:.3f} ON={on_v:.3f} delta={delta:+.3f}ms ({pct:+.1f}%)")
PY

echo "result_dir=$RESULT_DIR"
cat "$RESULT_DIR/compare.txt"
