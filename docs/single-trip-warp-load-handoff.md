# Single-Trip GET — Warp Load-Test Handoff (for Codex)

Audience: Codex picking this up cold. Scope: **load-test the single-trip fast GET
path under concurrency** (warp) and characterize how it compares to the canonical
path. All AWS hosts from the last session were **terminated** — you must
re-provision (steps in §6). This doc records what was observed, the rig/method, how
to reproduce, and Codex's current troubleshooting theory and isolation plan (§9).

Repo: `github.com/buckit-io/buckit`, working dir as checked out. Branches/binaries in §5.

---

## 1. Results observed so far

Warp GET, concurrency 64, 640 KiB non-inlined objects, EC:2, reboot-cold, working set
~5–7 GB reused via `--list-existing`:

| arm | obj/s | vs OFF |
|---|---:|---:|
| **OFF** (canonical) | ~593–610 | — |
| **ON, open all 6 shadows** (committed baseline) | ~309 | −48% |
| **ON, early-release** (open 6, cancel unused immediately) | ~309 (cold) | −48% (no change cold) |
| **ON, exactly-read-quorum** (open only 4) | 208 (stream) / 254 (eager) | −58…−66% |

Two raw observations, stated without interpretation:
- Under this concurrent load the single-trip path (ON) sustains roughly **half** the
  throughput of the canonical path (OFF).
- The **exactly-read-quorum** variant (which opens *fewer* shadows) measured **lower**
  throughput than the open-all-6 baseline, not higher.

Separately (different workload, for context only): single-request **cold-latency**
A/B is documented in `docs/single-trip-recon-review.md` — there single-trip is a
modest win. That is a single-request test, not this concurrent-load test.

---

## 2. What the two paths do (factual cost model)

Canonical GET (OFF): read `xl.meta` from all disks (≈366 B, caches quickly), resolve
quorum, then read the M **data shards** from `DataDir/part.1` via **bounded reads**
(`ReadFileStream` with an exact byte length) on the data disks. There is a subtle but
important reason this cluster normally reads data indices 1..M: OFF does construct a
`prefer` mask, but uses `disk.Hostname() == ""`. With URL/hostnamed distributed
endpoints, even the local `xlStorage` disks have a non-empty endpoint host, so the
mask is all false. `parallelReader` therefore keeps erasure-index order and starts
the first M readers, which are the M data indices. OFF only reconstructs when one of
those data reads is missing/corrupt (or in a path-endpoint deployment where the old
locality test actually reorders readers). This corrects an earlier verbal analysis
that treated OFF as prefer-local on this rig.

Single-trip GET (ON, `BUCKIT_FAST_GET=1`, `cmd/singletrip-read.go`): a co-located
shadow `object/current/part.1` = `[1 KiB fixed header][erasure shard]` exists. The
fast path opens it with **`ReadFileStream(..., 0, -1)` (to-EOF)**, reads the header
to rebuild `FileInfo` without `xl.meta`, then decodes. The server side of a to-EOF
`ReadFileStream` runs `xioutil.Copy` (128 KiB buffer) over a held connection (grid
for remote disks). `BUCKIT_FASTGET_EAGER=1` additionally reads the first block in the
open goroutine.

On-disk layout for a **non-inlined** object (verified on disk): both
`object/<DataDir>/part.1` (e.g. 262176 B for a 1 MiB object — the canonical shard)
**and** `object/current/part.1` (263200 B = header + shard) exist; `xl.meta` ≈ 366 B.

---

## 3. Measurements established during the session (facts)

- **Controller is not the bottleneck.** During load the warp controller was ~82%
  idle (first a 2-vCPU box, later a 4-vCPU t3.xlarge). Node-side iowait was high and
  the data HDDs were ~100% util.
- The reboot-cold load run is a **cold→warm mix**: a ~5–7 GB working set caches in
  ~13 s at the observed ~430 MiB/s, so a 40 s run starts disk-bound and warms.
- During the exactly-read-quorum run, `fast_get_fallbacks` stayed ~0 (checked
  mid-run: 5 total) and `fast_get_hits` climbed — i.e. the fast path was engaging,
  not silently falling back.
- A **fully warm** (no-reboot) ON smoke reached ~744–944 obj/s; the throughput gap
  vs OFF appears when the run involves disk I/O / streaming, less so fully cached.
- md5 of returned bytes matched the source for all valid (non-inlined) arms tested.

---

## 4. Gotchas that cost us time (do not repeat)

1. **Object size must be ≥ 640 KiB (non-inlined).** `smallFileThreshold = 128 KiB`
   (`cmd/xl-storage.go`); MinIO inlines when `ShardFileSize ≤ 128 KiB`. For EC:2
   (4 data) that means objects **≤ 512 KiB are inlined**: stored entirely in
   `xl.meta`, with **no `current/part.1` shadow** — so the fast path **cannot
   engage**; every ON GET probes 6 shadows (3 of them remote RPCs), gets "not found",
   and **falls back** to canonical. Our first warp run used 512 KiB and was
   **invalid** (all three arms ran identical canonical code). Always confirm
   `fast_get_hits` climbs and `fast_get_fallbacks` stays ~0 before trusting an ON
   number. (Observed side effect: with `FAST_GET=1`, inlined/small objects each pay a
   failed cross-node shadow probe + fallback.)

2. **Cold cache requires a reboot, not `drop_caches`.** Small objects do not re-cool
   under `echo 3 > drop_caches` (640 KiB ran 20→7→6 ms across rounds). **Reboot both
   nodes** before each arm. Instance-store **survives reboot** (only stop/terminate
   wipes it); mount the XFS volumes via `/etc/fstab` **by UUID** with `nofail` so a
   reboot auto-remounts them.

3. **Post-launch 503 settling:** the cluster reports "6 drives online" a few seconds
   before it actually serves. Probe a **throwaway** object until HTTP 200 before
   measuring, so the 503 window does not pollute samples and measured objects stay
   cold.

4. **Volume spec = ONE combined ellipsis:** `buckit ... server
   "http://buckit-node{1...2}:9000/mnt/data0{1...3}"` → one pool, one 6-drive EC:2
   set. Passing two separate args (one per node) makes **two pools of 3 drives** and
   EC:2 fails at startup with *"parity 2 should be ≤ 1"*.

5. **Warp has no v1.5.0 release binary.** Cross-build on a Mac:
   `GOOS=linux GOARCH=amd64 go install github.com/minio/warp@latest` →
   `$(go env GOPATH)/bin/linux_amd64/warp`; scp to the controller.

6. **PUT is slow on HDD EC:2** (~9–15 obj/s sustained). Build the warp working set
   **once** and reuse it with `warp get --list-existing` across all arms; do not
   re-PUT per arm.

7. **Warm vs cold are very different.** A fully-warm smoke (no reboot) reads far
   higher than a reboot-cold run. Compare arms only under the **same** protocol
   (the reboot-cold orchestrator), never a warm smoke of one arm vs a reboot-cold
   number of another.

---

## 5. Code: branches, binaries, variants

Committed baseline: **`fix/singletrip-recon-parallelreader`** — the working
single-trip fast path (prefer-local + parity reconstruction) plus the general
`parallelReader.preferReaders` fix; 3 clean commits. This is the **open-all-6**
version (the −48% arm). Its internals are described in
`docs/single-trip-recon-review.md`.

Perf experiments — **uncommitted working tree** on **`perf/singletrip-early-release`**
(`cmd/singletrip-read.go`, `cmd/singletrip-get_test.go`). The tree currently holds
**both** layered changes:
- **Early-release**: reintroduced `cancelReadCloser` (per-stream context cancel);
  selection picks M prefer-local and **cancels the unused N immediately** rather than
  after the response.
- **Exactly-read-quorum** (on top): `openSingleTripFastInfo` opens **only the read
  quorum** of disks (prefer-local), computed from `setDriveCount - defaultParityCount`
  (+1 when `data == parity`); decode `prefer` is all-false (exactly M readers).
