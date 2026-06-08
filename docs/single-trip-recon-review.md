# Single-Trip Fast GET — Prefer-Local + Parity Reconstruction: Review Guide

Audience: a reviewer (Claude Code or human) picking up the change cold. This
explains **what changed, why, and how it was validated**, and points at the parts
that most need scrutiny. Companion doc: `single-trip-parallelreader-deadlock-handoff.md`
(Codex's root-cause note).

Branch: `fix/singletrip-recon-parallelreader` (built on the single-trip phase-1 work).

---

## TL;DR

1. **A general erasure-coding bug was found and fixed** in
   `parallelReader.preferReaders` (`cmd/erasure-decode.go`) — a one-line fix. It is
   independent of single-trip and should be reviewed/committed on its own.
2. **The single-trip fast GET path gained prefer-local shard selection + parity
   reconstruction** (and an opt-in eager first-block prefetch). This is what first
   *exercised* the latent `preferReaders` bug.
3. **Result (2-node `d3.xlarge` real-HDD rig, EC:2, reboot-cold, n=300/arm/size):**
   the fast path is a **modest, size-dependent** cold-TTFB win, not the −63% an
   earlier `drop_caches` run suggested (that was a cold-cache artifact). Best variant
   **ON-eager**: median **−11% / −5% / −12% / −7%** at 640 KiB / 1 MB / 2 MB / 4 MB, with the best
   tail and mean at every size. `ON-stream` is erratic (−25% at 640 KiB but **+3%**
   at 1 MB). See §3 "Rig reboot-cold A/B — AUTHORITATIVE."

---

## 1. The core fix — `parallelReader.preferReaders` (review first)

File: `cmd/erasure-decode.go` (1 line) + `cmd/erasure-decode_test.go` (regression test).

```diff
 p.readers[next], p.readers[i] = p.readers[i], p.readers[next]
-p.readerToBuf[next] = i
-p.readerToBuf[i] = next
+p.readerToBuf[next], p.readerToBuf[i] = p.readerToBuf[i], p.readerToBuf[next]
```

**Bug:** `preferReaders` reorders `p.readers` to put preferred (local) readers first.
It must apply the *same permutation* to `p.readerToBuf` (reader-position →
output-buffer-index). The old code *assigned current indices* instead of *swapping
the existing mapping values*. Across chained swaps this produces **duplicate
output-buffer mappings** — two readers write the same buffer, so fewer than
`dataBlocks` distinct buffers fill, `canDecode()` never becomes true, and
`parallelReader.Read` blocks forever on its trigger channel
(`cmd/erasure-decode.go:161`).

Concrete EC:2 (M=4,N=2) case, preferred positions `[1,2,3]`:
```
readers:     [1, 2, 3, 0, 4, 5]
readerToBuf: [1, 2, 3, 2, 4, 5]   ← buf 2 mapped twice; only 3 distinct buffers fill
```

**Why it was dormant:** `preferReaders` only reorders when some `prefer[i]` is true.
The canonical decode path sets `prefer[i] = disk.Hostname() == ""`, which is a
**no-op in distributed deployments with hostnamed endpoints** (`http://node/path`
→ local disks still report a non-empty `Hostname()`), so nothing was ever marked
preferred and the buggy branch never ran. The single-trip change below sets
`prefer[i] = disk.IsLocal()` (the correct signal), which finally exercised it.

**Regression test:** `TestParallelReaderPreferReadersDoesNotDeadlock` — 6 readers,
M=4, prefer `[1,2,3]`, watchdog-fails if `Read` doesn't return in 1s. Verified
load-bearing: reverting the one-liner makes the test deadlock/fail; the fix makes
it pass.

**Note:** consider whether the canonical path's `prefer[i] = disk.Hostname()==""`
(`cmd/erasure-object.go`, `cmd/erasure-healing.go`) should also be `IsLocal()` —
prefer-local is currently a no-op there in hostnamed-endpoint clusters. Out of scope
for this change, but worth a follow-up.

---

## 2. Single-trip fast path changes

All in `cmd/singletrip-read.go` unless noted. The fast path reads `current/part.1`
(direct header + shard, co-located in one file) instead of `xl.meta` + `DataDir/part.1`.

What changed from the original single-trip fast path:

- **Quorum relaxed to "any decodable M"** (`pickSingleTripFastInfo`): accept a
  `directSig`-agreeing group with **≥ M distinct valid indices** (data and/or
  parity), instead of requiring all M *data* indices. This is what allows parity
  reconstruction.
- **Full M+N reader layout handed to the decoder** (`buildSingleTripFastInfo`):
  builds a reader for **every agreeing shard** at its `erasureIndex-1` position —
  the layout `parallelReader` is designed for. (An earlier attempt built a sparse
  M-only/nil-padded slice; that was a dead end and is gone.)
- **Prefer-local selection via the decoder**: `getObjectWithSingleTripInfo` sets
  `prefer[i] = disk.IsLocal()` (line ~432). `parallelReader` then reads the cheapest
  M shards (locals first) and **reconstructs missing data from parity** if a local
  shard is parity / a data shard is remote. This minimizes cross-node reads
  (3 local + 1 remote on a 3+3 layout) instead of chasing data shards that may be
  remote.
- **Close-after-response ordering**: only non-agreeing reads are closed in-path
  (`closeSingleTripHeaderReadsKeepGroup`); the selected readers' streams are owned
  by `info.readers` and closed after the response (`closeBitrotReaders`), matching
  the original path. (Closing partially-read remote streams *in-path before the
  response* was investigated as a hang cause and ruled out; the real cause was §1.)
- **Eager first-block prefetch (opt-in, `BUCKIT_FASTGET_EAGER=1`)**: for a LOCAL
  shard, `readSingleTripHeader` reads the header **and first encoded block** in one
  go off the open stream, then decode serves block 0 from memory and streams the
  rest (`multiReadCloser` = first-block buffer + remaining stream). Removes the
  header→body barrier so local shards decode instantly. Remote shards keep a live
  stream. **Default OFF** (see §3 for why).

Supporting:
- `cmd/singletrip-get.go`: `globalFastGetEager` env gate.

---

## 3. Validation

### Unit tests (`-tags kqueue,dev`, both `BUCKIT_FASTGET_EAGER` on and off)

- `TestParallelReaderPreferReadersDoesNotDeadlock` — the core-fix regression guard.
- `TestSingleTripFastGetReconstructsFromParity` — deletes a data shard's shadow so
  the fast path must reconstruct from parity; asserts bytes + fast-hit.
- `TestSingleTripDecodeMixedReadersReconstructNoHang` — `erasure.Decode` with
  instant + slow readers + forced reconstruction, watchdog'd. (Note: this passes
  even *without* the §1 fix — the deadlock needs the real `preferReaders` reorder
  path; kept as a decode-level guard.)
