# Two-Node D3 HDD Bench — 640 KiB Cold TTFB (EC:2)

June 5, 2026 single-trip GET phase-1 cold-TTFB A/B on the two-node AWS `d3.xlarge`
real-HDD cluster, reconfigured to **EC:2** so the 6-drive set has **4 data drives
+ 2 parity**. Companion to `docs/single-trip-hdd-bench/two-node-d3-2mib.md` (same
rig, same method); read that first for the rig and method details.

## Headline

At 640 KiB the fast path is **not a clean win — it is a crossover**. Pooled across
300 cold samples per arm:

- **Low percentiles favor OFF, dramatically:** p10 ON +288%, p25 ON +210% (OFF
  floor ~2 ms vs ON floor ~4 ms).
- **Median is roughly a wash, slightly worse for ON:** p50 +11.8%.
- **The tail favors ON:** p90 −26%, p95 −26%, mean −18%.

The fast path's per-request overhead (EOF-stream open + cancel-not-drain close, no
connection reuse — design §9.9) sets a ~4 ms floor that dominates small/fast GETs,
while its saved metadata seek only pays off on the seek-bound tail. At 2 MiB the
saved seek dominated (clean ~14% median win); at 640 KiB the fixed overhead and the
seek-saving roughly cancel in the middle and the result splits by percentile.

## Config change vs the 2 MiB run

- Standard storage class set to **`EC:2`** via `MINIO_STORAGE_CLASS_STANDARD=EC:2`
  (env, both nodes; verified `mc admin info` → `EC:2` and process environ).
- 6 drives → **4 data + 2 parity**. A 640 KiB object splits into 4 × 160 KiB data
  shards (> 128 KiB inline cutoff), so a standalone `DataDir/part.1` and shadow are
  written. Verified on disk: `xl.meta` 370 B, `current/part.1` 164,896 B
  (160 KiB shard + header).
- Dataset: bucket `singletrip-cold-640k-ec2`, 100 distinct 640 KiB objects, random
  content, loaded under `FAST_GET=1`.

## Method

Identical to the 2 MiB run: 100 distinct cold first-touch objects per sweep, page
cache dropped on **both** nodes before each GET, **fresh server restart per arm**
(clears buckit's in-process metadata cache), client-side **curl** TTFB
(`time_starttransfer`) host-local on node1 against loopback, 3 alternating rounds →
300 samples/arm. No `mc admin trace` (curl-side only).

**Fast-path verified:** a 30-GET spot check on this dataset returned
`fast_get_hits_total = 30`, `fast_get_fallbacks_total = 0` — the ON arm is genuinely
single-trip, so the crossover below is not a silent-fallback artifact.

## Results

### Pooled cold TTFB, 640 KiB EC:2 (n = 300 per arm)

| percentile | OFF (ms) | ON (ms) | ON vs OFF |
|---|---:|---:|---:|
| p10  | 2.23  | 8.66  | +287.8% |
| p25  | 5.45  | 16.89 | +210.1% |
| p50  | 20.25 | 22.63 | +11.8% |
| p75  | 33.31 | 29.71 | −10.8% |
| p90  | 53.96 | 39.76 | −26.3% |
| p95  | 61.28 | 45.58 | −25.6% |
| mean | 30.22 | 24.71 | −18.2% |

Floors (5 fastest of 300): OFF ~1.7–1.9 ms, ON ~3.9–4.2 ms.

### Per-round medians (rig noise)

| round | ON median (ms) | OFF median (ms) |
|---|---:|---:|
| 1 | 21.47 | 10.30 |
| 2 | 19.66 | 28.52 |
| 3 | 27.67 | 19.75 |

As at 2 MiB, round-to-round variance swamps the per-round ON/OFF gap (round 2 OFF
even had a single 677 ms outlier). Only the pooled aggregate is meaningful.

## Interpretation

- **The ~2 ms OFF floor is not a fully-cold read.** 640 KiB with a 370 B `xl.meta`
  is tiny; the D3-behind-Nitro device layer appears to serve a chunk of these from
  a cache `drop_caches` cannot clear, giving OFF a large population of ~2 ms
  responses. The OFF path can return that fast because its cached-metadata + small
  read has little fixed machinery.
- **The ON floor is set by the fast path itself (~4 ms).** Opening `current/part.1`
  as an EOF stream per disk and closing with cancel-not-drain (no connection reuse)
  is a heavier per-request path than a warm small `xl.meta` read. For small objects
  where the disk work is cheap, this fixed cost makes ON *slower* at the fast end.
- **ON wins only where a real seek happens** — the p75–p95 tail and the mean —
  because there the one saved metadata seek outweighs the fixed overhead.
- **Net:** at 640 KiB on this rig the fast path is a tail/mean improvement
  (−18% mean, −26% p90) bought at the cost of low-percentile and median latency.
  This is the opposite balance from 2 MiB and is consistent with §9.9's
  cancel-close-churn caveat scaling worse as object size shrinks.

## Caveats

Same as the 2 MiB doc: curl-side only; real-HDD but very noisy (pooled only);
GET-only; magnitudes are rig-specific. Additionally: the ~2 ms OFF floor suggests
imperfect cold isolation on the D3/Nitro device cache, which inflates OFF's
low-percentile advantage — a server-side `mc admin trace` cross-check would help
separate device-cache effects from the path difference.

## Raw artifacts

Local under `/tmp/st-hdd-bench/` (not checked in): `agg-on.txt`/`agg-off.txt`
(300 samples/arm), `r{1,2,3}-{ON,OFF}.txt`, `urls-640k-ec2-clean.txt`,
`multi-round-640k.log`.
