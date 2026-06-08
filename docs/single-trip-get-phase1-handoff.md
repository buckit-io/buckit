# Single-Trip GET Phase 1 Handoff

## Scope

This note summarizes the work completed on the two-host HDD-style benchmark setup for the phase-1 single-trip GET prototype, plus the code change made to fix misleading fast-get fallback metrics.

## Hosts

- `buckit_node1`
  - public IP: `3.81.144.6`
  - private IP: `172.31.44.123`
- `buckit_node2`
  - public IP: `35.173.238.193`
  - private IP: `172.31.37.19`

SSH used:

```sh
ssh -i /Users/rooseveltlai/Downloads/buckit.pem ubuntu@3.81.144.6
ssh -i /Users/rooseveltlai/Downloads/buckit.pem ubuntu@35.173.238.193
```

## Host Setup Completed

Each host now has:

- three XFS-formatted instance-store mounts:
  - `/mnt/data01`
  - `/mnt/data02`
  - `/mnt/data03`
- benchmark workspace:
  - `/home/ubuntu/singletrip-bench`
- deployed Buckit binary:
  - `/home/ubuntu/singletrip-bench/bin/buckit`
- helper scripts copied from repo:
  - `/home/ubuntu/singletrip-bench/run/start-buckit.sh`
  - `/home/ubuntu/singletrip-bench/run/cold-curl-ttfb.sh`
- `mc` installed on `buckit_node1`:
  - `/home/ubuntu/singletrip-bench/bin/mc`

## Cluster Topology

Important detail: Buckit rejected endpoint hostnames with underscores.

This failed:

- `buckit_node1`
- `buckit_node2`

because Buckit reported:

- `FATAL Unable to split host port buckit_node1:9000: invalid hostname`

So the distributed single pool was run with DNS-valid aliases:

- `buckit-node1 -> 172.31.44.123`
- `buckit-node2 -> 172.31.37.19`

Those mappings were written to `/etc/hosts` on both nodes.

Distributed server command shape used on both nodes:

```sh
/home/ubuntu/singletrip-bench/bin/buckit \
  --config-dir /home/ubuntu/singletrip-bench/config \
  server \
  --address :9000 \
  --console-address :9001 \
  http://buckit-node1/mnt/data01 \
  http://buckit-node1/mnt/data02 \
  http://buckit-node1/mnt/data03 \
  http://buckit-node2/mnt/data01 \
  http://buckit-node2/mnt/data02 \
  http://buckit-node2/mnt/data03
```

Live backend info observed via `mc admin info`:

- one pool
- one set
- `6` drives per set
- standard parity `3`
- RRS parity `1`

So the live setup is effectively `EC:3` for standard objects, not `EC:2`.

## Datasets Created

Buckets loaded on the live cluster:

- `singletrip-cold-640k-6d`
- `singletrip-cold-1m-6d`
- `singletrip-cold-2m-6d`

Object counts:

- `10` objects per bucket

Sizes:

- `640 KiB`
- `1 MiB`
- `2 MiB`

Source data on node1:

- `/home/ubuntu/singletrip-bench/data/640k`
- `/home/ubuntu/singletrip-bench/data/1m`
- `/home/ubuntu/singletrip-bench/data/2m`

Presigned URL lists on node1:

- `/home/ubuntu/singletrip-bench/results/urls-640k.txt`
- `/home/ubuntu/singletrip-bench/results/urls-1m.txt`
- `/home/ubuntu/singletrip-bench/results/urls-2m.txt`

The `640 KiB` and `1 MiB` buckets were loaded while `FAST_GET=1` was enabled, and `current/part.1` shadow files were verified to exist.

## Functional Validation Completed

### Byte-for-byte correctness

Validated by downloading objects back on `buckit_node1` and using `cmp` against the original upload files.

Passed:

- `FAST_GET=1`: all 10 objects matched exactly
- `FAST_GET=0`: all 10 objects matched exactly

### Dependency split validation

Validated on `obj-01.bin` across all six drives:

- `FAST_GET=1`
  - renamed away all `xl.meta`
  - GET still returned `HTTP 200`
  - bytes still matched original file

- `FAST_GET=0`
  - renamed away all `current/`
  - GET still returned `HTTP 200`
  - bytes still matched original file

This confirms:

- ON path can serve without `xl.meta`
- OFF path can serve without `current/`

## Code Change Made

### Problem

The fast-get fallback counter was misleading.

Observed before fix:

- immediately after a fresh `FAST_GET=1` restart, before external GETs, `mc` metrics already showed:
  - `fast_get_fallbacks_total = 9`

Root cause:

- internal `.minio.sys` reads used `GetObjectNInfo`
- those reads passed the coarse `fastGetRequestEligible(...)` gate
- they could not use the single-trip path
- so they incremented `fastGetFallbacks`

This made the metric unsuitable for interpreting user-facing GET fallback.

### Fix

Minimal fix implemented:

- reject `minioMetaBucket` in `fastGetRequestEligible(...)`

Files changed:

- [cmd/singletrip-get.go](/private/tmp/buckit-single-trip-get-phase1/cmd/singletrip-get.go)
- [cmd/erasure-object.go](/private/tmp/buckit-single-trip-get-phase1/cmd/erasure-object.go)
- [cmd/singletrip-header_test.go](/private/tmp/buckit-single-trip-get-phase1/cmd/singletrip-header_test.go)

Behavior after fix:

- fresh `FAST_GET=1` restart shows no startup fallback noise
- after one real GET:
  - `fast_get_hits_total = 1`
  - no fallback line emitted

After a fresh 100-request ON-only run:

- `GetObject total = 100`
- `fast_get_hits_total = 100`
- `fast_get_fallbacks_total = 0` effectively (no line emitted)

## Metrics / Benchmark Notes

### Reading metrics

Useful command:

```sh
/home/ubuntu/singletrip-bench/bin/mc admin prometheus metrics bench api --api-version v3
```

Relevant series after fix:

- `minio_api_requests_total{name="GetObject",...}`
- `minio_api_requests_fast_get_hits_total{...}`
- `minio_api_requests_fast_get_fallbacks_total{...}` (only appears when non-zero)

### Earlier benchmark notes

Several A/B runs were performed before the metric fix, including:

- 10-run ON/OFF passes for `640 KiB`, `1 MiB`, `2 MiB`
- reversed-order runs
- 100-run `2 MiB` ON/OFF percentile runs

Takeaway:

- results were not uniformly in favor of `FAST_GET=1`
- `2 MiB` in particular behaved inconsistently across different run orders
- after the metric fix, the ON-only 100-run counter check confirmed:
  - `100/100` requests hit the fast path
  - `0/100` requests fell back

### Latest percentile numbers captured

From the latest fresh patched-build 100-run `2 MiB` curl TTFB comparison:

ON:

- `p95 47.417 ms`
- `p90 29.137 ms`
- `p80 24.092 ms`
- `p70 22.538 ms`
- `p50 13.761 ms`

OFF:

- `p95 37.950 ms`
- `p90 28.294 ms`
- `p80 21.372 ms`
- `p70 12.067 ms`
- `p50 6.452 ms`

These are only for the latest fresh 100-run `2 MiB` comparison.

## Local Result Files

Not checked in; available in the local workspace under:

- `.tmp/bench2/`
- `.tmp/bench2_rerun_2m/`
- `.tmp/bench100_2m/`
- `.tmp/bench100_on_counts/`
- `.tmp/bench100_off_counts/`

These contain raw curl timings and, where captured, trace JSON.

## Current State

At the time of hand-off:

- the patched binary has been rebuilt and deployed to both nodes
- the cluster was last switched to `FAST_GET=1` and then later to `FAST_GET=0`/`FAST_GET=1` multiple times during experiments
- one `640 KiB` 100-run test was started but the user interrupted the turn

Because the last turn was interrupted, do not assume any in-flight benchmark loop completed cleanly.

Before resuming, check:

```sh
ssh -i /Users/rooseveltlai/Downloads/buckit.pem ubuntu@3.81.144.6 'ps -ef | grep -E "buckit|curl --http1.1" | grep -v grep'
ssh -i /Users/rooseveltlai/Downloads/buckit.pem ubuntu@35.173.238.193 'ps -ef | grep -E "buckit|curl --http1.1" | grep -v grep'
```

and re-establish whether the cluster is currently ON or OFF.

## Recommended Next Steps

1. Confirm current cluster mode (`FAST_GET=1` or `FAST_GET=0`) and kill any stray benchmark loops left by the interrupted turn.
2. Re-run the `640 KiB` 100-request comparison cleanly on the patched build.
3. If useful, add admin-trace capture to the fresh patched 100-run size comparisons, not just curl TTFB.
4. If the benchmark note should be preserved, convert key results from this hand-off into a checked-in benchmark README similar to `docs/single-trip-hdd-bench/README.md`.