- Full `SingleTrip|FastGet` suite passes both modes.

### Rig reboot-cold A/B — AUTHORITATIVE (2× `d3.xlarge`, EC:2 = 4 data + 2 parity, 3 local + 3 remote)

This is the trustworthy result; the `drop_caches` pooled section further below is
**superseded** (see "Why reboot-cold" — small objects didn't re-cool under
`drop_caches`, inflating the 640 KiB win).

Method: fresh AWS `d3.xlarge` pair, one 6-drive EC:2 set, 100 distinct objects/size
(640 KiB, 1 MB, 2 MB, 4 MB) seeded once. Per arm-cycle: **reboot both hosts** (clears all
RAM cache; instance-store survives reboot, fstab auto-remounts), relaunch the arm,
wait for `6 drives online` + a readiness probe on a *throwaway* object (so the
post-launch 503 window never touches measured objects and they stay genuinely cold),
then one cold pass = 100 distinct first-touch GETs. **3 reboot cycles × 100 =
300 cold samples/arm/size.** Arms on one binary: **OFF** `BUCKIT_FAST_GET=0`;
**ON-stream** `FAST_GET=1, EAGER=0` (prefer-local + reconstruction); **ON-eager**
`FAST_GET=1, EAGER=1` (adds first-block prefetch). 0 × 503 across all runs.

Cold first-byte TTFB medians (ms):

| size | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| 640 KiB | 14.75 | **11.12 (−25%)** | 13.20 (−11%) |
| 1 MB | 22.48 | **23.26 (+3%)** ⚠ | 21.24 (−5%) |
| 2 MB | 21.16 | 19.47 (−8%) | **18.56 (−12%)** |
| 4 MB | 23.15 | 21.86 (−6%) | **21.60 (−7%)** |

