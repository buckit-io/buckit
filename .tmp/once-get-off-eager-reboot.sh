#!/usr/bin/env bash
set -euo pipefail

PEM=${PEM:-/Users/rooseveltlai/Downloads/buckit.pem}
N1=${N1:-54.235.32.111}
N2=${N2:-98.83.23.248}
C=${C:-100.24.17.87}
HOSTS=${HOSTS:-172.31.34.45:9000,172.31.37.173:9000}
MC=${MC:-/home/ubuntu/singletrip-bench/bin/mc}
ONCE_GET=${ONCE_GET:-/home/ubuntu/once_get.py}
BINARY=${BINARY:-buckit-rfs-trace}
CONCURRENCY=${CONCURRENCY:-32}
SEED=${SEED:-17}
TIMEOUT=${TIMEOUT:-300}
KEYS_FILE=${KEYS_FILE:-}
ONCE_GET_ARGS=${ONCE_GET_ARGS:-}
ORDER=${ORDER:-OFF,EAGER}
SETTLE_SECONDS=${SETTLE_SECONDS:-20}
OUT=${OUT:-/tmp/st-hdd-bench/once-get-off-eager-$(date +%H%M%S)}

mkdir -p "$OUT"

ssh_cmd() {
  ssh -i "$PEM" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=8 -o BatchMode=yes "$@"
}

scp_cmd() {
  scp -i "$PEM" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=8 "$@"
}

wait_ssh() {
  local host=$1
  for _ in $(seq 1 80); do
    if ssh_cmd "ubuntu@$host" true >/dev/null 2>&1; then
      return 0
    fi
    sleep 3
  done
  return 1
}

wait_healthy() {
  for _ in $(seq 1 60); do
    if ssh_cmd "ubuntu@$N1" "$MC alias set bench http://127.0.0.1:9000 buckitadmin buckitadmin >/dev/null 2>&1; $MC admin info bench 2>/dev/null" | grep -q '6 drives online'; then
      return 0
    fi
    sleep 3
  done
  return 1
}

wait_serving() {
  for _ in $(seq 1 40); do
    code=$(ssh_cmd "ubuntu@$N1" "curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:9000/minio/health/ready" 2>/dev/null || true)
    if [[ "$code" == 200 ]]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

extract_log_path() {
  awk '{for (i = 1; i <= NF; i++) if ($i ~ /^log=/) { sub(/^log=/, "", $i); print $i; exit }}' "$1"
}

reboot_nodes() {
  for host in "$N1" "$N2"; do
    ssh_cmd "ubuntu@$host" "pkill -x buckit 2>/dev/null || true; cp /home/ubuntu/singletrip-bench/bin/$BINARY /home/ubuntu/singletrip-bench/bin/buckit.new && mv /home/ubuntu/singletrip-bench/bin/buckit.new /home/ubuntu/singletrip-bench/bin/buckit; sudo reboot" >/dev/null 2>&1 || true
  done
  sleep 30
  wait_ssh "$N1"
  wait_ssh "$N2"
}

launch_arm() {
  local tag=$1
  local fast_get=$2
  local eager=$3
  local spread=${4:-0}
  local hedge=${5:-0}
  for pair in "$N1:node1" "$N2:node2"; do
    host=${pair%:*}
    node=${pair#*:}
    ssh_cmd "ubuntu@$host" "env -u BUCKIT_FASTGET_NO_FALLBACK -u BUCKIT_FASTGET_EAGER_SELECTED BUCKIT_FASTGET_EAGER=$eager BUCKIT_FASTGET_SPREAD=$spread BUCKIT_FASTGET_HEDGE=$hedge bash /home/ubuntu/singletrip-bench/run/launch.sh $fast_get EC:2" > "$OUT/${tag}-launch-${node}.txt"
  done
  wait_healthy
  wait_serving
  if [[ "$SETTLE_SECONDS" != "0" ]]; then
    sleep "$SETTLE_SECONDS"
  fi
}

run_once_get() {
  local tag=$1
  local key_arg=""
  if [[ -n "$KEYS_FILE" ]]; then
    key_arg="--keys-file=$KEYS_FILE"
  fi
  ssh_cmd "ubuntu@$C" "python3 $ONCE_GET --host=$HOSTS --bucket=warp-benchmark-bucket --access-key=buckitadmin --secret-key=buckitadmin --concurrent=$CONCURRENCY --timeout=$TIMEOUT $key_arg --shuffle --seed=$SEED $ONCE_GET_ARGS --output=/home/ubuntu/${tag}-once-get.csv --progress-every=1000" > "$OUT/${tag}-once-get.txt" 2>&1
  scp_cmd "ubuntu@$C:/home/ubuntu/${tag}-once-get.csv" "$OUT/${tag}-once-get.csv"
  for pair in "$N1:node1" "$N2:node2"; do
    host=${pair%:*}
    node=${pair#*:}
    log_path=$(extract_log_path "$OUT/${tag}-launch-${node}.txt")
    scp_cmd "ubuntu@$host:$log_path" "$OUT/${tag}-${node}.server.log"
  done
}

run_arm() {
  local tag=$1
  local fast_get=$2
  local eager=$3
  local spread=${4:-0}
  local hedge=${5:-0}
  echo "== $tag reboot ==" | tee -a "$OUT/summary.txt"
  reboot_nodes
  echo "== $tag launch ==" | tee -a "$OUT/summary.txt"
  launch_arm "$tag" "$fast_get" "$eager" "$spread" "$hedge"
  echo "== $tag once-get ==" | tee -a "$OUT/summary.txt"
  run_once_get "$tag"
  tail -n 3 "$OUT/${tag}-once-get.txt" | tee -a "$OUT/summary.txt"
}

IFS=',' read -r -a arms <<< "$ORDER"
for arm in "${arms[@]}"; do
  case "$arm" in
    OFF)
      run_arm OFF 0 0 0
      ;;
    EAGER)
      run_arm EAGER 1 1 1
      ;;
    EAGERSTABLE)
      run_arm EAGERSTABLE 1 1 1
      ;;
    EAGERSTABLEHEDGE)
      run_arm EAGERSTABLEHEDGE 1 1 1 1
      ;;
    *)
      echo "unknown arm in ORDER: $arm" >&2
      exit 2
      ;;
  esac
done

printf 'artifacts=%s\n' "$OUT" | tee -a "$OUT/summary.txt"
