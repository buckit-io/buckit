#!/usr/bin/env bash
set -euo pipefail

# Replays a pre-seeded mixed-object keyset against four Buckit ingress nodes.
# This is the local copy of the final EC2 rig used for the 100k mixed run:
# 80k x 640KiB + 20k x 2MiB, 4 targets, 20 concurrency per target.

KEY=${KEY:?set KEY to the EC2 SSH private key}
LOADGEN_PUB=${LOADGEN_PUB:?set LOADGEN_PUB to the load generator public IP}
MASTER_KEYS_FILE=${MASTER_KEYS_FILE:?set MASTER_KEYS_FILE on the load generator}
RUN_ROOT_LOCAL=${RUN_ROOT_LOCAL:-/tmp/buckit-fastopen-west1-results/$(date -u +%Y%m%dT%H%M%SZ)-mixed-fanout}

STORAGE_PUBS_CSV=${STORAGE_PUBS_CSV:?comma-separated storage public IPs}
STORAGE_PRIVS_CSV=${STORAGE_PRIVS_CSV:?comma-separated storage private IPs}
STORAGE_NAMES_CSV=${STORAGE_NAMES_CSV:-node1,node2,node3,node4}
CLUSTER_NODES=${CLUSTER_NODES:-buckit-node1,buckit-node2,buckit-node3,buckit-node4}
DRIVE_PATHS=${DRIVE_PATHS:-/mnt/data1/buckit-drive1,/mnt/data2/buckit-drive2,/mnt/data3/buckit-drive3,/mnt/data4/buckit-drive4}
MINIO_STORAGE_CLASS_STANDARD=${MINIO_STORAGE_CLASS_STANDARD:-EC:4}
PER_TARGET_CONCURRENCY=${PER_TARGET_CONCURRENCY:-20}
OBJECT_SIZE_LABEL=${OBJECT_SIZE_LABEL:-mixed-80k-640KiB-20k-2MiB}
START_FROM=${START_FROM:-round1-on}
ROUNDS=${ROUNDS:-round1-on:ON:1,round1-off:OFF:0,round2-off:OFF:0,round2-on:ON:1,round3-on:ON:1,round3-off:OFF:0}

IFS=',' read -r -a STORAGE_PUBS <<<"$STORAGE_PUBS_CSV"
IFS=',' read -r -a STORAGE_PRIVS <<<"$STORAGE_PRIVS_CSV"
IFS=',' read -r -a STORAGE_NAMES <<<"$STORAGE_NAMES_CSV"