Cold first-byte TTFB **p90** (ms) — the tail, where the variants diverge most:

| size | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| 640 KiB | 24.66 | 26.02 (**+6%**) | **22.59 (−8%)** |
| 1 MB | 37.46 | 34.51 (−8%) | **28.19 (−25%)** |
| 2 MB | 28.77 | 32.77 (**+14%**) | **24.78 (−14%)** |
| 4 MB | 37.52 | 28.93 (−23%) | **27.53 (−27%)** |

At p90 **ON-eager wins at every size** (−8 / −25 / −14 / −27%), and by *more* than at
the median — the eager block-0-from-memory decode is what protects against the
slow-shard-open tail. **ON-stream regresses at p90 for 640 KiB (+6%) and 2 MB (+14%)**
(lazy streaming has no tail protection), so on tail latency it is often *worse than
OFF*. This is the strongest argument for the ON-eager default.

Full distributions (ms), `n=300/arm`:

**640 KiB** (single block, 160 KiB shard):

| pct | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| p10 | 2.98 | 3.01 | 2.63 |
| p25 | 3.12 | 3.20 | 2.78 |
| **p50** | 14.75 | **11.12** | 13.20 |
| p75 | 19.19 | 18.86 | 19.27 |
| p90 | 24.66 | 26.02 | **22.59** |
| p95 | 27.69 | 32.17 | **24.48** |
| p99 | 61.33 | 68.18 | **45.77** |
| mean | 13.36 | 12.51 | **12.07** |
| max | 68.59 | 68.78 | 65.14 |

per-cycle p50: OFF 14.1/15.5/14.5 · ONS 10.7/11.3/11.3 · ONE 14.4/11.6/13.0

**1 MB** (exactly one full block = `blockSizeV2`, 256 KiB shard — the largest single-block):

| pct | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| p10 | 14.83 | 13.06 | 14.09 |
| p25 | 18.79 | 17.77 | 17.34 |
| **p50** | 22.48 | 23.26 | **21.24** |
| p75 | 27.51 | 28.23 | **24.62** |
| p90 | 37.46 | 34.51 | **28.19** |
| p95 | 43.01 | 38.64 | **30.72** |
| p99 | 90.03 | 77.00 | 76.74 |
| mean | 24.23 | 23.57 | **21.55** |
| max | 121.54 | 78.19 | 81.82 |

per-cycle p50: OFF 22.0/22.2/22.9 · ONS 23.3/23.7/22.7 · ONE 21.1/21.2/21.6

**2 MB** (2 blocks, 512 KiB shard):

| pct | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| p10 | 9.75 | 9.86 | 8.36 |
| p25 | 17.06 | 15.96 | 14.89 |
| **p50** | 21.16 | 19.47 | **18.56** |
| p75 | 23.48 | 23.61 | **21.42** |
| p90 | 28.77 | 32.77 | **24.78** |
| p95 | 34.17 | 38.62 | **25.61** |
| p99 | 42.75 | 52.13 | **40.32** |
| mean | 20.23 | 20.42 | **17.93** |
| max | 43.62 | 55.56 | 47.37 |

per-cycle p50: OFF 19.8/21.5/21.5 · ONS 19.3/20.0/19.6 · ONE 18.4/18.6/18.8

**4 MB** (4 blocks, 1 MiB shard; eager prefetches only block 0 = 25% of shard):

| pct | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| p10 | 16.82 | 17.64 | 17.06 |
| p25 | 20.43 | 19.91 | 19.44 |
| **p50** | 23.15 | 21.86 | **21.60** |
| p75 | 30.96 | 25.24 | **24.83** |
| p90 | 37.52 | 28.93 | **27.53** |
| p95 | 40.90 | 35.56 | **31.58** |
| p99 | 92.94 | 67.37 | 69.42 |
| mean | 26.00 | 23.48 | **22.52** |
| max | 127.37 | 97.66 | 70.38 |

per-cycle p50: OFF 23.1/23.3/23.4 · ONS 21.9/22.1/21.8 · ONE 21.6/21.6/21.5

**Reading the reboot-cold results:**