- Tests updated accordingly: `TestSingleTripFastGetEndToEnd` asserts exactly
  read-quorum opens (`assertSingleTripQuorumOpens`); the former reconstruct test is
  now `TestSingleTripFastGetFallsBackWhenQuorumShadowMissing`. All single-trip +
  `ParallelReader` unit tests pass with eager on and off.

Prebuilt linux/amd64 binaries (operator Mac, `/tmp/st-hdd-bench/`):
- `buckit-fixed` — committed baseline (open all 6) = the −48% arm.
- `buckit-earlyrel` — early-release.
- `buckit-rquorum` — exactly-read-quorum = the −58…−66% arm.
(Other `buckit-*` are older cold-latency experiments; ignore for the load test.)

Build: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags kqueue -trimpath -o <out> .`
Unit tests: `CGO_ENABLED=0 go test -tags kqueue,dev -run 'SingleTrip|FastGet|ParallelReader' ./cmd/`
(run with and without `BUCKIT_FASTGET_EAGER=1`).

---

## 6. Re-provision the rig (hosts are gone)

Need: **2× d3.xlarge** (x86 — dense-HDD D3 is x86-only; `rotational=0` is a Nitro
artifact, the media is real HDD) and **1 controller** (t3.xlarge). SSH key:
`/Users/rooseveltlai/Downloads/buckit.pem`, user `ubuntu`. The operator launches EC2
and provides public + private IPs.

Per cluster node:
1. `mkfs.xfs -f` each of `/dev/nvme1n1 nvme2n1 nvme3n1`; mount to `/mnt/data0{1,2,3}`;
   add to `/etc/fstab` **by UUID** with `defaults,noatime,nofail`; `chown -R ubuntu`.
   (`/tmp/st-hdd-bench/provision-node.sh`.)
2. `/etc/hosts` on both nodes: `<priv1> buckit-node1`, `<priv2> buckit-node2`
   (underscores rejected; use hyphen names).
3. `mkdir -p /home/ubuntu/singletrip-bench/{bin,run,config,logs,results,data}`.
4. scp `bin/buckit` (a §5 binary) + `bin/mc`
   (`curl -sSL https://dl.min.io/client/mc/release/linux-amd64/mc`) + `run/launch.sh`.

`run/launch.sh` (in `/tmp/st-hdd-bench/launch.sh`) — args `<FAST_GET> [SC]`, honors
`BUCKIT_FASTGET_EAGER`:
```
export BUCKIT_FAST_GET=$FG BUCKIT_FASTGET_EAGER=$EAGER
export MINIO_ROOT_USER=buckitadmin MINIO_ROOT_PASSWORD=buckitadmin
export MINIO_STORAGE_CLASS_STANDARD=EC:2 MINIO_CONFIG_ENV_FILE=
nohup buckit --config-dir $ROOT/config server --address :9000 --console-address :9001 \
  "http://buckit-node{1...2}:9000/mnt/data0{1...3}" > $LOG 2>&1 &
```
Run the **same** command on **both** nodes; wait for `mc admin info` →
"6 drives online, EC:2". Controller: scp the cross-built `warp` to `~/warp`.

---

## 7. Run the load A/B

Working set (once, cluster up):
```
warp put --host=<priv1>:9000,<priv2>:9000 --access-key=buckitadmin --secret-key=buckitadmin \
  --obj.size=640KiB --concurrent=32 --duration=10m --noclear --no-color   # ~8k objects (~5GB)
```
Create a throwaway probe object + presigned URL for readiness; verify engagement with
a short `warp get --list-existing` (check `fast_get_hits` climbs, `fast_get_fallbacks`
~0).

Reboot-cold A/B orchestrator: **`/tmp/st-hdd-bench/warp-ab-multi.sh`** (edit the 3 IPs
at top). Per arm-cycle: reboot both → remount → launch arm → wait healthy →
wait-serving (probe) → `warp get --list-existing --concurrent=64 --duration=40s`.
3 rounds, **alternating arm order**. Arms: OFF (`fg=0`), ONS (`fg=1 eager=0`),
ONE (`fg=1 eager=1`). Output: `/tmp/st-hdd-bench/warp/multi.txt`. To test a variant,
overwrite `bin/buckit` with that binary and re-run.

Production cleanup note: the prototype per-request fast-get profiler was removed
from the production candidate. Use external CPU/heap profiles, `iostat`, and regular
fast-get hit/fallback counters for future validation.

---

## 8. Pointers

- `docs/single-trip-recon-review.md` — fast-path internals + the
  `parallelReader.preferReaders` fix + the cold-latency results.
- `docs/single-trip-parallelreader-deadlock-handoff.md` — earlier decode-deadlock
  root cause (already fixed).
- `cmd/singletrip-read.go` (fast path), `cmd/singletrip-get.go` (env gates,
  eligibility).
- `/tmp/st-hdd-bench/` (operator Mac) — binaries, scripts, `warp/multi.txt`
  (last results). Scripts hardcode the now-dead IPs; re-point before use.

---

## 9. Codex theory and troubleshooting plan

This section is intentionally self-contained so a new Codex/Claude session can
resume without the previous conversation.

### 9.1 What is and is not equivalent to OFF

The current exactly-read-quorum experiment selects all 3 local physical disks and
then the first remote physical disk. Adding a hash/random rotation for the remote
disk fixes a serious hotspot, but it still does **not** make the selected shard set
identical to OFF:

- OFF normally reads erasure data indices 1..4. Across objects, those four indices
  rotate over all six physical disks according to `fi.Erasure.Distribution`. It
  averages roughly 2 local + 2 remote shards for a request handled by either node.
- ON prefer-local reads 3 local + 1 remote regardless of erasure index. Some of the
  local shards are parity, so ON commonly calls `ReconstructData`; OFF normally does
  not. More importantly for HDD load, every request handled by a node fans out to
  **all three of that node's HDDs**.
- With a uniformly hash-selected remote drive and warp balanced across both hosts,
  the long-run read count per physical HDD should become approximately balanced and
  similar to OFF. The per-request scheduling pattern remains different, however:
  ON synchronizes three local disks on every request, while OFF's four data shards
  rotate across all six disks.

The uncommitted exactly-quorum code currently chooses the same first remote entry
from `er.getDisks()` for every request. That concentrates all cross-node reads from
node 1 onto one HDD on node 2, and vice versa. This is the leading explanation for
exactly-quorum becoming worse (208/254 obj/s) than open-all (309 obj/s). Do not draw
conclusions about exactly-quorum until the remote disk is selected deterministically
but evenly, for example `hash(object) % numberOfRemoteDisks`.

### 9.2 Ranked hypotheses for the remaining ON-vs-OFF gap

1. **Fixed remote-HDD hotspot in the current exactly-quorum prototype.** Expected
   signature: one remote HDD per node has much higher `r/s`, queue depth, `await`,
   and utilization than its two peers. Hash-rotating the remote selection should
   materially improve exactly-quorum without changing CPU usage much.

2. **Unbounded remote streams destroy connection reuse.** OFF opens each shard with
   a known finite length. ON opens `current/part.1` using
   `ReadFileStream(..., 0, -1)`, so the HTTP response is to-EOF/unknown-length. The
   decoder reads exactly the expected shard bytes but may never perform the extra
   read that observes response EOF/chunk termination. `cancelReadCloser.Close()`
   then cancels even a successfully consumed selected stream, likely discarding the
   inter-node connection rather than returning it to the idle pool. One new remote
   connection per GET is expensive at hundreds of GET/s. Open-all is potentially
   worse because it opens three remote streams and leaves two mostly unused.
   Expected signature: high TCP connection creation/TIME_WAIT and a large gain when
   selected full-object streams are read through EOF and closed normally rather
   than canceled. Cancellation should remain for genuinely unused streams.

