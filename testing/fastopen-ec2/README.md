## FastOpen EC2 Rig

This directory contains a small EC2 rig for FastOpen A/B testing without
Docker. The scripts now support multi-node distributed clusters via explicit
cluster-host and drive-path configuration.

Files:

- `node_setup.sh`
  - prepares a host for the distributed Buckit rig
  - installs `python3` when missing
  - creates Buckit drive paths for the configured attached volumes
  - installs host aliases for every node in `CLUSTER_HOSTS`
- `mount_data_volumes.sh`
  - formats and mounts the attached data EBS volumes
  - can persist mounts in `/etc/fstab`
  - expected for the 2-volume-per-node EC2 topology
- `start_cluster.sh`
  - starts the distributed Buckit cluster on a node
  - enables or disables FastOpen with `FAST_GET=0|1`
- `stop_cluster.sh`
  - stops the local Buckit process on a node
- `run_ab.sh`
  - runs a simple `OFF -> ON` sequence from the local ingress node
  - seeds deterministic objects once
  - replays the same GET plan for both arms
  - can start/stop multiple remote nodes via `REMOTE_HOSTS`
  - stores CSV and JSON outputs under `~/buckit-fastopen-ec2/results/<run-id>/`
- `monitor_host.py`
  - samples host CPU and memory from `/proc`
  - writes CSV plus summary JSON
- `monitor_go_gc.py`
  - samples Go runtime metrics from `/minio/metrics/v3/debug/go`
  - records GC count/pause deltas, heap, and goroutines
- `monitor_sockets.py`
  - samples TCP socket states with `ss -tan`
  - filters by peer IP and port for node-to-node traffic
  - records `ESTAB`, `TIME-WAIT`, `SYN-*`, `FIN-*`, and `CLOSE-WAIT`
- `threaded_seed.py`
  - seeds deterministic objects with a threaded uploader
  - intended for real Linux hosts where `fastopen_local.py seed` is too slow

Expected layout:

- root dir: `~/buckit-fastopen-ec2`
- binary: `~/buckit-fastopen-ec2/bin/buckit`
- runner: `~/buckit-fastopen-ec2/bin/fastopen_local.py`
- cluster host map supplied via `CLUSTER_HOSTS=name=ip,name=ip,...`
- per-node drive paths supplied via `DRIVE_PATHS=/path1,/path2,...`

Typical flow:

1. Copy `bin/buckit`, `bin/fastopen_local.py`, and this directory to both hosts.
2. Run `mount_data_volumes.sh` on every storage node for the attached EBS data
   volumes.
3. Run `node_setup.sh` on every storage node with the correct cluster map and
   per-volume drive paths.
4. Copy the cluster SSH private key to the ingress/loadgen host as
   `~/.ssh/buckit-fastopen-cluster`.
5. Start Buckit on every storage node with matching `CLUSTER_NODES` and
   `DRIVE_PATHS`.
6. From the ingress/loadgen host, run:

```sh
~/buckit-fastopen-ec2/bin/run_ab.sh
```

### Planned 8-Node EC2 Topology

The current target topology for the next EC2 run is:

```text
storage nodes: 8 x m6g.large Spot
load generator: 1 x m6g.large Spot
drives: 16 total
drives per node: 2
data volume shape: 2 x gp3 20 GiB attached per storage node
storage class: EC:4
measured target: one fixed ingress host
```

Suggested per-node drive layout:

```text
/mnt/data1/buckit-drive1
/mnt/data2/buckit-drive2
```

Suggested cluster naming:

```text
buckit-node1 ... buckit-node8
```

Example `node_setup.sh` invocation on `buckit-node1`:

```sh
DATA_DEVICES='/dev/nvme1n1,/dev/nvme2n1' \
MOUNT_POINTS='/mnt/data1,/mnt/data2' \
~/buckit-fastopen-ec2/mount_data_volumes.sh

CLUSTER_HOSTS='buckit-node1=10.0.0.11,buckit-node2=10.0.0.12,buckit-node3=10.0.0.13,buckit-node4=10.0.0.14,buckit-node5=10.0.0.15,buckit-node6=10.0.0.16,buckit-node7=10.0.0.17,buckit-node8=10.0.0.18' \
NODE_NAME=buckit-node1 \
DRIVE_PATHS='/mnt/data1/buckit-drive1,/mnt/data2/buckit-drive2' \
~/buckit-fastopen-ec2/node_setup.sh
```