- **The win is real but modest and *non-monotonic* in object size** — not the −63%
  the `drop_caches` method suggested. The fast path saves the small metadata-read
  phase (~2–3 ms, `xl.meta` is ~366 B); the dominant cost (cold block-0 *data* read,
  paid by both OFF and ON) is unchanged. So the relative win tracks "how big is the
  cheap metadata phase vs the data read," which isn't monotonic.
- **1 MB is the weakest case** (one full single block): **ON-stream regresses to
  +3% vs OFF** (real — per-cycle 23.3/23.7/22.7 vs 22.0/22.2/22.9), while ON-eager
  still wins −5%. Lazy streaming loses its edge exactly at the full-block boundary;
  the eager block-0-from-memory decode is what keeps ON-eager ahead there.
- **ON-eager is the only consistent performer:** −11% / −5% / −12% / −7% across
  640 KiB / 1 MB / 2 MB / 4 MB, and it has the **best tail (p90/p95/p99) and mean at
  *every* size** (e.g. 4 MB p90 27.5 vs OFF 37.5). The earlier "eager has a bad
  single-block tail" was a transient under `drop_caches`; under clean reboot-cold its
  tail is the best.
- **ON-stream is erratic:** −25% (640 KiB), **+3% (1 MB, regression)**, −8% (2 MB),
  −6% (4 MB), with worse tails at the smaller sizes. The +3% at 1 MB is a **localized
  full-single-block-boundary** effect — it recovers to a modest win at 2–4 MB — but
  it shows lazy streaming has no reliable edge across sizes.
- Per-cycle medians are tight across the 3 reboots → reproducible cold; **0 × 503**
  (readiness probe worked).

**Recommended default: ON-eager** — the only variant that wins or ties at every size
and owns the tail/mean. ON-stream's 640 KiB median edge isn't worth its 1 MB
regression and worse tails.

**Why reboot-cold (and why the `drop_caches` numbers below are superseded):** with
`drop_caches=3` before each GET, *small* objects (640 KiB) did **not** actually
re-cool — per-round medians ran 20.6 → 7.3 → 5.9 ms (round 1 cold, then effectively
warm), which inflated the headline 640 KiB win to −63%. Rebooting both hosts forces
a true cold cache every cycle; per-cycle medians are now stable (e.g. OFF 640 KiB
14.1/15.5/14.5), so these are the numbers to trust.

---

### [SUPERSEDED] Rig pooled A/B (`drop_caches` method)

> Kept for history. The 640 KiB figures here are inflated by the cold-cache issue
> described above; use the reboot-cold table instead.

Method: cold first-byte TTFB, page cache dropped on **both** nodes before each GET,
single connection, `curl -w %{time_starttransfer}` issued host-local on node1
against the loopback endpoint; 100 distinct objects/size; **3 alternating rounds ×
100 = 300 samples/arm** (fresh server restart per arm to clear in-process metadata
cache). Arms: **OFF** = canonical `xl.meta` path; **ON-stream** = fast path,
`BUCKIT_FASTGET_EAGER=0` (prefer-local + reconstruction, no prefetch); **ON-eager**
= fast path, `BUCKIT_FASTGET_EAGER=1` (adds first-block prefetch).

All six arms returned **300/300 HTTP 200, zero timeouts**. (Pre-fix, the
prefer-local/eager arms hung ~15% with 30 s stalls; see §1.) Full cold-TTFB
distribution (ms):

**640 KiB (single-block):**

| metric | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| n | 300 | 300 | 300 |
| min | 4.11 | 3.91 | 3.51 |
| p10 | 4.48 | 4.36 | 3.90 |
| p25 | 4.92 | 4.56 | 4.07 |
| **p50** | **13.95** | **5.10** | **4.83** |
| p75 | 21.65 | 19.35 | 18.41 |
| p90 | 28.94 | 24.39 | 25.81 |
| p95 | 36.77 | 31.62 | 69.06 ⚠ |
| p99 | 47.90 | 50.25 | 707.64 ⚠ |
| max | 120.55 | 152.11 | 962.04 ⚠ |
| mean | 15.18 | 12.80 | 29.73 ⚠ |
| **median vs OFF** | — | **−63%** | **−65%** |

**2 MiB (multi-block):**