3. **Eager serializes the local and remote body stages.** For a 640 KiB object the
   entire ~160 KiB local shard is the first block. Each local open goroutine reads
   header + full shard, and `openSingleTripFastInfo` waits for all selected goroutines
   with `g.Wait()`. The selected remote goroutine reads only its header; its body is
   consumed later by decode. Unless HTTP/socket buffering has already pulled the
   remote body, the critical path becomes `slowest of 3 local body reads` followed
   by `remote body read`. OFF starts its selected body reads together. This explains
   why eager can improve TTFB yet still reduce saturated throughput.

4. **Eager adds allocation and copy bandwidth.** At 640 KiB, eager allocates about
   3 x 160 KiB of temporary local shard buffers per GET. `parallelReader` then owns
   its normal pooled decode buffers, and `io.MultiReader` copies the eager buffers
   into them. At ~300 GET/s this is roughly 140 MiB/s of extra short-lived allocation
   and copy traffic before accounting for metadata objects/maps. Expected signature:
   higher `alloc_space`, GC CPU, `runtime.memmove`, and heap churn in ON-eager than
   ON-stream. This likely explains only the eager-vs-stream delta, not the entire
   ~50% ON-vs-OFF gap.

5. **Prefer-local fan-out and reconstruction change HDD queueing.** Even after
   remote rotation, every ON request handled by a node needs all three local HDDs.
   A slow/queued local disk gates every such request. OFF rotates data indices over
   all six drives and normally avoids Reed-Solomon reconstruction. Expected
   signature: an OFF diagnostic changed to `prefer = IsLocal()` falls toward ON, or
   an ON diagnostic that selects data indices 1..M rises toward OFF.

6. **The duplicate shadow files may have worse physical placement.**
   `writeSingleTripShadow` copies each canonical shard into a separately allocated
   `current/part.1`. The shadow workload may be more fragmented or laid out less
   favorably than canonical `DataDir/part.1` files on XFS/HDD. This is lower
   confidence, but it is testable with `filefrag` samples and a direct read-only
   microbenchmark over matched canonical/shadow files.

7. **Warm-cache behavior masks the issue.** The fully warm ON smoke was fast, while
   reboot-cold mixed runs were poor and HDDs reached ~100% utilization. That points
   primarily to disk/stream scheduling rather than the header parser itself. Keep
   cold, warm, and cold-to-warm results separate.

### 9.3 Isolation variants — run in this order

Use the same 640 KiB working set, concurrency 64, reboot protocol, alternating arm
order, and at least three rounds. Change one dimension per binary.

1. **R0: reproduce controls.** Re-run OFF and committed open-all ON-stream/ON-eager
   to verify the new hosts reproduce ~600 vs ~300 obj/s before testing changes.

2. **R1: exactly-quorum + hash-rotated remote, stream mode.** Select all local disks
   plus one remote using a stable object hash. Do not use process-global randomness;
   reproducibility and even object-to-drive distribution matter. This isolates the
   fixed-HDD hotspot without eager allocation/copying.

3. **R2: same as R1, eager mode.** The R2-R1 delta is the eager prefetch cost/benefit
   after disk selection is balanced.

4. **R3: ON exact-data-indices.** Use the object's deterministic erasure
   distribution to open the physical disks holding erasure indices 1..M, rather
   than 3-local+1-remote. Keep the shadow file/header path. This most closely matches
   OFF's shard set while retaining ON's to-EOF stream implementation. If R3 remains
   slow, shard selection/reconstruction is not the main cause.

5. **R4: OFF with `prefer[index] = disk.IsLocal()` (diagnostic only).** This makes
   canonical files use the same 3-local+1-remote/reconstruction policy as ON while
   retaining OFF's metadata and bounded stream path. If this OFF variant falls near
   R1, prefer-local HDD scheduling/reconstruction is the cause. If it stays near
   normal OFF, investigate ON's stream lifecycle and shadow layout.

6. **R5: selected-stream EOF/connection-reuse diagnostic.** For a full-object GET,
   do not cancel selected streams after the expected body is consumed. Read through
   response EOF (or otherwise give the response a correct content length), then
   close normally. Continue canceling unused streams. A large improvement identifies
   connection churn. A two-request implementation (fixed 1 KiB header read followed
   by a bounded body read) is acceptable as a diagnostic even though it is not the
   final single-trip design.

7. **R6: shadow-vs-canonical direct-read microbenchmark.** Bypass metadata and decode;
   issue matched bounded reads of the same shard bytes from `current/part.1` and
   `DataDir/part.1`. This isolates physical placement/path effects.

Decision table:

| result | implication |
|---|---|
| R1 greatly exceeds current exactly-quorum | fixed remote HDD hotspot confirmed |
| R3 approaches OFF, R1 does not | prefer-local/reconstruction/fan-out is costly |
| R4 falls near R1 | canonical path is fast partly because it reads data indices, not local shards |
| R5 approaches OFF | unknown-length stream close/cancel and connection reuse are primary |
| R6 shadow materially slower | duplicate-file placement/fragmentation is primary |
| Only R2 is worse than R1 | eager allocation/copy/barrier is the remaining issue |

### 9.4 Capture alongside every arm

- `iostat -dx 1` on both nodes, preserving per-device `r/s`, `rkB/s`, `await`,
  `aqu-sz`, and `%util`. The per-HDD distribution is more important than aggregate
  throughput.
- CPU plus allocation profiles for OFF, R1, and R2. Look for Reed-Solomon routines,
  hashing, `runtime.memmove`, allocation/GC, HTTP transport, and syscall time.
- `ss -s`, TCP state counts, and connection-rate evidence. Compare selected-stream
  cancel behavior with the EOF/drain diagnostic.
- Network bytes per node. OFF may read about two remote shards/request on average;
  prefer-local ON should read one. If ON sends more bytes despite that, to-EOF
  prestream/read amplification is occurring.
- Existing fast-path hit/fallback counters before and after each run. Record exact
  deltas, not only snapshots.
- Add temporary counters/log sampling for selected physical disk, erasure index,
  data-vs-parity, local-vs-remote, reconstruction invoked, streams opened, streams
  canceled, and bytes consumed. Avoid per-request logging during the measured run;
  use counters or sampling so instrumentation does not become the bottleneck.
- Warp per-host throughput. A large node1/node2 asymmetry points to selection or
  disk imbalance rather than generic decoder CPU.

### 9.5 Current recommendation before more data

Do not merge the uncommitted exactly-quorum implementation as written. Its fixed
remote selection invalidates its throughput result and reduces failure tolerance:
one missing selected shadow causes fallback because there is no spare. First run R1
through R5. The most promising production shape is likely early-quorum/open-racing:
start enough candidates to tolerate a slow/missing shard, proceed with the first
balanced decodable M, cancel only unused streams, and let selected streams complete
normally so transport connections remain reusable.

---

## 10. New rig and quick isolation results (2026-06-07)

Current hosts:

| role | public IP | private IP |
|---|---|---|
| buckit_node1 | `54.235.32.111` | `172.31.34.45` |
| buckit_node2 | `98.83.23.248` | `172.31.37.173` |
| controller | `100.24.17.87` | `172.31.42.212` |

Both storage nodes have three 1.8 TB instance-store HDDs formatted XFS and mounted
at `/mnt/data01..03` by UUID. The cluster is one six-drive pool, one erasure set,
EC:2. A reusable Warp set was seeded with 9,883 objects of 640 KiB (6.0 GiB logical).
The seed completed at 16.39 obj/s and was balanced across both hosts. Reboot/remount
and fast-path hit validation passed.

Quick protocol: one reboot-cold arm, Warp GET `--list-existing`, concurrency 64,
duration 15 seconds (about 12-13 measured seconds). These short runs are fully cold
and therefore lower than the earlier 40-second cold-to-warm numbers, but repeated
controls are stable enough for directional isolation.

| arm | obj/s | result |
|---|---:|---|
| OFF | 159.4 | initial control |
| OFF repeat | 165.9 | control reproduced |
| committed ON-stream/open-all | 87.4 | -45% vs first OFF |
| exactly-quorum, fixed remote | 85.5 | no improvement |
| exactly-quorum, object-hash rotated remote | 88.7 | small +3.7%; hotspot is secondary |
| exactly-quorum, exact data indices 1..M | 87.8 | reconstruction/locality is not primary |
| exact-data, bounded header + bounded body | 87.0 | to-EOF cancellation is not primary |
| exact-data, full contiguous shard prefetch + EOF drain | 87.0 | header/body barrier is not primary |

