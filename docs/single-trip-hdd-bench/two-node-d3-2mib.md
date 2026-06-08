# Two-Node D3 HDD Bench — 2 MiB Cold TTFB

This document records the June 5, 2026 single-trip GET phase-1 cold-TTFB A/B run on
a **two-node AWS `d3.xlarge` cluster with real local HDD storage**. It supersedes
the earlier, confounded 2 MiB numbers in `docs/single-trip-get-phase1-handoff.md`
(which repeated a small object set and was polluted by buckit's in-process metadata
cache — see "Method corrections" below).

## Headline

Pooled across **300 cold first-byte samples per arm** (3 rounds × 100 distinct
objects), enabling the single-trip fast path (`FAST_GET=1`) reduced **median cold
TTFB by ~14%** versus the canonical `xl.meta` path (`FAST_GET=0`), with a ~8–12%
reduction across p75–p95. The bottom decile is within noise. The signal is real but
**smaller than the design §6 HDD prediction (≥25%)** and the rig is very noisy, so
only the pooled aggregate is trustworthy.

## Rig

- Two AWS `d3.xlarge` nodes:
  - node1 — public `3.81.144.6`, private `172.31.44.123`
  - node2 — public `35.173.238.193`, private `172.31.37.19`
- Storage: three `/mnt/data0{1,2,3}` XFS mounts per node, **real dense HDD** local
  instance storage (D3 family).
  - **`rotational=0` is a Nitro artifact, not SSD.** D3/D3en expose spinning HDDs
    through the NVMe interface, so the kernel reports the `/dev/nvme*` devices as
    non-rotational even though the media is HDD. Confirm via IMDSv2 instance-type
    (`d3.xlarge`), not the rotational flag.
- Topology: one pool, one set, 6 drives, **EC:3** (standard parity 3, RRS 1).
- Endpoints use DNS aliases `buckit-node1` / `buckit-node2` in `/etc/hosts`
  (buckit rejects hostnames with underscores).
- SSH: `ssh -i /Users/rooseveltlai/Downloads/buckit.pem ubuntu@<public-ip>`.

## Dataset

- Bucket: `singletrip-cold-2m-big`
- 100 distinct objects, 2 MiB each, independent random content.
- Loaded once under `FAST_GET=1` so the `current/part.1` shadow exists on every
  drive (verified: `current/part.1` = 700140 B per drive vs `xl.meta` ~382 B).
- Presigned URLs (12 h) generated for all 100 objects; `curl` issues them
  host-local on node1 against `http://127.0.0.1:9000`.

## Method

The whole experiment is a runtime A/B on **identical on-disk data and one binary**;
the only variable is `BUCKIT_FAST_GET`.

1. **Distinct cold objects.** Each of the 100 objects is GET once per sweep — a
   genuine first-touch. (Re-reading the same object is not an independent cold
   sample, even with caches dropped.)
2. **Cold on both nodes.** Before every GET, page cache is dropped on **both**
   nodes (`sudo sh -c "sync; echo 3 > /proc/sys/vm/drop_caches"`). This is a
   distributed cluster, so dropping only node1 would leave shards warm on node2.
3. **Fresh restart per arm.** The server is restarted before each arm to clear
   buckit's **in-process metadata cache** (which `drop_caches` does not clear).
4. **Measurement.** Client-side TTFB only, via
   `curl --http1.1 -sS -o /dev/null -w '%{time_starttransfer}'`, executed on node1
   against the loopback endpoint. **No `mc admin trace` server-side TTFB was
   captured in this run** — these are curl numbers.
5. **Rounds.** 3 alternating rounds of [ON sweep, OFF sweep], 100 GETs each,
   pooled to 300 samples per arm.
6. **Fast-path verification.** After each ON sweep the counters confirmed the path
   actually fired: `minio_api_requests_fast_get_hits_total` == GET count and
   `fast_get_fallbacks_total` == 0 (line absent when zero). OFF sweeps show 0 hits.

## Results

### Pooled cold TTFB, 2 MiB (n = 300 per arm)

| percentile | OFF (ms) | ON (ms) | ON vs OFF |
|---|---:|---:|---:|
| p10  | 18.03 | 19.59 | +8.7% |
| p25  | 24.48 | 23.74 | −3.0% |
| **p50** | **36.68** | **31.62** | **−13.8%** |
| p75  | 47.43 | 42.78 | −9.8% |
| p90  | 56.26 | 52.04 | −7.5% |
| p95  | 65.49 | 57.40 | −12.4% |
| mean | 37.19 | 34.15 | −8.2% |

### Per-round medians (illustrating rig noise)

| round | ON median (ms) | OFF median (ms) |
|---|---:|---:|
| 1 | 37.69 | 33.67 |
| 2 | 25.72 | 43.17 |
| 3 | 31.35 | 29.09 |

Round-to-round variance (ON 25.7–37.7; OFF 29.1–43.2) **exceeds the ON/OFF gap**,
so no single round is meaningful — only the pooled aggregate is. Round 1 even shows
ON slower than OFF. This matches the run-order inconsistency noted in the handoff.

## Method corrections (why earlier 2 MiB numbers were wrong)

An initial attempt repeated the **same 10 objects** across cycles and reported a
~31–54% "win." That was an artifact:

- **In-process metadata cache.** `drop_caches` clears only the Linux page cache;
  buckit keeps its own metadata cache that survives it. Re-GETting the same object
  was served partly from that cache, so cycles 2–3 were ~5–8 ms while cycle 1 was
  ~20–30 ms — a warmup curve, not a fast-path effect.
- **Fix:** many distinct objects (touch each once) + fresh server restart per arm.
  The honest signal then collapsed to the ~14% median above.

## Caveats

- **curl-side only.** Server-side `mc admin trace` TTFB was not captured this run;
  curl TTFB includes connect/request-send overhead the server timer would not see
  (small over loopback, but present).
- **Real HDD, but noisy.** Despite being genuine HDD, the D3-behind-Nitro path has
  its own caching/queueing; treat per-round numbers as noise and the pooled median
  as the result.
- **Magnitude below §6.** ~14% median vs the design's ≥25% HDD prediction. Plausible
  causes: EC:3 metadata fan-out is parallel across spindles (≈ one seek of
  wall-clock, not six), and fixed RPC/processing overhead dilutes the single saved
  open/seek as a percentage of TTFB.
- **GET only.** The shadow doubles write cost; PUT numbers from this rig are
  meaningless by design.

## Raw artifacts

Local (not checked in), under `/tmp/st-hdd-bench/` on the operator's Mac:

- `agg-on.txt`, `agg-off.txt` — 300 pooled samples per arm
- `r{1,2,3}-{ON,OFF}.txt` — per-round 100-sample sweeps
- `urls-2m-big-signed.txt` — the 100 presigned URLs
- driver scripts: `cold-sweep.sh`, `multi-round.sh`, `launch.sh` (on the nodes),
  `stats.sh`
