## FastOpen Docker ON Run: 2026-06-13

This note records the clean `BUCKIT_FAST_GET=1` ON-arm run that was executed on
`ubuntudell` against the local 2-container Docker Buckit cluster.

### Cluster

- Host: `ubuntudell`
- Topology: `2` Buckit containers, `4` drives per container
- Drive backing: host-backed loop image files
- Per-container limits:
  - CPU: `1`
  - Memory: `2 GiB`
- S3 endpoints:
  - node1: `http://127.0.0.1:9000`
  - node2: `http://127.0.0.1:9002`

### Run

- Run type: mixed replay against the already-seeded bucket
- Bucket: `docker-mixed-seed`
- Duration: `12h`
- Concurrency: `5 + 5`
- Delay: `100ms - 500ms`
- Mix:
  - `GET 88`
  - `PUT 1`
  - `DELETE 1`
- Run directory on host:
  - `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z`

### Primary Artifacts

Replay outputs:

- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/node1.stdout.log`
- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/node2.stdout.log`
- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/node1.csv`
- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/node2.csv`
- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/node1.json`
- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/node2.json`
- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/keys-node1.txt`
- `/home/rooseveltlai/buckit-docker-rig/results/replay-12h-5x2-20260613T010332Z/keys-node2.txt`

Container snapshot captured after the run:

- `/home/rooseveltlai/buckit-docker-rig/results/container-snapshot-20260613T131557Z`

For each container snapshot:

- `docker-inspect.json`
- `docker-stdout-stderr.log`
- `container-state.txt`
- `buckit-journal-since-run.log`
- `pid1-environ.txt`
- `systemd-files.txt`
- `etc-systemd-system.tar`
- `var-lib-systemd.tar`
- `var-log.tar`
- `os-release.txt`

Host-backed raw Buckit drive images:

- `/home/rooseveltlai/buckit-docker-data/buckit-fastopen-2n/node1`
- `/home/rooseveltlai/buckit-docker-data/buckit-fastopen-2n/node2`

Sizes at capture time:

- Replay run directory: `168M`
- Container snapshot: `112M`
- Node1 drive backing: `59G`
- Node2 drive backing: `59G`

### Replay Result Summary

Node1 final summary:

- completed: `608,989`
- GET: `595,466`
- PUT: `6,810`
- DELETE: `6,713`
- GET latency:
  - total ms: `mean 48.87`, `p50 31.36`, `p90 102.16`, `p99 280.39`
  - TTFB ms: `mean 45.40`, `p50 28.48`, `p90 96.61`, `p99 266.39`
- PUT total ms:
  - `mean 428.40`, `p50 419.93`, `p90 675.99`, `p99 1011.60`
- DELETE total ms:
  - `mean 33.66`, `p50 23.15`, `p90 59.18`, `p99 181.90`
- errors:
  - GET `0`
  - PUT `0`
  - DELETE `0`

Node2 final summary:

- completed: `608,230`
- GET: `594,869`
- PUT: `6,744`
- DELETE: `6,617`
- GET latency:
  - total ms: `mean 49.40`, `p50 31.29`, `p90 104.63`, `p99 285.41`
  - TTFB ms: `mean 45.81`, `p50 28.39`, `p90 98.53`, `p99 270.65`
- PUT total ms:
  - `mean 430.53`, `p50 420.83`, `p90 683.48`, `p99 969.01`
- DELETE total ms:
  - `mean 34.85`, `p50 23.37`, `p90 64.80`, `p99 186.12`
- errors:
  - GET `0`
  - PUT `0`
  - DELETE `0`

### Observations

1. The ON-arm run completed cleanly.
   - No replay window errors were recorded.
   - No FastOpen fallback or final-error activity was observed in the clean run.

2. GET median latency stayed stable.
   - Hourly `get_ttfb_ms p50` stayed roughly in the `27.5ms - 28.8ms` band.
   - Hourly `get_total_ms p50` stayed roughly in the `30.3ms - 31.7ms` band.

3. GET `p90` was higher than the first warm-up hour, but broadly stable after
   warm-up.
   - Excluding the first hour, hourly `get_ttfb_ms p90` stayed roughly in the
     `94ms - 103ms` band.
   - Excluding the first hour, hourly `get_total_ms p90` stayed roughly in the
     `100ms - 109ms` band.

4. PUT and DELETE did not show a clear time-based degradation trend after
   warm-up.
   - PUT was noisier than GET, but remained in a stable band.
   - DELETE stayed stable.

5. Host load was fairly stable after warm-up.
   - `host_load5` mostly stayed in the `~6.7 - 8.1` range.

6. Host memory used percentage climbed gradually over the run, but host memory
   availability remained healthy.
   - The growth is consistent with page cache / reclaimable cache growth, not a
     host-wide memory shortage.

7. The Docker containers were repeatedly colliding with their `2 GiB` cgroup
   hard memory limit.
   - `memory.current` was effectively at `memory.max`.
   - `memory.events:max` was very high on both containers.
   - This indicates real container-local memory pressure from cache and charged
     memory, even though the host still had ample free memory.

### Interpretation

This ON-arm run is clean enough to use as the baseline before switching to the
OFF arm (`BUCKIT_FAST_GET=0`).

The key outcome from this run is:

- no operational errors
- no FastOpen fallback behavior
- stable `p50`
- stable post-warm-up `p90`
- observed pressure is dominated by the Docker/loop/XFS/cgroup environment, not
  by an obvious FastOpen-specific failure mode

The next comparison should keep the same:

- seeded bucket
- replay key shards
- concurrency (`5 + 5`)
- delay (`100ms - 500ms`)
- container memory cap (`2 GiB`)

and only change:

- `BUCKIT_FAST_GET=1` -> `BUCKIT_FAST_GET=0`