Direct matched-file cold read on node1, 3,000 files (1,000 per HDD): canonical
`DataDir/part.1` took 36.19 s; shadow `current/part.1` took 38.25 s. The shadow was
only 5.7% slower, so physical placement/fragmentation cannot explain the ~45% S3
gap.

What is now ruled out as the dominant cause: fixed remote-HDD selection,
prefer-local parity reconstruction, selected shard count, unknown-length stream
cancellation/connection reuse, shadow-file physical placement, and eager/full-body
prefetch. The next high-value step is a paired OFF/ON CPU + allocation profile and
per-device `iostat` capture under the same short cold run. In particular, compare
the canonical `streamingBitrotReader` path with `singleTripStreamingBitrotReader`,
and count actual storage operations/bytes per request; the remaining gap is in the
Buckit fast-read pipeline rather than the files or EC2 hosts.

Local artifacts: `/tmp/st-hdd-bench/warp-quick.sh`, `buckit-rquorum-rotated`,
`buckit-data-quorum`, `buckit-bounded-shadow`, and `buckit-full-prefetch`. The
worktree was restored to the pre-test exactly-quorum prototype after these failed
diagnostics; the experimental binaries remain for reference only.

---

## 11. Concurrency sweep and ON-eager phase profile (2026-06-07)

Additional local artifacts:

- `/tmp/st-hdd-bench/warp-concurrency-sweep.sh`
- `/tmp/st-hdd-bench/warp-profile-point.sh`
- `/tmp/st-hdd-bench/profile-helper.go`
- `/tmp/st-hdd-bench/analyze-profiles.sh`
- `/tmp/st-hdd-bench/fastget-prof-c32/`

The cluster was restored after profiling to ON-eager; `mc admin info` showed
`6 drives online, EC:2`.

### 11.1 Concurrency sweep

One reboot-cold arm per point, Warp GET `--list-existing`, 640 KiB objects,
duration 10 seconds:

| concurrency | OFF obj/s | ON-eager obj/s | interpretation |
|---:|---:|---:|---|
| 1 | 18.93 | 23.32 | eager helps single sequential TTFB |
| 4 | 58.05 | 56.74 | roughly tied |
| 16 | 114.60 | 89.29 | ON-eager hits a scaling knee |
| 32 | 148.73 | 82.37 | OFF keeps scaling; ON plateaus |
| 64 | 129.43 | 81.49 | OFF overdriven but still higher |

This confirms the problem is not single-request latency. ON-eager improves c=1, but
its saturated service time roughly doubles once concurrent random HDD reads build
queues.

### 11.2 pprof/iostat controls

Profile point: c32, Warp duration 12 seconds, profile window 18 seconds.

| arm | obj/s | avg TTFB | median TTFB | p90 TTFB | p99 TTFB |
|---|---:|---:|---:|---:|---:|
| OFF | 148.90 | 192 ms | 167 ms | 433 ms | 814 ms |
| ON-eager | 86.00 | 373 ms | 341 ms | 718 ms | 1.131 s |

CPU profiles sampled only about 3% CPU on both arms. Mutex profiles were effectively
empty. Heap/alloc profiles were dominated by startup/background allocations and did
not show a request-path allocation explanation. This is not CPU, GC, or mutex
limited.

`iostat -dx` showed all six data HDDs active in both arms. OFF averaged roughly
160-171 read ops/s per disk with about 96-99% util; ON-eager averaged roughly
152-157 read ops/s per disk with about 87-94% util. The ON disk rate is lower, but
not low enough by itself to explain the full object-throughput gap. Also note the
profile run had substantial cache effects: Warp reported ~93 MiB/s logical OFF
while iostat only saw ~10 MiB/s raw disk reads, so treat this as a relative
request-latency profile rather than a pure cold-HDD bandwidth profile.

### 11.3 Per-request ON-eager profiling

Short ON-eager profile run: c32, Warp duration 8 seconds, reproduced the issue at
79.06 obj/s. Historical per-request logs were filtered to Warp `.rnd` objects.
The log field `remote=` was unreliable because it used `disk.Hostname() != ""`;
classify remote by `host="http://..."` instead when interpreting those artifacts.

Node1 Warp requests:

| phase | n | avg | p50 | p90 | p95 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| `openphase_ms` | 410 | 195 ms | 171 ms | 355 ms | 411 ms | 581 ms |
| local eager body read | 1230 | 29 ms | 10 ms | 65 ms | 111 ms | 312 ms |
| remote open/header | 410 | 149 ms | 132 ms | 261 ms | 343 ms | 507 ms |
| decode/first-byte | 410 | 120 ms | 116 ms | 226 ms | 284 ms | 434 ms |
| open + decode | 410 | 316 ms | 307 ms | 492 ms | 582 ms | 724 ms |

Node2 Warp requests:

| phase | n | avg | p50 | p90 | p95 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| `openphase_ms` | 239 | 543 ms | 553 ms | 832 ms | 931 ms | 1154 ms |
| local eager body read | 717 | 144 ms | 17 ms | 470 ms | 549 ms | 740 ms |
| remote open/header | 239 | 82 ms | 53 ms | 219 ms | 306 ms | 385 ms |
| decode/first-byte | 239 | 3 ms | 0 ms | 1 ms | 1 ms | 80 ms |
| open + decode | 239 | 546 ms | 556 ms | 834 ms | 932 ms | 1155 ms |

Important observation: each successful GET opens exactly four shadows. In eager
mode, the three local selected shard bodies are read inside the header-open
goroutines before `openSingleTripFastInfo` returns; the selected remote shard is
kept as a live stream and may be read later by decode. That creates a staged
barrier:

1. wait for remote header/open and three local body reads;
2. build `singleTripFastInfo`;
3. run decode, which may still need to read the remote body.

On node1 this is visible as `openphase` plus an additional ~116 ms median decode
delay. In other words, eager does not necessarily read the four required shard
bodies in one fully overlapped decode wave. OFF's canonical path pays metadata
first, but the body reads happen together in `parallelReader`; that can win under
concurrency even though OFF uses more trips.

Node2 is worse mostly because the max of three local eager body reads is very high
under HDD queueing. A single local-body read has p50 only 17 ms, but p90 470 ms; a
GET waits for three of them, so the per-request max lands near the long tail.

### 11.4 Current theory

The dominant ON-eager under-load cost is the fast path's fork/join structure, not
network round trips alone:

- Eager moves full first-block shard reads into the metadata/open phase, so the S3
  response cannot start until those local HDD reads complete.
- The selected remote shard body is not prefetched in the same wave, so some
  requests pay local-body wait and then remote-body/decode wait serially.
- At c16+, HDD random-read tail latency makes the max-of-three-local-body barrier
  expensive; throughput becomes `concurrency / longer service time`.
- OFF has two logical trips, but its large body reads are driven by the decoder in a
  single parallel wave and can keep more independent work in flight.

This does not fully explain why ON-stream is also poor; ON-stream still has a
header-open barrier before decode, uses the shadow to-EOF stream shape, and may be
less able to overlap remote body work than canonical bounded `DataDir/part.1`
reads. But for ON-eager specifically, the phase profile shows the eager body
barrier is real and large.

### 11.5 Next troubleshooting plan

1. Add low-overhead counters, not per-request logs, for selected erasure indices,
   local/remote classification using `IsLocal()`, whether decode reads a remote
   body, and per-request selected shard count.
2. Test an "eager all selected M" diagnostic: after header quorum/selection,
   prefetch the first block for all selected shards, including the selected remote
   shard, concurrently. This should remove the node1 `openphase + remote decode`
   serial cost. If c32 improves materially, the staged barrier is confirmed.