If the device names differ on a given instance, override `DATA_DEVICES`
accordingly. The helper intentionally does not guess arbitrary block-device
ordering.

Example `start_cluster.sh` invocation on every storage node:

```sh
CLUSTER_NODES='buckit-node1,buckit-node2,buckit-node3,buckit-node4,buckit-node5,buckit-node6,buckit-node7,buckit-node8' \
DRIVE_PATHS='/mnt/data1/buckit-drive1,/mnt/data2/buckit-drive2' \
MINIO_STORAGE_CLASS_STANDARD=EC:4 \
FAST_GET=1 \
~/buckit-fastopen-ec2/start_cluster.sh
```

For the measured run, use the dedicated loadgen host and target one fixed Buckit
ingress host rather than spreading requests across multiple endpoints.

## Local Linux Hosts

The same rig works on non-EC2 Linux hosts. The current validated pair is:

```text
host1: ubuntudell  (192.168.4.46)
host2: thinkpad    (192.168.4.47)
ssh user: rooseveltlai
sudo: passwordless on both hosts
```

Bring-up used in the last session:

```sh
rsync -a --delete --exclude '__pycache__' /tmp/buckit-localhosts-rig/ \
  ubuntudell:~/buckit-fastopen-ec2/
rsync -a --delete --exclude '__pycache__' /tmp/buckit-localhosts-rig/ \
  thinkpad:~/buckit-fastopen-ec2/

ssh ubuntudell \
  'NODE_NAME=buckit-node1 LOCAL_PRIVATE_IP=192.168.4.46 \
    PEER_NAME=buckit-node2 PEER_PRIVATE_IP=192.168.4.47 \
    ~/buckit-fastopen-ec2/node_setup.sh'

ssh thinkpad \
  'NODE_NAME=buckit-node2 LOCAL_PRIVATE_IP=192.168.4.47 \
    PEER_NAME=buckit-node1 PEER_PRIVATE_IP=192.168.4.46 \
    ~/buckit-fastopen-ec2/node_setup.sh'

ssh ubuntudell 'FAST_GET=1 ~/buckit-fastopen-ec2/start_cluster.sh'
ssh thinkpad   'FAST_GET=1 ~/buckit-fastopen-ec2/start_cluster.sh'
```

Readiness check:

```sh
ssh ubuntudell 'curl -fsS http://127.0.0.1:9000/minio/health/ready'
ssh thinkpad   'curl -fsS http://127.0.0.1:9000/minio/health/ready'
```

### Parallel Seeding On Real Hosts

For real-host runs, prefer `threaded_seed.py` over `fastopen_local.py seed`.
The built-in seed helper was too slow for `40k x 640KiB` on the Linux-host rig.

Example `5k` split across both hosts:

```sh
RUN_ID=20260611T131931Z-on-5k-conc40

ssh ubuntudell 'cd ~/buckit-fastopen-ec2/results/'"$RUN_ID"' && \
  python3 ~/buckit-fastopen-ec2/threaded_seed.py \
    --endpoint http://127.0.0.1:9000 \
    --bucket fastopen-bench \
    --key-prefix obj-localhosts-5k-conc40- \
    --start-index 1 \
    --object-count 2500 \
    --object-size-bytes 655360 \
    --workers 32'

ssh thinkpad 'cd ~/buckit-fastopen-ec2/results/'"$RUN_ID"' && \
  python3 ~/buckit-fastopen-ec2/threaded_seed.py \
    --endpoint http://127.0.0.1:9000 \
    --bucket fastopen-bench \
    --key-prefix obj-localhosts-5k-conc40- \
    --start-index 2501 \
    --object-count 2500 \
    --object-size-bytes 655360 \
    --workers 32'
```

Generate the shared key file on the client host:

```sh
python3 - <<'PY'
from pathlib import Path
p = Path.home() / "buckit-fastopen-ec2/results/20260611T131931Z-on-5k-conc40/keys-5000.txt"
with p.open("w", encoding="utf-8") as f:
    for i in range(1, 5001):
        f.write(f"obj-localhosts-5k-conc40-{i:06d}\n")
print(p)
PY
```

### Real-Host Concurrency-40 ON Run

The latest validated real-host run used:

```text
run dir: ~/buckit-fastopen-ec2/results/20260611T131931Z-on-5k-conc40
client host: ubuntudell
target host: 127.0.0.1:9000 on ubuntudell
objects: 5000
object size: 640KiB
concurrency: 40
path arm: ON
cache profile: 1x-all
ordering: key-order
```