if [[ ${#STORAGE_PUBS[@]} -ne 4 || ${#STORAGE_PRIVS[@]} -ne 4 || ${#STORAGE_NAMES[@]} -ne 4 ]]; then
  echo "expected exactly four storage public IPs, private IPs, and names" >&2
  exit 1
fi

mkdir -p "$RUN_ROOT_LOCAL"

ssh_ec2() {
  local host=$1
  shift
  ssh -o StrictHostKeyChecking=no -i "$KEY" "ec2-user@$host" "$@"
}

scp_from() {
  local src_host=$1
  local src_path=$2
  local dst=$3
  mkdir -p "$dst"
  scp -r -o StrictHostKeyChecking=no -i "$KEY" "ec2-user@$src_host:$src_path" "$dst/"
}

wait_ssh() {
  local host=$1
  for _ in $(seq 1 120); do
    if ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no -i "$KEY" "ec2-user@$host" true >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  echo "ssh not ready: $host" >&2
  return 1
}

reboot_all() {
  local host
  for host in "${STORAGE_PUBS[@]}" "$LOADGEN_PUB"; do
    ssh -o StrictHostKeyChecking=no -i "$KEY" "ec2-user@$host" "sudo reboot" >/dev/null 2>&1 || true
  done
  sleep 15
  for host in "${STORAGE_PUBS[@]}" "$LOADGEN_PUB"; do
    wait_ssh "$host"
  done
}

ensure_storage_ready() {
  local host
  for host in "${STORAGE_PUBS[@]}"; do
    ssh_ec2 "$host" 'for mp in /mnt/data1 /mnt/data2 /mnt/data3 /mnt/data4; do findmnt "$mp" >/dev/null; done'
  done
}

start_cluster_all() {
  local fastget=$1
  local host
  for host in "${STORAGE_PUBS[@]}"; do
    ssh_ec2 "$host" "cd ~/buckit-fastopen-ec2 && ./stop_cluster.sh || true && rm -f logs/buckit.pid && CLUSTER_NODES='$CLUSTER_NODES' DRIVE_PATHS='$DRIVE_PATHS' MINIO_STORAGE_CLASS_STANDARD='$MINIO_STORAGE_CLASS_STANDARD' FAST_GET=$fastget ./start_cluster.sh"
  done
}

wait_cluster_ready() {
  for _ in $(seq 1 180); do
    local ok=1
    local host code
    for host in "${STORAGE_PUBS[@]}"; do
      code=$(ssh_ec2 "$host" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9000/minio/health/ready || true")
      [[ "$code" == "200" ]] || ok=0
    done
    [[ $ok -eq 1 ]] && return 0
    sleep 2
  done
  echo "cluster not ready" >&2
  return 1
}

prepare_shards() {
  local arm=$1
  ssh_ec2 "$LOADGEN_PUB" "RUN_DIR=/home/ec2-user/buckit-fastopen-loadgen/results/$arm; rm -rf \"\$RUN_DIR\"; mkdir -p \"\$RUN_DIR\"; python3 - <<'PY'
from pathlib import Path
master = Path('$MASTER_KEYS_FILE')
run_dir = Path('/home/ec2-user/buckit-fastopen-loadgen/results/$arm')
files = [run_dir / f'keys-shard-{i}.txt' for i in range(1, 5)]
out = [f.open('w', encoding='utf-8') for f in files]
try:
    for idx, line in enumerate(master.read_text(encoding='utf-8').splitlines()):
        out[idx % 4].write(line + '\n')
finally:
    for f in out:
        f.close()
PY"
}

start_monitors() {
  local arm=$1
  local idx host name peer_ip
  for idx in 0 1 2 3; do
    host=${STORAGE_PUBS[$idx]}
    name=${STORAGE_NAMES[$idx]}
    peer_ip=${STORAGE_PRIVS[0]}
    [[ $idx -eq 0 ]] && peer_ip=${STORAGE_PRIVS[1]}
    ssh_ec2 "$host" "RUN_DIR=/home/ec2-user/buckit-fastopen-ec2/results/$arm; rm -rf \"\$RUN_DIR\"; mkdir -p \"\$RUN_DIR\"; \
      nohup python3 ~/buckit-fastopen-ec2/monitor_host.py --interval 1.0 --output-csv \"\$RUN_DIR/$arm.$name.host.csv\" --summary-json \"\$RUN_DIR/$arm.$name.host.json\" >/dev/null 2>&1 & echo \$! > \"\$RUN_DIR/$arm.$name.host.pid\"; \
      nohup python3 ~/buckit-fastopen-ec2/monitor_go_gc.py --interval 1.0 --url http://127.0.0.1:9000/minio/metrics/v3/debug/go --output-csv \"\$RUN_DIR/$arm.$name.gc.csv\" --summary-json \"\$RUN_DIR/$arm.$name.gc.json\" >/dev/null 2>&1 & echo \$! > \"\$RUN_DIR/$arm.$name.gc.pid\"; \
      nohup python3 ~/buckit-fastopen-ec2/monitor_sockets.py --interval 1.0 --peer-ip $peer_ip --port 9000 --output-csv \"\$RUN_DIR/$arm.$name.sockets.csv\" --summary-json \"\$RUN_DIR/$arm.$name.sockets.json\" >/dev/null 2>&1 & echo \$! > \"\$RUN_DIR/$arm.$name.sockets.pid\""
    ssh_ec2 "$LOADGEN_PUB" "RUN_DIR=/home/ec2-user/buckit-fastopen-loadgen/results/$arm; mkdir -p \"\$RUN_DIR\"; \
      nohup python3 ~/buckit-fastopen-loadgen/monitor_sockets.py --interval 1.0 --peer-ip ${STORAGE_PRIVS[$idx]} --port 9000 --output-csv \"\$RUN_DIR/$arm.loadgen.target$((idx+1)).sockets.csv\" --summary-json \"\$RUN_DIR/$arm.loadgen.target$((idx+1)).sockets.json\" >/dev/null 2>&1 & echo \$! > \"\$RUN_DIR/$arm.loadgen.target$((idx+1)).sockets.pid\""
  done
  ssh_ec2 "$LOADGEN_PUB" "RUN_DIR=/home/ec2-user/buckit-fastopen-loadgen/results/$arm; nohup python3 ~/buckit-fastopen-loadgen/monitor_host.py --interval 1.0 --output-csv \"\$RUN_DIR/$arm.loadgen.host.csv\" --summary-json \"\$RUN_DIR/$arm.loadgen.host.json\" >/dev/null 2>&1 & echo \$! > \"\$RUN_DIR/$arm.loadgen.host.pid\""
}

stop_monitors() {
  local arm=$1
  local host
  for host in "${STORAGE_PUBS[@]}" "$LOADGEN_PUB"; do
    ssh_ec2 "$host" "for dir in /home/ec2-user/buckit-fastopen-ec2/results/$arm /home/ec2-user/buckit-fastopen-loadgen/results/$arm; do [ -d \"\$dir\" ] || continue; for pidf in \"\$dir\"/*.pid; do [ -f \"\$pidf\" ] || continue; kill \$(cat \"\$pidf\") >/dev/null 2>&1 || true; done; done"
  done
  sleep 2
}

sync_arm() {
  local arm=$1
  local idx
  mkdir -p "$RUN_ROOT_LOCAL/$arm"
  scp_from "$LOADGEN_PUB" "/home/ec2-user/buckit-fastopen-loadgen/results/$arm" "$RUN_ROOT_LOCAL/$arm"
  for idx in 0 1 2 3; do
    scp_from "${STORAGE_PUBS[$idx]}" "/home/ec2-user/buckit-fastopen-ec2/results/$arm" "$RUN_ROOT_LOCAL/$arm/${STORAGE_NAMES[$idx]}"
  done
}

aggregate_arm() {
  local arm=$1
  python3 - "$RUN_ROOT_LOCAL/$arm/$arm" "$arm" <<'PY'
import csv, json, math, statistics, sys
from pathlib import Path
run_dir = Path(sys.argv[1])
arm = sys.argv[2]
start_ts = float((run_dir / f"{arm}.start_ts").read_text().strip())
end_ts = float((run_dir / f"{arm}.end_ts").read_text().strip())
rows = []
bytes_read = 0
errors = 0
for csv_path in sorted(run_dir.glob(f"{arm}.target*.csv")):
    if csv_path.name.endswith(".plan.csv"):
        continue
    with csv_path.open() as f:
        for row in csv.DictReader(f):
            rows.append(row)
            bytes_read += int(row.get("bytes") or 0)
            errors += (row.get("status") or "") != "200"
def pct(vals, p):
    vals = sorted(vals)
    k = (len(vals) - 1) * p / 100
    f = math.floor(k)
    c = math.ceil(k)
    return vals[f] if f == c else vals[f] * (c - k) + vals[c] * (k - f)
def stat(vals):
    return {"count": len(vals), "min": min(vals), "mean": statistics.fmean(vals), "p50": pct(vals, 50), "p90": pct(vals, 90), "p99": pct(vals, 99), "max": max(vals)}
ttfb = [float(r["ttfb_ms"]) for r in rows]
total = [float(r["total_ms"]) for r in rows]
out = {
    "path_arm": arm.split("-")[-1].upper(),
    "concurrency": 80,
    "object_size": "mixed",
    "request_count": len(rows),
    "elapsed_seconds": end_ts - start_ts,
    "results": {"all_requests": {"bytes_read": bytes_read, "errors": errors, "ttfb_ms": stat(ttfb), "total_ms": stat(total)}},
}
(run_dir / f"{arm}.aggregate.json").write_text(json.dumps(out, indent=2))
PY
}

run_arm() {
  local arm=$1
  local path_arm=$2
  local fastget=$3
  echo "=== $arm start ==="
  reboot_all
  ensure_storage_ready
  start_cluster_all "$fastget"
  wait_cluster_ready
  sleep 15
  prepare_shards "$arm"
  start_monitors "$arm"
  ssh_ec2 "$LOADGEN_PUB" "RUN_DIR=/home/ec2-user/buckit-fastopen-loadgen/results/$arm; start_ts=\$(date +%s.%N); pids=''; \
    for idx in 1 2 3 4; do \
      case \$idx in 1) host_ip='${STORAGE_PRIVS[0]}';; 2) host_ip='${STORAGE_PRIVS[1]}';; 3) host_ip='${STORAGE_PRIVS[2]}';; 4) host_ip='${STORAGE_PRIVS[3]}';; esac; \
      python3 ~/buckit-fastopen-loadgen/bin/fastopen_local.py run --host \${host_ip}:9000 --bucket fastopen-bench --access-key buckitadmin --secret-key buckitadmin --region us-east-1 --keys-file \"\$RUN_DIR/keys-shard-\${idx}.txt\" --concurrency $PER_TARGET_CONCURRENCY --cache-profile 1x-all --ordering key-order --path-arm $path_arm --object-size-label $OBJECT_SIZE_LABEL --output-csv \"\$RUN_DIR/$arm.target\${idx}.csv\" --summary-json \"\$RUN_DIR/$arm.target\${idx}.json\" --plan-output \"\$RUN_DIR/$arm.target\${idx}.plan.csv\" > \"\$RUN_DIR/$arm.target\${idx}.stdout.log\" 2>&1 & pids=\"\$pids \$!\"; \
    done; rc=0; for p in \$pids; do wait \$p || rc=1; done; end_ts=\$(date +%s.%N); printf '%s\n' \"\$start_ts\" > \"\$RUN_DIR/$arm.start_ts\"; printf '%s\n' \"\$end_ts\" > \"\$RUN_DIR/$arm.end_ts\"; exit \$rc"
  stop_monitors "$arm"
  sync_arm "$arm"
  aggregate_arm "$arm"
  echo "=== $arm done ==="
}

started=0
IFS=',' read -r -a ARMS <<<"$ROUNDS"
for spec in "${ARMS[@]}"; do
  IFS=':' read -r arm path_arm fastget <<<"$spec"
  if [[ $started -eq 0 && "$arm" != "$START_FROM" ]]; then
    continue
  fi
  started=1
  run_arm "$arm" "$path_arm" "$fastget"
done

echo "$RUN_ROOT_LOCAL"