3. Test a "no eager body before selection" diagnostic: read headers only, select M,
   then let decode read all selected bodies in parallel. This should approximate
   OFF's body scheduling while preserving single-trip metadata. If it beats eager,
   the local prefetch barrier is the main load issue.
4. Compare ON-stream, ON-eager-current, eager-all-selected, and no-eager-body at
   c16/c32 only. Long multi-round tests are not needed until one variant moves the
   plateau.
5. Fix profiling labels: replace `disk.Hostname() != ""` with `!disk.IsLocal()` in
   the profile log, or log both fields explicitly.

### 11.6 Diagnostic results: header-only and selected-eager

Built current worktree as `/tmp/st-hdd-bench/buckit-headeronly-current`:

- Linux amd64, build id `f2c3a73ca137717022bedeae1c91c74bf35d6671`
- sha256 `f8e9ccf7d4828b8c430173df0c26923e967570d930d653c499358f3cd0c1a613`

Quick comparison, same binary, one reboot-cold arm, Warp duration 10 seconds:

| concurrency | OFF | ON-stream/header-only | ON-eager |
|---:|---:|---:|---:|
| 16 | 114.29 obj/s | 79.09 obj/s | 92.15 obj/s |
| 32 | 145.21 obj/s | 82.98 obj/s | 82.02 obj/s |

Conclusion: simply removing eager body prefetch is not a fix. Header-only still
plateaus at about the same level. Eager helps at c16 but not at c32.

Then added diagnostic env `BUCKIT_FASTGET_EAGER_SELECTED=1` and built
`/tmp/st-hdd-bench/buckit-eager-selected`:

- Linux amd64, build id `99646f61385ed72bf9436c5d988080a371ae549b`
- sha256 `be9f685d74b9ae005e4b99ee889d45e2b41554468a039d1693661f7cfaf22c2d`

In selected-eager mode, the code reads headers first, selects the M decode shards,
then prefetches the first block for all selected shards concurrently. Focused tests
passed:

```sh
GOCACHE=/tmp/st-hdd-bench/gocache GOTMPDIR=/tmp/st-hdd-bench/gotmp \
  CGO_ENABLED=0 go test -tags kqueue,dev -run 'SingleTrip|FastGet|ParallelReader' ./cmd/

BUCKIT_FASTGET_EAGER=1 BUCKIT_FASTGET_EAGER_SELECTED=1 \
  GOCACHE=/tmp/st-hdd-bench/gocache GOTMPDIR=/tmp/st-hdd-bench/gotmp \
  CGO_ENABLED=0 go test -tags kqueue,dev -run 'SingleTrip|FastGet|ParallelReader' ./cmd/
```

Selected-eager quick result:

| concurrency | ON-selected-eager |
|---:|---:|
| 16 | 90.76 obj/s |
| 32 | 86.42 obj/s |

Conclusion: prefetching all selected M shards after selection also does not move the
plateau. It slightly improves c32 vs current eager/header-only, but remains far from
OFF. This weakens the earlier "local eager barrier" theory as the dominant cause.
The more likely remaining cause is the shadow stream path itself: unknown-length
`ReadFileStream(..., 0, -1)` plus `singleTripStreamingBitrotReader` and remote grid
stream behavior are materially worse under concurrent small-object reads than the
canonical bounded `DataDir/part.1` bitrot readers, even when the selected shard count
and eager scheduling are changed.

Next best diagnostic: make the single-trip path use bounded reads for selected
shadow shards after reading the 1 KiB header, i.e. two storage reads per selected
shard (`offset=0,length=1024` for header, then bounded body range). This loses the
single-trip property but isolates whether `to EOF` streaming/connection handling is
the real under-load cost. If bounded shadow reads approach OFF, the production
single-trip design needs a bounded-length shadow stream or protocol support to keep
one trip without the current to-EOF behavior.

Current cluster state after diagnostics: relaunched with regular ON-eager,
`BUCKIT_FASTGET_EAGER_SELECTED` unset. `mc admin info` reported `6 drives online,
0 drives offline, EC:2`.

### 11.7 Diagnostic phase metrics quick validation

Added low-cardinality diagnostic phase metrics exported through the existing API
metrics endpoint during isolation.

Phases instrumented during that validation:

| phase | meaning |
|---|---|
| `metadata` | OFF `getObjectFileInfo` / normal xl.meta path |
| `reader_setup` | `NewGetObjectReader` setup |
| `shadow_open` | ON shadow `ReadFileStream(..., current/part.1, 0, -1)` open |
| `shadow_header` | ON 1 KiB single-trip header read |
| `shadow_prefetch` | ON eager first-block body prefetch |
| `readat` | body shard `ReaderAt.ReadAt` calls used by decode |
| `decode` | `erasure.Decode` plus response write path |
| `decode_firstbyte` | time from decode entry to first write |

Built `/tmp/st-hdd-bench/buckit-diag-metrics-v2`:

- Linux amd64, build id `1faa13badc29e7ea7fdbd7b7cb26613ebba7a19b`
- sha256 `9d43761a4718f0e7ac7ccae3d948591a791458e46fca77373eafdb8f915a0c5b`

Focused tests passed:

```sh
GOCACHE=/tmp/st-hdd-bench/gocache GOTMPDIR=/tmp/st-hdd-bench/gotmp \
  CGO_ENABLED=0 go test -tags kqueue,dev -run 'SingleTrip|FastGet|ParallelReader' ./cmd/
```

One-shot validation, c32, Warp duration 8 seconds, one reboot-cold arm each:

| arm | obj/s |
|---|---:|
| OFF | 146.15 |
| ON-eager | 81.91 |

Phase diff from before/after metrics snapshots, summed across both nodes:

| arm | phase | count | avg ms | bytes | p50 | p90 | p99 |
|---|---|---:|---:|---:|---:|---:|---:|
| OFF | metadata | 1163 | 1.00 | 0 | <=1 ms | <=1 ms | <=20 ms |
| OFF | readat | 4650 | 109.24 | 758,842,346 | <=100 ms | <=500 ms | <=1 s |
| OFF | decode | 1164 | 219.16 | 0 | <=500 ms | <=500 ms | <=1 s |
| ON-eager | shadow_open | 2720 | 29.01 | 0 | <=0.5 ms | <=200 ms | <=500 ms |
| ON-eager | shadow_header | 2720 | 40.73 | 2,785,280 | <=20 ms | <=200 ms | <=500 ms |
| ON-eager | shadow_prefetch | 2040 | 71.76 | 334,298,880 | <=20 ms | <=500 ms | <=1 s |
| ON-eager | readat | 2742 | 16.84 | 446,235,629 | <=0.5 ms | <=100 ms | <=500 ms |
| ON-eager | decode | 687 | 68.25 | 0 | <=5 ms | <=200 ms | <=500 ms |

Interpretation:

- OFF spends almost all request time in the decode/body-read phase; metadata is not
  a bottleneck.
- ON-eager's decode/body phase is faster than OFF once it reaches decode.
- The missing throughput is before decode: per GET, ON-eager performs four shadow
  opens, four header reads, and three local eager prefetch reads. These phases have
  HDD-tail latencies under c32 and extend request service time before response
  streaming begins.
- This confirmed the metric idea was useful for quick validation, but the phase
  metrics were removed from the production candidate to avoid permanent diagnostic
  series.

Artifacts:

- Metrics run: `/tmp/st-hdd-bench/diag-metrics-c32-130132`
- Runner: `.tmp/fastget-diag-metrics-c32.sh`

Current cluster state after this metrics validation: ON-eager diagnostic metrics
binary running, `BUCKIT_FASTGET_EAGER_SELECTED` unset.
`mc admin info` reported `6 drives online, 0 drives offline, EC:2`.

PR cleanup note: these diagnostic phase metrics were removed from the production
candidate. Keep the regular fast-get hit/fallback counters.

### 11.8 No-fallback validation