Result:

```text
elapsed: 253.44s
throughput: 19.73 obj/s, 12.93 MB/s
total p50: 1832.61ms
TTFB p50: 1829.37ms
httptrace reuse: 14842 / 15000 (98.95%)
```

This run did not show a clean late-run collapse. It was slow from the start,
with only mild slice-to-slice wobble.

## Manual Cold A/B Flow

For order-switched runs, client-host switches, or extra monitoring, run the
arms manually instead of using `run_ab.sh`.

Recommended defaults from the FastOpen EC2 tests:

```sh
OBJECT_SIZE=640KiB
OBJECT_COUNT=20000
CONCURRENCY=16
CACHE_PROFILE=1x-all
ORDERING=key-order
MINIO_STORAGE_CLASS_STANDARD=EC:2
```

Cold-arm sequence:

1. Stop Buckit on both nodes with `stop_cluster.sh`.
2. Reboot both nodes.
3. Start both nodes with `FAST_GET=0` or `FAST_GET=1`.
4. Wait for `http://127.0.0.1:9000/minio/health/ready`.
5. Start `monitor_host.py`, `monitor_go_gc.py`, and `monitor_sockets.py` on
   both nodes.
6. Run `fastopen_local.py run` from the chosen client/ingress host.
7. Stop monitors and save their CSV/JSON outputs.
8. Repeat for the other arm without reseeding if the object set is unchanged.

Example monitor startup on node2, where node1 private IP is `172.31.65.107`:

```sh
RUN_DIR=~/buckit-fastopen-ec2/results/$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$RUN_DIR"

nohup python3 ~/buckit-fastopen-ec2/bin/monitor_host.py \
  --interval 1.0 \
  --output-csv "$RUN_DIR/on.node2.host.csv" \
  --summary-json "$RUN_DIR/on.node2.host.json" >/dev/null 2>&1 &
echo $! > "$RUN_DIR/on.node2.host.pid"

nohup python3 ~/buckit-fastopen-ec2/bin/monitor_go_gc.py \
  --interval 1.0 \
  --url http://127.0.0.1:9000/minio/metrics/v3/debug/go \
  --output-csv "$RUN_DIR/on.node2.gc.csv" \
  --summary-json "$RUN_DIR/on.node2.gc.json" >/dev/null 2>&1 &
echo $! > "$RUN_DIR/on.node2.gc.pid"

nohup python3 ~/buckit-fastopen-ec2/bin/monitor_sockets.py \
  --interval 1.0 \
  --peer-ip 172.31.65.107 \
  --port 9000 \
  --output-csv "$RUN_DIR/on.node2.sockets.csv" \
  --summary-json "$RUN_DIR/on.node2.sockets.json" >/dev/null 2>&1 &
echo $! > "$RUN_DIR/on.node2.sockets.pid"
```

Example `run` command:

```sh
python3 ~/buckit-fastopen-ec2/bin/fastopen_local.py run \
  --host 127.0.0.1:9000 \
  --bucket fastopen-bench \
  --access-key buckitadmin \
  --secret-key buckitadmin \
  --region us-east-1 \
  --keys-file "$RUN_DIR/keys-20000.txt" \
  --concurrency 16 \
  --cache-profile 1x-all \
  --ordering key-order \
  --path-arm ON \
  --object-size-label 640KiB \
  --output-csv "$RUN_DIR/on.csv" \
  --summary-json "$RUN_DIR/on.json" \
  --metrics-before "$RUN_DIR/on.metrics.before.json" \
  --metrics-after "$RUN_DIR/on.metrics.after.json" \
  --metrics-url http://127.0.0.1:9000/minio/metrics/v3/api/requests
```

## Result Notes

The runner CSV records request index, latency, status, and bytes. It does not
currently record per-request wall-clock completion time, so exact per-slice
throughput requires either full-run elapsed time or future CSV timestamp
columns. The monitor CSVs do include wall-clock timestamps and can be aligned
to `run_id` start time plus `elapsed_seconds` from the arm summary JSON.

FastOpen HTTP connection-reuse counters, when present in the binary, are
captured by `fastopen_local.py` in the metrics snapshots:

- `minio_api_requests_fast_open_httptrace_connections_total`
- `minio_api_requests_fast_open_httptrace_reused_connections_total`
- `minio_api_requests_fast_open_httptrace_fresh_connections_total`
- `minio_api_requests_fast_open_httptrace_was_idle_connections_total`