| metric | OFF | ON-stream | ON-eager |
|---|---:|---:|---:|
| n | 300 | 300 | 300 |
| min | 4.82 | 4.79 | 4.30 |
| p10 | 14.69 | 12.40 | 11.83 |
| p25 | 20.22 | 17.51 | 17.46 |
| **p50** | **23.87** | **21.94** | **20.40** |
| p75 | 30.98 | 29.12 | 23.95 |
| p90 | 41.88 | 39.45 | 29.37 |
| p95 | 47.09 | 46.48 | 39.46 |
| p99 | 79.56 | 63.40 | 72.18 |
| max | 192.44 | 131.99 | 102.88 |
| mean | 27.14 | 24.33 | 21.83 |
| **median vs OFF** | — | **−8%** | **−15%** |

Per-round medians (rounds of 100 — shows the rig's run-to-run noise; read the
pooled distribution above, not any single round):

| | OFF | ON-stream | ON-eager |
|---|---|---|---|
| 640 KiB | 20.6 / 7.3 / 5.9 | 12.4 / 12.8 / 4.7 | 4.7 / 15.9 / 4.3 |
| 2 MiB | 23.1 / 23.8 / 24.2 | 21.8 / 24.8 / 20.6 | 20.0 / 21.3 / 19.9 |

How to read it:

- **Both fast-path arms beat OFF at the median for both sizes.** The single-block
  win is large (−63%/−65%); the multi-block win is smaller at the median but
  ON-eager widens across the upper percentiles (p90 41.9 → 29.4, mean 27.1 → 21.8).