Added diagnostic env `BUCKIT_FASTGET_NO_FALLBACK=1`. For fast-get-eligible
requests, if `tryFastGet` returns `ok=false`, the request returns an error instead
of falling back to the canonical path. This is diagnostic-only and was added to rule
out mixed ON/OFF work during the measured ON arm.

Important operational note: object-based readiness probes fail in this mode if the
probe object lacks a single-trip shadow. Use `/minio/health/ready` for readiness
checks instead.

Built `/tmp/st-hdd-bench/buckit-diag-nofallback`:

- Linux amd64, build id `04293489805a8f00ad6bc60089ab831b4bf9175e`
- sha256 `c0f2d7e6d9859b18c348772b2c2caf0eda92b74570b3dd862309e806436e6877`

Focused tests passed with no-fallback unset:

```sh
GOCACHE=/tmp/st-hdd-bench/gocache GOTMPDIR=/tmp/st-hdd-bench/gotmp \
  CGO_ENABLED=0 go test -tags kqueue,dev -run 'SingleTrip|FastGet|ParallelReader' ./cmd/
```

One-shot c32 ON-eager validation, Warp duration 8 seconds:

| arm | obj/s |
|---|---:|
| ON-eager, no fallback | 84.11 |

Metrics diff across both nodes:

| metric | delta |
|---|---:|
| fast-get hits | 694 |
| fast-get fallbacks | 0 |

Phase averages:

| phase | count | avg ms | bytes |
|---|---:|---:|---:|
| decode | 701 | 63.53 | 0 |
| readat | 2798 | 15.62 | 455,410,687 |
| shadow_header | 2776 | 41.45 | 2,842,624 |
| shadow_open | 2776 | 30.48 | 0 |
| shadow_prefetch | 2082 | 71.44 | 341,181,504 |

Conclusion: fallback is not causing the ON-eager plateau. With fallback disabled,
throughput remains in the same range and the phase profile is unchanged. Current
cluster state after validation: relaunched with `BUCKIT_FASTGET_NO_FALLBACK` unset,
regular ON-eager, diagnostic metrics binary still running, 6 drives online EC:2.

### 11.9 Request-level metrics fix

The first phase metrics missed the per-request fork/join barrier. Per-shard averages
for `shadow_open`, `shadow_header`, and `shadow_prefetch` cannot be summed to explain
request service time because those operations happen concurrently and the request
waits for the slowest required shard plus selection work. Added request-level phases:

- `fast_open`: wraps the full `openSingleTripFastInfo` call, including all selected
  shadow goroutines, `g.Wait()`, header grouping, optional eager prefetch, and M-shard
  selection.
- `request_firstbyte`: from fast-path entry to first write from the decode goroutine.
- `request_total`: from fast-path entry to decode goroutine completion.

Built `/tmp/st-hdd-bench/buckit-diag-metrics-v3`:

- Linux amd64, build id `ab8c0bdcfcae992d5c0ec8f230d54421a96f0ffc`
- sha256 `f6747eb9bc139ab5a512f8fd4fc71c8966e1233764d1c34f07f4da3f4b457fc5`

ON-only c32 validation, no-fallback enabled, Warp duration 8 seconds:

| metric | value |
|---|---:|
| throughput | 81.53 obj/s |
| throughput-derived service time (`32 / obj/s`) | 392.49 ms |
| fast-get hits | 685 |
| fast-get fallbacks | 0 |

Phase averages:

| phase | count | avg ms | bytes |
|---|---:|---:|---:|
| `fast_open` | 685 | 340.03 | 0 |
| `request_firstbyte` | 692 | 378.95 | 0 |
| `request_total` | 692 | 378.95 | 0 |
| `decode` | 692 | 42.10 | 0 |
| `readat` | 2762 | 10.24 | 449,512,453 |
| `shadow_open` | 2740 | 30.42 | 0 |
| `shadow_header` | 2740 | 41.93 | 2,805,760 |
| `shadow_prefetch` | 2055 | 80.19 | 336,756,960 |

This closes the accounting gap. `request_total` (~379 ms) aligns with
throughput-derived service time (~392 ms). The missing time was inside the full
`openSingleTripFastInfo` barrier, not an uninstrumented post-decode wait. The
per-shard shadow phase averages are still useful, but `fast_open` is the critical
request-level metric: ON-eager spends about 340 ms before entering decode.

Current cluster state after this validation: relaunched with `BUCKIT_FASTGET_NO_FALLBACK`
unset, regular ON-eager, 6 drives online EC:2.

## 12. Final load-test root cause and fix: M-spread selection

Date: June 7, 2026. Branch/worktree: `perf/singletrip-early-release`.

### 12.1 What was actually wrong

The concurrent Warp throughput regression was not caused by fallback and was not an
unmeasured post-decode wait. Request-scoped profile logs showed that
`openSingleTripFastInfo` time was almost exactly `g.Wait()` time, and `g.Wait()` was
almost exactly the slowest selected shard's active time.

The exact-quorum prototype had a load-balance bug:

1. EC:2 has `M=4`, `N=2`.
2. Each node has 3 local drives.
3. The fast path opened exactly `M` shadows by selecting all local disks first, then
   appending remote disks in fixed erasure-set order, then truncating at `M`.
4. On each landing node this became `3 local + first remote`.
5. Under c32 Warp load this pinned every remote quorum read from node1 to
   `buckit-node2:/mnt/data01`, and every remote quorum read from node2 to
   `buckit-node1:/mnt/data01`.

Trace proof from `/tmp/st-hdd-bench/fastopen-trace-c32-141323`:

```text
landing node1 remote selected:
893 times read=3 disk=1 http://buckit-node2:9000/mnt/data01

landing node2 remote selected:
497 times read=3 disk=0 http://buckit-node1:9000/mnt/data01
```

For a representative bad request (`18B6DF3891AD18BA`):

```text
fastopen total = 353.523 ms
g.Wait        = 353.501 ms
slowest shard = remote node2/data01 = 353.473 ms
decode total  = 110.002 ms
computed body production = 463.525 ms
```

The remote selected shard was not prefetched in the local-only eager arm because
`BUCKIT_FASTGET_EAGER_SELECTED` was unset. That meant the request paid for slow
remote stream acquisition during fast-open and then could pay again during decode
when the remote body was needed.

### 12.2 OFF baseline caveat

OFF is also not truly local-prefer on this hostnamed distributed setup. The canonical
decode path used:

```go
prefer[index] = disk.Hostname() == ""
```

But profile logs showed local disks have non-empty hostnames:

```text
host="/mnt/data02" hostname="buckit-node2:9000" local=true
```

Therefore OFF's `prefer` mask is effectively all false in this rig. A short OFF
profile run (`/tmp/st-hdd-bench/off-readat-locality-c32-144054`) captured 516 normal
GETs where decode read `2 local + 2 remote` shards, not `3 local + 1 remote`.

This matters for comparisons: OFF spreads body reads better than the original
exact-quorum ON selector because OFF opens all valid readers and does not pin to the
first remote disk. OFF also benefits from `xl.meta` being warm in bounded working-set
load tests, so OFF is partly advantaged versus a true cold-metadata workload.

### 12.3 Implemented fix / diagnostic mode

Added `BUCKIT_FASTGET_SPREAD=1` as a diagnostic selector mode. With spread enabled,
the fast path still opens exactly `M` shadows, but it rotates across the full
erasure-set disk order instead of local-first truncating. On this 2-node EC:2 layout
that produces `2 local + 2 remote` per GET and spreads remote reads across all three
remote drives.

Relevant code:

- `cmd/singletrip-get.go`: `globalFastGetSpreadSelection`
- `cmd/singletrip-read.go`: `selectSingleTripFastOpenDisks` and
  `selectSingleTripSpreadDisks`
- `cmd/singletrip-get_test.go`:
  `TestSingleTripFastOpenRemoteSelectionRotates` and
  `TestSingleTripFastOpenSpreadSelectionRotatesAcrossSet`

The remote-rotation fix for local-first mode remains useful:

- Default selector: all local disks first, then rotated remote candidates.
- Spread selector: rotated full-set selection, used only when
  `BUCKIT_FASTGET_SPREAD=1`.

### 12.4 Additional diagnostics added

Request-scoped profile logs exposed the full fast-open critical path during
isolation. Those profiler logs were later removed from the production candidate.

Aggregate diagnostic metrics also include:

- `fast_open`
- `fast_open_wait`
- `fast_open_pick`
- `fast_open_build`
- `fast_open_close`
- `request_firstbyte`
- `request_total`

`BUCKIT_FASTGET_NO_FALLBACK=1` remains a diagnostic guard: eligible fast-get misses
return an error instead of silently using OFF, so ON-only validation cannot be
polluted by fallback work.

### 12.5 Validation results

Focused tests:

```sh
GOCACHE=/tmp/st-hdd-bench/gocache GOTMPDIR=/tmp/st-hdd-bench/gotmp \
  CGO_ENABLED=0 go test -tags kqueue,dev -run 'SingleTrip|FastGet|ParallelReader' ./cmd/
```

Result:

```text
ok   github.com/buckit-io/buckit/cmd   6.759s
```

ON-spread profiled c32 run:

```text
artifacts: /tmp/st-hdd-bench/fastopen-trace-c32-144904
throughput: 80.46 MiB/s, 128.73 obj/s
```

Selection distribution:

```text
1054 requests: all were 2 local + 2 remote
```

Remote selected-shard distribution:

```text
node1 -> node2/data01: 348
node1 -> node2/data02: 391
node1 -> node2/data03: 339

node2 -> node1/data01: 346
node2 -> node1/data02: 323
node2 -> node1/data03: 361
```

Side-by-side c32 throughput, profiling disabled:

```text
artifacts: /tmp/st-hdd-bench/off-vs-onspread-c32-145331
OFF       66.10 MiB/s, 105.76 obj/s
ON-spread 73.47 MiB/s, 117.55 obj/s
```

`ON-spread` was +11.1% obj/s over OFF in that run.

Side-by-side c32 throughput with selected-eager enabled:

```text
artifacts: /tmp/st-hdd-bench/off-vs-onspread-c32-150400
OFF                         67.38 MiB/s, 107.81 obj/s
ON-spread + selected-eager  73.58 MiB/s, 117.72 obj/s
```

The selected-eager variant was essentially identical to local-only eager in spread
mode. That suggests the large win came from shard-selection/load distribution, not
from moving remote body reads from decode into fast-open.

### 12.6 Current recommendation

For this EC:2 concurrent-load workload, keep the `M-spread` selector as the leading
candidate for further validation. It matches OFF's effective shard spread while still
skipping `xl.meta`, avoids the remote-disk hot spot, and measured faster than OFF in
quick side-by-side runs.

Open follow-ups:

1. Decide whether `BUCKIT_FASTGET_SPREAD=1` stays diagnostic or becomes the default
   exact-quorum selector for distributed EC:2-style pools.
2. Fix OFF local preference separately by changing `disk.Hostname() == ""` to
   `disk != nil && disk.IsLocal()` and then rerun OFF vs ON-spread. That is a
   separate behavioral change and should not be mixed into this commit unless the
   goal is to redefine the baseline.
3. Run longer multi-round alternating order tests after the quick validation phase.
4. Consider hedged `M+1` only after the spread selector is fully characterized.

## 13. Critical cache discovery: stable selection, not lazy body, fixes hot repeats

The earlier duration-based warp results were heavily shaped by repeated-object page
cache behavior. To isolate this, `testing/singletrip-hdd/once_get.py` now supports:

```sh
--repeat=2 --repeat-mode=passes
```

This lists a fixed key set once, shuffles it deterministically, then issues the
entire key set once followed by the same key set a second time. This makes pass 1 a
cold-to-warm read and pass 2 a repeated-object read.

Important result: OFF's hot-cache collapse was not mainly caused by its lazy bounded
body reader. It was caused by stable per-object shard selection. The original
single-trip spread selector rotated selected shards per request; repeated GETs for
the same object often read a different M-shard set, so pass 2 missed the warmed
shadow shard bodies.

Validation workload:

- Two-node EC:2 pool, 6 HDD drives, EC:2.
- 9,883 object keys from `/home/ubuntu/once-get-keys-seed17.txt`.
- `--repeat=2 --repeat-mode=passes`, concurrency 32.
- Reboot before each arm.

Key artifacts:

- OFF vs current Eager: `/tmp/st-hdd-bench/repeat2-passes-off-eager-213512`
- OFF vs lazy-body diagnostic: `/tmp/st-hdd-bench/repeat2-passes-off-lazy-214959`
- Lazy body + stable selection: `/tmp/st-hdd-bench/repeat2-passes-lazystable-220024`
- Current Eager + stable selection: `/tmp/st-hdd-bench/repeat2-passes-eagerstable-220630`

Summary:

| arm | throughput | pass 1 p50 TTFB | pass 2 p50 TTFB |
|---|---:|---:|---:|
| OFF | 120.15 obj/s | 441.0 ms | 11.9 ms |
| EAGER unstable | 147.23 obj/s | 247.3 ms | 118.0 ms |
| LAZYSTABLE | 175.90 obj/s | 295.6 ms | 11.9 ms |
| EAGERSTABLE | 178.62 obj/s | 271.5 ms | 11.4 ms |

Conclusions:

- `BUCKIT_FASTGET_LAZY_BODY=1` was useful as a diagnostic and is preserved in commit
  `2752daca0`, but it is not needed for the hot-cache fix.
- Stable selection is the critical fix: the same object must select the same decode
  shard set across requests if we want repeated-object cache reuse.
- Current Eager + stable selection is the best tested candidate so far. It preserves
  Eager's lower cold-pass TTFB while matching OFF's hot-pass TTFB collapse.
- The final code drops the lazy-body diagnostic and keeps stable selection as the
  actionable behavior to validate further.

Implementation note:

- Stable selection keeps the object-hash start position and has no per-request
  rotation in either local-first remote selection or spread selection.
- The repeat-pass generator remains checked in because it is the simplest way to
  catch cache-shard-selection regressions.

Availability note:

- The default fast path should not need to open more than M shards when the chosen
  drives are known online.
- Before selecting the stable primary M, exclude drives that cluster/admin state
  already knows are offline, unreachable, or otherwise unavailable.
- If one of the selected M header/body reads fails quickly despite being considered
  online, retry with a deterministic backup shard. This is cheaper than paying the
  M+1 hedge cost on every GET.
- Hedging should remain an optional tail-latency mode, not the steady-state default,
  unless future tests show the tail win is worth the extra HDD pressure.

## 14. M+1 hedge diagnostic

A follow-up diagnostic tested stable primary `M` plus one deterministic hedge shard:

- Env: `BUCKIT_FASTGET_EAGER=1`, `BUCKIT_FASTGET_SPREAD=1`, optional
  `BUCKIT_FASTGET_HEDGE=1`.
- The hedge opens one extra stable candidate beyond the primary M.
- The fast-open path prefers the stable primary M if all are ready and agreeing.
- If one primary is slow or bad, any agreeing M among the M+1 can proceed.
- Unused/in-flight candidates are cancel-closed/drained asynchronously.

Validation workload:

- Same 9,883-key repeat-pass run as section 13.
- Reboot before each arm.
- Artifacts: `/tmp/st-hdd-bench/repeat2-passes-eagerstable-hedge-223008`.

Summary:

| arm | throughput | pass 1 p50 TTFB | pass 1 p95 TTFB | pass 1 p99 TTFB | pass 2 p50 TTFB |
|---|---:|---:|---:|---:|---:|
| EAGERSTABLE | 196.86 obj/s | 250.3 ms | 590.3 ms | 853.7 ms | 11.4 ms |
| EAGERSTABLEHEDGE | 181.83 obj/s | 288.4 ms | 589.0 ms | 792.2 ms | 12.1 ms |

Interpretation:

- The hedge preserved hot-cache reuse: pass 2 p50 TTFB stayed near 12 ms.
- It reduced throughput by about 8% versus EAGERSTABLE in this run and worsened pass
  1 p50.