- **ON-eager's single-block tail (p95 69 / p99 708 / max 962 ms) is a single
  temporal stall, not steady overhead.** All 10 of its >100 ms outliers fall in
  round 2, positions 128–159 (a contiguous burst; six were 624–962 ms). Per-round
  means: 10.4 / **69.2** / 9.6 ms. OFF and ON-stream had a mild round-2 bump too
  (1–2 outliers each), so that window was generally noisy — but the arm order was
  fixed OFF → ON-stream → ON-eager every round, so ON-eager always ran last and
  caught the worst of it. The tail is therefore **temporally confounded**, not shown
  to be steady eager overhead. Likely mechanism if eager *does* amplify such stalls:
  `readSingleTripHeader` synchronously reads the first block from **all three local
  disks** and `openSingleTripFastInfo` waits for **every** disk goroutine
  (`g.Wait`), so one slow local HDD blocks the whole open phase, and the required
  remote shard is only read *after* — making local-tail and remote-transfer
  additive. ON-stream starts its selected body reads together after the header
  barrier, allowing more overlap. (Drain-on-close is *not* the leading explanation:
  cleanup runs after first-byte, the custom reader's `Close()` doesn't drain, and
  ON-stream has the same unused remote streams without the tail.)

  **Rerun result (ON-eager, cold 640 KiB, n=300):** the
  severe tail did **not** reproduce — median 9.2 ms, p95 37 ms, **max 206 ms, only
  2 GETs >100 ms** (vs the pooled run's 10 up to 962 ms), confirming a one-time
  transient, not steady eager overhead. Per-GET profiling of the slow ones shows
  they are **open-phase-bound and pinned to the slowest *remote* `ReadFileStream`
  open**: `maxRemoteOpen` ≈ `openphase` (e.g. 204.2 ≈ 204.4 ms), while
  `maxLocalBody` stays ≤4 ms on the slow GETs (56 ms max overall) and `firstbyte` is
  ~0.3 ms. So the cause is **cross-node grid-open latency variance** — *not* a local
  body stall (Codex's predicted 700–900 ms local `body_ms` did not appear) and not
  drain-on-close. The open phase `g.Wait`s on **all 6** disks, so one slow remote
  open blocks the whole GET; this exposure is **shared by ON-stream** (also opens all
  6), so it is not eager-specific. Mitigation if pursued: early-quorum — proceed once
  the needed M shards (prefer-local) are ready instead of waiting on slow *unneeded*
  remote opens.
- **640 KiB is noisy round-to-round** (OFF medians 20.6/7.3/5.9). The pooled p50 is
  the right summary; don't over-read a single round. 2 MiB is much steadier.
- The −8% vs −63% gap between sizes is expected: at 640 KiB metadata/open dominates
  cold TTFB (so collapsing it helps a lot), while at 2 MiB transfer is a larger
  share of first-byte.

Byte-correctness: md5 MATCH on both sizes (`current/part.1` served with `xl.meta`
absent on disk). Single-open-per-disk invariant preserved (one `ReadFileStream` per
participating disk; asserted by the end-to-end test).

**Caveat:** the rig HDDs are AWS d3 (real spinning disks behind Nitro NVMe; the
`rotational=0` flag is a Nitro artifact). They share backing storage and a page-cache
layer, so treat these as **relative off-vs-on ratios**, not absolute HDD figures.
Raw per-sample data: `/tmp/st-hdd-bench/pool-{640k,2m}-{OFF,ONS,ONE}.txt`.

---

## 4. Recommended default & open items

- **Default = ON-eager** (`FAST_GET=1, EAGER=1`): per the authoritative reboot-cold
  results (§3) it's the only variant that wins or ties at **every** size (−11% / −5%
  / −12% / −7% at 640 KiB / 1 MB / 2 MB / 4 MB) and has the **best tail and mean at every size**.
  The earlier "eager has a bad single-block tail" was a `drop_caches`-era transient;
  under clean reboot-cold its tail is the best.
- **ON-stream (eager off) is not recommended as default:** erratic across sizes
  (−25% at 640 KiB but **+3% regression at 1 MB**, −8% at 2 MB) with worse tails at
  the smaller sizes. The full-block (1 MB) boundary is where lazy streaming loses to
  OFF; eager's block-0-from-memory decode is what avoids that.
- **Magnitude expectation:** the cold-TTFB win is **modest (~5–12% at the median for
  ON-eager, larger in the tail)**, because the fast path only elides the small
  metadata-read phase (~2–3 ms; `xl.meta` ≈ 366 B) while the dominant cost — the cold
  block-0 *data* read — is paid by both arms. Don't expect the (artifactual) −63%.
- **Earlier eager-tail investigation (now moot for the default but still informative):**
  - **Rerun done (ON-eager, cold 640 KiB, n=300, profiling on):** tail did not
    reproduce (median 9.2, p95 37, max 206 ms, 2 outliers). Slow GETs are
    open-phase-bound with `maxRemoteOpen` ≈ `openphase` (up to 204 ms) and
    `maxLocalBody` ≤4 ms — i.e. **cross-node `ReadFileStream`-open latency variance**,
    blocking the whole GET because the open phase `g.Wait`s on all 6 disks. Shared by
    ON-stream (also opens all 6), so not eager-specific. (Codex's predicted 700–900 ms
    local `body_ms` did not appear; drain-on-close already ruled out.)
  - **Mitigation:** early-quorum — proceed once the needed M shards (3 local + fastest
    remote) are ready instead of waiting on the 2 slow *unneeded* remote opens. Helps
    both fast-path arms. Also worth: investigate why the remote grid open occasionally
    spikes to 100s of ms (node2/grid/network).
- **Prototype-only code to strip before merge if landing for real:**
  the `BUCKIT_FASTGET_*` diagnostic env gates.
- **Out-of-scope follow-ups:** canonical-path `prefer` should use `IsLocal()` (§1
  note); remote `ReadFileStream` cancellation under load (separate from the deadlock).

---

## 5. Reproduce

Unit:
```sh
CGO_ENABLED=0 go test -tags kqueue,dev ./cmd \
  -run 'TestParallelReaderPreferReadersDoesNotDeadlock|SingleTrip|FastGet' -count=1
```

Rig (scripts under `/tmp/st-hdd-bench/` on the operator's Mac, IPs hardcoded):
- `pooled-both.sh` → pooled OFF/ON-stream/ON-eager for 640 KiB + 2 MiB.
- Per-arm: `run/launch.sh <FAST_GET> EC:2` on both nodes. Raw data:
  `pool-{640k,2m}-{OFF,ONS,ONE}.txt`.

## 6. Suggested commit split

1. `cmd/erasure-decode.go` + `cmd/erasure-decode_test.go` — the `preferReaders`
   permutation fix (general; standalone).
2. `cmd/singletrip-*.go` (+ tests) — prefer-local + reconstruction + eager prefetch
   + profiler, and the bench docs.