- It was still much faster than OFF in the same repeat-pass methodology:
  EAGERSTABLEHEDGE was 181.83 obj/s versus OFF's 120.15 obj/s from section 13
  (`+51%`), with pass 2 p50 TTFB still near OFF's hot-cache 12 ms behavior.
- It slightly improved pass 1 p99/max, suggesting it can rescue some slow-primary
  tails, but the extra candidate open/read pressure is not free on HDDs.
- Current recommendation: do not make M+1 hedge the default from this one run. Keep
  it diagnostic and only revisit if future workloads show tail latency is worth the
  throughput/median tradeoff.

## 15. Two MiB repeat-pass validation and multi-block Eager fix

The 2 MiB run initially exposed a correctness bug before the benchmark could run:
local multi-block Eager opened the shadow body as:

```text
ReadFileStream(current/part.1, offset=singleTripHeaderLen, length=-1)
```

For local `xlStorage.ReadFileStream`, `length < 0` returns the opened `*os.File`
directly and does not seek to `offset`. That means the local multi-block Eager body
stream started at byte 0, reread the 1 KiB single-trip header as if it were bitrot
body data, and returned 503 on 2 MiB FastGet reads. The 640 KiB tests did not hit
this because single-block Eager already used a bounded body read.

Fix: local multi-block Eager now opens the body with an exact bounded bitrot length:

```text
ReadFileStream(current/part.1, offset=singleTripHeaderLen, length=singleTripBitrotBodyLen(header))
```

Targeted tests after the fix:

```sh
GOCACHE=/tmp/st-hdd-bench/gocache GOTMPDIR=/tmp/st-hdd-bench/gotmp \
  CGO_ENABLED=0 go test -tags kqueue,dev -run 'SingleTrip|FastGet|ParallelReader' ./cmd/
```

Result:

```text
ok   github.com/buckit-io/buckit/cmd   6.874s
```

2 MiB validation workload:

- Seeded 1,002 objects under prefix `2mbench/` using `warp put --obj.size=2MiB`.
- Key file: `/home/ubuntu/once-get-keys-2mbench.txt`.
- `--repeat=2 --repeat-mode=passes`, concurrency 32.
- Reboot before each arm.
- Artifacts: `/tmp/st-hdd-bench/repeat2-passes-2m-off-eagerstable-hedge-231116`.

Aggregate:

| arm | throughput | p50 TTFB | p95 TTFB |
|---|---:|---:|---:|
| OFF | 110.31 obj/s, 220.62 MiB/s | 56.6 ms | 602.0 ms |
| EAGERSTABLE | 126.99 obj/s, 253.98 MiB/s | 49.5 ms | 565.7 ms |
| EAGERSTABLEHEDGE | 120.88 obj/s, 241.76 MiB/s | 76.0 ms | 442.7 ms |

Important caveat: this aggregate is a 50/50 cold-pass + hot-pass mix by construction
and is not a production throughput estimate for a 50 TB+ per-host working set. Pass 2
assumes every object was just read once and remains hot in the file-system page cache;
that creates the ~11 ms p50 TTFB hot pass and inflates the aggregate, especially for
OFF. For large AI/object-storage deployments where the working set is far larger than
RAM, the pass 1 / exactly-once view below is the more relevant comparison.

Pass split:

| arm | pass 1 p50 TTFB | pass 1 p95 TTFB | pass 2 p50 TTFB | pass 2 p95 TTFB |
|---|---:|---:|---:|---:|
| OFF | 307.2 ms | 743.0 ms | 11.0 ms | 20.1 ms |
| EAGERSTABLE | 253.9 ms | 783.3 ms | 11.3 ms | 19.5 ms |
| EAGERSTABLEHEDGE | 243.4 ms | 564.9 ms | 12.0 ms | 52.2 ms |

Interpretation:

- EAGERSTABLE is the best 2 MiB throughput result in this run: +15% obj/s over OFF.
- Stable selection again preserves the hot pass: pass 2 p50 TTFB is about 11-12 ms
  for all arms.
- Do not use the aggregate mixed-pass throughput to estimate production behavior for
  very large datasets; it overweights hot-cache reads. For such deployments, use
  pass 1, exactly-once key traversal, or a working set larger than RAM/cache.
- The M+1 hedge improves cold-pass TTFB tail for 2 MiB (`p95 564.9 ms` vs
  EAGERSTABLE `783.3 ms`), but loses throughput and has worse hot-pass p95 TTFB.
- Recommendation remains unchanged: default should be stable spread exactly M;
  M+1 hedge is a tail-latency diagnostic/option, not the default.

## 16. Recommended next steps

### 16.1 Make the default candidate explicit

Current best default candidate:

```text
FAST_GET=1
BUCKIT_FASTGET_EAGER=1
BUCKIT_FASTGET_SPREAD=1
BUCKIT_FASTGET_HEDGE=0
```

Stable selection is now unconditional. This is `EAGERSTABLE`: stable spread exactly
M, no M+1 hedge. It is the best tested balance so far:

- Preserves repeated-object cache reuse.
- Avoids local-HDD hot spotting.
- Avoids paying the extra M+1 open/read cost on every GET.
- Keeps the hedge path available as a diagnostic/tail-latency option.

### 16.2 Add offline-drive replacement without steady-state hedging

The default path should not open more than M shards when selected drives are known
online. Availability should be handled before and after selection:

- Before selecting stable primary M, exclude drives that cluster/admin state already
  knows are offline, unreachable, healing-unavailable, or otherwise unavailable.
- If one selected M header/body read fails quickly despite being considered online,
  retry once with a deterministic backup shard.
- Keep backup choice stable per object so cache reuse remains predictable.
- Do not pay the M+1 hedge cost on every GET unless an operator explicitly enables
  tail-latency mode.

This should provide most of the availability benefit without adding HDD pressure to
the healthy steady state.

### 16.3 Run production-relevant benchmarks

Do not use repeat-pass aggregate throughput as the production estimate. It is useful
for cache diagnostics but overweights hot-cache reads.

Next benchmark set should use exactly-once traversal / pass 1 style results:

- 2 MiB exactly-once, concurrency 32 and/or 64.
- 8 MiB or 16 MiB exactly-once to check whether Eager benefit diminishes as body
  transfer dominates.
- 64 MiB if the target includes AI-training-style packed shards.
- Dataset larger than RAM/page cache if feasible; otherwise avoid repeated keys and
  report the limitation clearly.

Key metrics to report:

- obj/s and MiB/s.
- TTFB p50/p90/p95/p99.
- total request p50/p90/p95/p99.
- pass1/exactly-once throughput estimate, not mixed hot/cold aggregate.
- drive-level distribution and saturation if available.

### 16.4 Decide large-object positioning

Expected trend: Eager's relative throughput win shrinks as object size grows because
large-object GETs are dominated by body read, network transfer, erasure decode, and
client streaming.

If 8-64 MiB results show small throughput gains but persistent TTFB gains, position
FastGet/Eager as:

- strong for small/medium objects and metadata-heavy access,
- useful for TTFB-sensitive GETs,
- less central for AI-training large-shard sustained throughput.

For AI-training workloads, also benchmark packed-shard patterns separately:

- 64-512 MiB sequential GETs,
- range reads if loaders sample within large shard objects,
- aggregate sustained throughput over a working set larger than cache,
- input-pipeline stall behavior if integrated with a training loader.

### 16.5 PR cleanup guidance

Before a production PR, decide what remains in the final patch:

- Keep stable selection.
- Keep the multi-block Eager bounded-body fix.
- Keep the repeat-pass generator if benchmark tooling is part of the branch.
- Keep `BUCKIT_FASTGET_HEDGE` only if diagnostic flags are acceptable; otherwise
  split it into a separate experimental commit/branch.
- Remove or gate noisy low-level profiling/tracing if it is not suitable for
  production builds.
- Document the default env combination and the exact benchmark methodology used to
  justify it.
