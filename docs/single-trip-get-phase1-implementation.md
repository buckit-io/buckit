# Single-Trip Direct GET — Phase 1 Prototype Implementation Plan

**Status:** Implementation plan / prototype scope

**Companion to:** `docs/single-trip-get-design.md` (read it first for the layout,
header, quorum, and durability model). This document is the concrete build plan
for a **measurement prototype** of the latest-GET fast path, not the production
design.

---

## Goal

Validate the performance claim in design §6 — that removing the `xl.meta`
fan-out read phase from a healthy latest GET buys lower first-byte latency and
higher seek-bound throughput — with the smallest faithful change to the real
server. Everything not relevant to the GET-side measurement is deliberately
faked or skipped.

### What the prototype measures, and what it does not

The §6 claim splits into two physically distinct effects that need different
measurements:

- **First-byte latency** is governed by *round-trip phases*. Today: metadata
  fan-out (≈ one parallel seek across the set) **then** shard fan-out (≈ one
  parallel seek) = two sequential phases. The fast path collapses this to one
  phase. This is only observable when the disks are genuinely parallel — i.e.
  on a real multi-spindle cluster, never on a serial single-host benchmark.
- **Saturated throughput** is governed by *seeks-per-GET-per-disk*. A data disk
  does a metadata seek plus a data seek (2) today, versus header + data in one
  open (1). This is per-spindle and IOPS-bound under concurrency.

A single-host benchmark that reads 16 `xl.meta` files serially does **not**
model either effect — in a real deployment those 16 reads fan out concurrently
across 16 separate drives, so the metadata phase costs roughly one seek of
wall-clock latency, not sixteen. The prototype is therefore exercised on the
cluster rig (`testing/cluster`, 4 nodes × 4 drives = 16 drive paths in one EC:4
set), with an optional single-spindle IOPS sanity check as the only honest
single-host experiment.

**Caveat on the rig — read before trusting absolute numbers.** The rig's drives
are XFS-on-loopback files inside Docker (on macOS, inside a LinuxKit VM). They
are 16 *independent drive paths* and give a correct functional A/B, but they are
**not 16 independent physical HDD spindles**: they share backing storage and an
extra page-cache layer, so they do not faithfully reproduce HDD seek latency.
The rig is therefore good for *relative* off-vs-on comparison and for proving the
mechanism, but **HDD seek-behavior evidence requires running the same build on a
host with real, separate spinning disks** (see §9.2 and §9.9). Treat container
results as ratios between arms, not as absolute HDD figures.

### Deliberate prototype shortcuts

Stated up front because they bound what the numbers mean:

- `current/part.1` is an **additive shadow copy** of the shard, not a moved
  directory. This doubles write cost and storage. **Only GET numbers are valid;
  PUT numbers are meaningless.**
- Single-part, latest-only, non-versioned, non-transitioned, non-encrypted,
  non-compressed, non-object-lock, non-inline; **offset-0 reads only** (full
  object or a range starting at byte 0 — required for the true single-trip stream
  reuse in §4). Everything else falls through to the existing path.
- No crash consistency, no repair, no delete-marker reconstruction, no
  `renameat2` exchange, no `versions/<id>/`. `xl.meta` stays canonical, so
  fallback is always correct.

---

## 0. Feature flag & scope guard

- Read `BUCKIT_FAST_GET=1` once at startup into `globalFastGetEnabled`
  (prototype-only gate; no config subsystem wiring).
- Eligibility is checked in **two stages**, because some predicates are knowable
  from the request alone but others require the object's metadata — which on the
  fast path we only learn *from the direct header*, not from `xl.meta`. Mixing
  them into one pre-`getObjectFileInfo` check would be wrong (we'd be testing
  encryption/transition/part-count before we have any metadata).

  **Stage 1 — request-level precheck (before any read), `fastGetRequestEligible`:**
  - flag on; `opts.VersionID == ""` (latest);
  - bucket versioning is Unversioned (from bucket metadata, available without
    object metadata);
  - no SSE-C request headers (request-detectable); not a replication request;
  - range is absent or **starts at byte offset 0** (see §4 — true single-trip
    requires an offset-0 read; non-zero offsets fall back in Phase 1).

  **Stage 2 — header-level validation (after reading the direct header),
  `fastGetHeaderValid`:** confirm from the quorum header that the object is
  single-part, not a delete marker, not transitioned, not SSE-S3/KMS encrypted,
  not compressed, not object-locked, not inline, and (under the default
  response-metadata scoping of §1.1) carries no custom `x-amz-meta-*` user
  metadata or object tags, and that its bounded response headers did not exceed
  the §1.1 byte budget (over-cap objects are marked ineligible at write time and
  must fall back, never truncate). Any failure → fall back to `getObjectFileInfo`.
  These facts are carried in the header (§1) precisely so we can validate them
  without `xl.meta`.
- The existing RLock path is unchanged (we still take the read lock as today).

---

## 1. On-disk header format

New file `cmd/singletrip-header.go`. A fixed-size, self-describing, CRC'd header
prefixed onto each disk's `current/part.1`:

```
current/part.1 = [ singleTripHeader (fixed size, prototype: 1024B) ][ existing shard bytes: hash_0|block_0|... ]
```

`singleTripHeader` carries the design §2.1 minimum needed to rebuild a
`FileInfo` without reading `xl.meta`:

- magic + format version + `headerLen`; header CRC (xxhash) over the rest;
- a **direct-path signature** `[4]byte` for cross-disk quorum grouping —
  **computed explicitly** at write time, *not* read from `FileInfo` (which does
  not expose `xlMetaV2VersionHeader.Signature`). The signature must cover **every
  field that affects decode correctness**, not just identity:
  `directSig = xxhash32(versionID ‖ modTimeNanos ‖ ErasureM ‖ ErasureN ‖
  ErasureBlockSize ‖ ErasureDist ‖ bitrotAlgo ‖ partCount ‖ PartSize ‖
  ActualPartSize ‖ Size ‖ ETag ‖ flags ‖ contentType ‖ contentEncoding ‖
  cacheControl ‖ expires ‖ storageClass)` — the bounded response-metadata fields (§1.1) are
  included so copies that would send *different* response headers cannot group
  together, and the §3.1 quorum additionally checks them for byte-equality.
  `ErasureIndex` is deliberately
  **excluded** (it differs per disk). A 4-byte digest can collide, so the quorum
  step (§3.1) does **not** trust the signature alone: it groups by `directSig`
  and then additionally requires the common layout fields above to be
  byte-equal across the group, and the per-disk `ErasureIndex` values to be
  **distinct and valid**. **`ErasureIndex` is 1-based in Buckit** — valid values
  are `1..M+N` (`cmd/erasure-metadata.go:81` checks `Index > 0 && Index <= M+N`),
  the **data** shards are indices `1..M`, and the on-disk array position is
  `Index-1`. Signature first for cheap grouping, field equality + index validity
  for correctness;
- `ErasureM, ErasureN, ErasureIndex, ErasureBlockSize, ErasureDist []uint8`;
- `Size, ModTime, PartSize, ActualPartSize`, bitrot algorithm;
- ETag, VersionID;
- validation flags needed by Stage-2 eligibility (§0): `isDeleteMarker`,
  `isInline`, `isTransitioned`, `isEncrypted`, `isCompressed`,
  `isObjectLocked`, `partCount`, and — for the §1.1 default scoping —
  `hasUserMeta` and `hasObjectTags` (so the read path can reject objects with
  custom `x-amz-meta-*`/tags without `xl.meta`). These flags are covered by
  `directSig` and the §3.1 byte-equality check like the other layout fields;
- **S3 response-metadata subset** (see §1.1) — without it the fast path would
  send wrong/missing response headers.

Prototype implementation note: Phase 1 uses a small fixed-binary payload encoded
by `cmd/singletrip-header.go`, not generated `msgp`, to keep the measurement
patch self-contained. It is still **zero-padded to a fixed
`singleTripHeaderLen`** so the payload offset is a compile-time constant — no
second read is needed to discover where the shard begins. Production can switch
the inner payload to generated `msgp` if we want schema evolution support after
the measurement proves useful.

### 1.1 Response-metadata fidelity (don't just decode the bytes)

Decoding the payload correctly is necessary but not sufficient: a normal GET also
returns response headers that `ToObjectInfo` derives from `fi.Metadata` and
`setObjectHeaders` sends to the client (`cmd/erasure-metadata.go:97`,
`cmd/api-headers.go:125,149`) — `Content-Type`, `Content-Encoding`,
`Cache-Control`, `Expires`, **a non-STANDARD `x-amz-storage-class`** (kept in
`UserDefined` by `cleanMetadata`, `cmd/object-api-utils.go:401`, and emitted by
`setObjectHeaders`), object tags, and arbitrary `x-amz-meta-*` user metadata. The
fast path synthesizes `ObjectInfo` from the header, not from `xl.meta`, so it must
carry enough to reproduce these or it will silently drop/garble them — and the §6
byte-and-header equality test (which compares against a flag-off GET) would catch
it as a diff.

Three parts, by boundedness:

- **Bounded common headers — carried in the fixed header, with explicit caps.**
  `Content-Type`, `Content-Encoding`, `Cache-Control`, `Expires`, storage class
  (and `ETag`, already present). These have **no inherent small bound** (a
  `Content-Type` or `Cache-Control` can be long), so the header reserves a fixed
  byte budget per field and a total budget within `singleTripHeaderLen`, e.g.
  `Content-Type ≤ 128`, `Content-Encoding ≤ 64`, `Cache-Control ≤ 128`,
  `Expires ≤ 40`, `storage-class ≤ 32` (tune to fit). **Over-cap is not truncated
  and not a silent failure: an object whose encoded subset exceeds the budget is
  marked Stage-2 ineligible (`shadowEligible` false on write, header-invalid on
  read) and falls back.** All of these fields are included in `directSig` and the
  §3.1 byte-equality check (so storage class can't be lost or mixed across a
  group).
- **Storage class specifically.** STANDARD is the default and emits no header;
  only a *non-STANDARD* `x-amz-storage-class` round-trips. Carry the value in the
  bounded subset above (covered by `directSig`/equality). If you prefer not to
  carry it, the alternative is to mark any non-STANDARD-storage-class object
  Stage-2 ineligible — but do not simply ignore it, or the fast path would drop
  the header.
- **Unbounded `x-amz-meta-*` user metadata and object tags.** These have no size
  bound, so a fixed header cannot hold them. Phase 1 picks one of:
  - **(default) Scope it out, with a caveat.** The benchmark/correctness objects
    carry no custom user metadata or tags; the §6 header-equality test asserts the
    bounded subset only. Documented limitation: objects *with* user metadata/tags
    are not fast-served faithfully and must be excluded from the fast path (treat
    presence of user-meta/tags as a Stage-2 ineligibility, falling back). This
    keeps the constant-offset design and is fine for a perf prototype.
  - **(upgrade) Variable-length metadata region.** Carry `metaLen` in the fixed
    header, followed by a msgpack blob of the full `MetaUser`/relevant `MetaSys`,
    then the shard. The payload offset becomes `fixedLen + metaLen` (read from the
    fixed part) — still consumed from the *same* single open, so the single-trip
    invariant holds; only the "compile-time-constant offset" simplification is
    relaxed. Use this only if full-fidelity GETs matter for the evaluation.

  Phase 1 uses the default (scope-out) unless the perf objects need custom
  metadata.

- **Compression and object-lock metadata are also scoped out.** Compression is
  not only response metadata: normal GET uses the internal compression marker and
  actual-size metadata to wrap the object stream in decompression. If the fast
  path synthesized an `ObjectInfo` without those fields, it would stream raw
  compressed shard bytes and compute ranges against the wrong size. Therefore any
  object carrying `X-Minio-Internal-compression` is Stage-2 ineligible and falls
  back. Object-lock retention/legal-hold metadata similarly affects
  `x-amz-object-lock-*` response headers; Phase 1 does not carry it, so locked
  objects are ineligible and fall back.

**Bitrot.** The per-block bitrot hashes stay **interleaved in the shard payload
exactly as today** (they are not moved into the header), and the fast path uses
the **streaming** bitrot reader exclusively (`newStreamingBitrotReader`), which
reads each block's hash inline from the stream. This is deliberate: the streaming
reader needs only the algorithm (carried in the header), **not** a per-block
checksum slice, so we do not have to serialize bitrot sums into the header. The
non-streaming/whole reader (`newWholeBitrotReader`, which *does* require a `sum`)
is never used on the fast path. `erasure.Decode` and verification are therefore
byte-for-byte unchanged; only the file's start offset shifts by
`singleTripHeaderLen`.

---

## 2. Write side — shadow `current/part.1`

Hook into `putObject` around the canonical commit (`commitRenameDataDir` /
`RenameData`, `cmd/erasure-object.go:1577`), gated on the flag. Note the ordering
required by §2.1: prior-shadow **invalidation runs *before* the canonical commit**
and new-shadow **installation runs *after*** it — it is not a single
post-commit step. This is an **additive shadow copy**, not a directory move
(prototype shortcut; GET-perf only).

**The disks are not all local.** The evaluation rig is distributed, so
`onlineDisks[i]` is a `storageRESTClient` (remote) for most shards, not a local
`xlStorage`. A local-only helper on `xlStorage` cannot run on remote disks. The
shadow write must therefore go through the `StorageAPI` interface, which is
transparently local or remote. Two viable approaches:

- **Prototype approach (no new RPC):** from the landing node, for each
  `onlineDisks[i]` (local or remote) compose the shadow file from the bytes that
  were just written, reusing **existing** `StorageAPI` calls — read the committed
  shard via `ReadFileStream(bucket, DataDir/part.1)` and write the shadow with a
  **streaming** `CreateFile(..., fileSize, io.MultiReader(bytes.NewReader(header), shardStream))`.
  `CreateFile` requires the exact `fileSize` up front and `xlStorage` validates
  the bytes written against it, so compute it explicitly:
  `fileSize = singleTripHeaderLen + fi.Erasure.ShardFileSize(fi.Parts[0].Size)`
  (`cmd/erasure-metadata.go:54`) — the source `DataDir/part.1` is the *encoded*
  shard file (it already includes the interleaved bitrot hashes), so
  `ShardFileSize` gives its on-disk length, and the header adds a fixed
  `singleTripHeaderLen`.
  **`CreateFile` is `O_EXCL` (`cmd/xl-storage.go:2128`) — it will not overwrite.**
  So write to a fresh temp path (`current/part.1.tmp-<uuid>`) and then
  `RenameFile` it over `current/part.1`, which replaces atomically per disk and
  works local+remote. This is also why the stale-shadow rule below is mandatory.
  Do **not** use `WriteAll` for this: it materializes the whole prefixed shard in
  memory, which is fine only for tiny test fixtures, not for the multi-MiB
  objects the perf run uses. `CreateFile` streams, so memory stays bounded. Both
  calls already work over storage REST/grid, so no protocol change is needed.
  Cost: an extra full read+write per disk at PUT time — an explicit PUT
  distortion, acceptable because **only GET numbers count** (see shortcuts).
- **Cleaner approach (if PUT distortion matters):** add a
  `WritePrefixedShard(...)` method to `StorageAPI` with storage REST/grid
  plumbing so each disk writes its own shadow locally from its already-present
  shard. More code; avoids the cross-node copy.

Phase 1 uses the prototype approach.

- Build `singleTripHeader` per disk from `fi` (plus that disk's `ErasureIndex`),
  then write `pathJoin(object, "current", "part.1")` = header followed by the
  shard bytes. No `renameat2`, no fsync ordering — out of scope.
- **Decide eligibility, then either install or invalidate — never "do nothing."**
  A single `shadowEligible(fi, bucket, opts)` guard, mirroring the read-side
  eligibility, returns false for: multipart (`len(fi.Parts) != 1`), inline /
  zero-byte (no `DataDir/part.1` exists), any encryption (SSE-C/S3/KMS),
  compressed objects, object-lock retention/legal-hold metadata,
  transitioned/remote, versioned or version-suspended buckets, delete markers,
  legacy-v1 objects, objects carrying custom `x-amz-meta-*` user metadata or
  object tags (under §1.1 default scoping), and objects whose bounded response
  headers (§1.1) exceed the per-field/total byte budget. **But "ineligible" must not mean "leave `current/`
  untouched"** — see §2.1: if a prior eligible version left a shadow, doing
  nothing leaves it readable and fast GET returns stale bytes. So on every
  successful canonical PUT, eligible ⇒ install the shadow (after commit);
  ineligible ⇒ **invalidate** the prior `current/` to the §2.1 `oldN+1`
  threshold-confirmed level *before* the canonical commit, or **abort** — never
  the unsafe "best-effort, ignore failures" path §2.1 rules out. Writing direct
  headers for
  objects the prototype must never fast-serve would also waste storage and risk a
  header that disagrees with what the read path can validate; the read side still
  re-validates from the header (§0 Stage 2) — defense in depth, not a substitute.

### 2.1 Stale-shadow invalidation (correctness-critical)

`current/part.1` is a **stable, reused path** — every overwrite and delete of the
key targets the same path. Unlike the per-version `DataDir`, it is *not*
write-once. The hard invariant is:

> **A stale `current/` shadow must never survive as a valid quorum after a newer
> canonical commit (overwrite or delete).** If the fresh shadow cannot be
> installed to quorum, no old shadow may remain that could form one — GET must be
> forced to fall back to `xl.meta`.

The corollary that's easy to miss: **every successful canonical PUT must take a
definite action on `current/` — install or delete — never "do nothing."** Doing
nothing is only safe when no prior shadow exists, which the write path cannot
assume. If this is violated, fast GET can return bytes (or existence) that
disagree with the canonical `xl.meta`, which is exactly the failure the whole
"xl.meta stays canonical" design forbids. Concretely:

- **Overwrite, new version eligible.** All steps run under the object write lock
  already held by `putObject`. Follow the uniform ordering: **(1)** invalidate the
  prior `current/` to the `oldN+1` threshold (§2.1, old-layout-derived); **(2)** canonical commit
  (`RenameData`); **(3)** install the new shadow (temp-write-then-`RenameFile`).
  Do **not** install-the-new-shadow-before-commit: if the commit then failed, a
  fresh shadow with the *new* `directSig` would be readable while `xl.meta` still
  points at the old version — a stale-*new* read, the mirror of the stale-old bug.
  Deleting first also means step 3 lands on an empty path, sidestepping the
  `O_EXCL` overwrite problem; the temp+`RenameFile` is retained only to maximize
  new-shadow coverage on disks that may still hold a leftover old file.
- **Overwrite, new version *ineligible* (the asymmetric case).** When an eligible
  object is overwritten by an ineligible one — inline/zero-byte, multipart,
  compressed, object-locked, encrypted, transitioned, versioned/suspended,
  delete-marker-like, or legacy-v1 — the write path produces no new shadow, so
  the **prior** eligible shadow would remain readable and fast GET would pass
  Stage 2 on *old* metadata and return stale bytes. Therefore: on every
  successful canonical PUT where
  `shadowEligible(...) == false`, **delete `current/` to the invalidation
  threshold below before returning success.**
- **Delete (non-versioned).** A delete in scope actually removes the object, so
  add an **invalidation hook in the delete path** (`deleteObject` /
  `DeleteObjects`) that deletes `current/` to the invalidation threshold below,
  under the write lock. (Versioned deletes create a delete marker and are out of
  scope per §2 — they never had a shadow.)

#### Invalidation success condition (there is no read-side staleness backstop)

"Best-effort" is **not** a correctness condition here, and this is the subtle
part: the read path deliberately never consults `xl.meta`, so **nothing on the
read side can detect that a surviving old shadow is stale.** If an overwrite/
delete removes `current/` from only some disks and the old shadow still has
enough shards to satisfy a fast read, `tryFastGet` will group the old `directSig`,
reach quorum, and return stale bytes. Invalidation must therefore *prove* the old
shadow can no longer form a fast-path read:

- A fast read requires (§3.1) ≥ `oldM` headers agreeing on one `directSig`,
  **and** all `oldM` data-index shards present. So the old shadow is provably
  unreadable once **at most `oldM-1` of its `current/` files remain** — i.e.
  invalidation must **confirm removal (or confirmed absence) of `current/` on at
  least `oldN + 1` disks**, where `oldM`/`oldN` are the **stale shadow's own
  erasure layout**, `oldM+oldN − (oldN+1) = oldM−1 < oldM`, which simultaneously
  drops the old `directSig` group below quorum *and* guarantees at least one
  data-index shard is gone (only `oldN` parity positions exist).
- **The threshold is keyed to the *old* layout, not the new object's.** This is a
  trap: an overwrite can change the erasure layout — storage class, max-parity
  config, or availability optimization can give the *new* object a different
  parity than the old (`cmd/erasure-object.go:1298`). Using the new object's `N`
  (or a default `EC:` value) can under-delete and leave a readable stale shadow.
  So derive `oldN` from the **existing `current/` headers** (each carries
  `ErasureN`, §1) before deleting; if those headers can't be read reliably on
  enough disks, fall back to a **conservative threshold = `maxPossibleParity + 1`**
  for the set (e.g. `setDriveCount/2 + 1`, ≥ any layout the old shadow could have
  had). Never assume the old parity equals the new parity. This threshold is
  stricter than the canonical write quorum, which is required because — unlike a
  normal write — a stale fast read has no second check.
- **Ordering is what makes failure cluster-safe — invalidate *before* the
  canonical commit.** A process-local kill switch is **not** sufficient: this is a
  distributed cluster, and another serving node would still fast-serve the stale
  `current/` shadow. The fix is ordering, not a flag. Under the object write lock,
  delete the prior `current/` to the `oldN+1` threshold **before** the canonical
  metadata commit (`RenameData`) that supersedes it (for delete, before the
  tombstone/removal commit). Then:
  - **Threshold met →** proceed with the canonical commit; for an eligible
    overwrite, install the new shadow afterward (a failed install just yields
    fallback, never staleness).
  - **Threshold *not* met →** **abort the operation before the canonical commit
    and return an error** (client retries). Because nothing newer was committed,
    the old `current/` shadow still agrees with the still-current old `xl.meta` on
    *every* node — the cluster stays consistent, with no window where new canonical
    state coexists with a readable old shadow anywhere. This is the cluster-safe
    behavior the local kill switch failed to provide.
  - Optional heavier alternative, only if you want to keep serving while degraded:
    disable fast GET **cluster-wide** via a peer/config broadcast that every
    serving node observes (e.g. a peer-REST notification flipping `globalFastGetEnabled`
    on all nodes) before reporting success. Phase 1 prefers abort-before-commit;
    the cluster-wide switch is noted only for completeness.
- **Known residual (prototype-only):** a disk that was offline during invalidation
  can rejoin later still carrying its old `current/` file. If enough such disks
  rejoin to re-cross quorum before the key is next written, a stale fast read
  becomes possible again. Phase 1 does not add the scanner/repair that would
  reconcile this (design §5.3); it is an accepted prototype limitation, called out
  as **not production-safe**, and is why the production design keeps `xl.meta`
  authoritative with active reconciliation.

**Crash residuals under the new ordering (invalidate → commit → install).** With
invalidation required *before* the canonical commit (§2.1), the crash windows are
benign:

- **Crash after invalidation, before commit:** the old `current/` was already
  reduced below the `oldN+1` threshold, and `xl.meta` still names the *old* version
  (not yet committed). A torn/partial old shadow can no longer form a fast quorum,
  so GET falls back to `xl.meta` and serves the old canonical version — consistent.
- **Crash after commit, before/while installing the new shadow:** `xl.meta` names
  the new version. Either the new shadow isn't installed yet (GET falls back to
  `xl.meta` → new version, consistent) or it's partially installed; partial
  installs can't reach the new-version quorum, so GET still falls back. A stale
  *old* shadow cannot reappear because it was invalidated pre-commit.

In all cases the read path's quorum + `directSig` check plus "fewer than `M`
agreeing ⇒ fallback" is the backstop, and `xl.meta` stays authoritative. The one
residual that *does* remain is the offline-disk rejoin above. Phase 1 adds no
scanner/repair (design §4/§5); this is acceptable for a measurement prototype but
is **not production-safe** — the production design's `renameat2`/repair model
(design §4–5) is what closes it.

---

## 3. Read side — single-trip fast path

In `GetObjectNInfo` (`cmd/erasure-object.go:203`), insert before the
`getObjectFileInfo` call at line 239:

```go
if fastGetRequestEligible(ctx, bucket, object, rs, opts) {       // Stage 1 (§0)
    if gr, ok := er.tryFastGet(ctx, bucket, object, rs, h, opts, nsUnlocker, &unlockOnDefer); ok {
        return gr, nil
    }
    // not ok → fall through to existing path unchanged
}
fi, metaArr, onlineDisks, err := er.getObjectFileInfo(...)
```

### 3.1 The single-trip invariant (the point of the whole experiment)

The fast path must be **one open / one storage request per disk**: the same
stream that returns the header continues directly into the shard bytes. If we
instead read the header with one `ReadFileStream(...,0,headerLen)` and then let
the decoder open `current/part.1` a *second* time, we have preserved two request
phases and two seeks — which is exactly the cost the design claims to remove, so
the measurement would be meaningless. The implementation below therefore opens
each disk's `current/part.1` **once** and threads that one open reader through
both header validation and decode.

`tryFastGet` (new code in `cmd/singletrip-get.go`):

1. **One open per disk — but open to EOF, not a precomputed length.** The encoded
   shard length we'd want as the read length depends on header fields we have not
   read yet (size, part size, erasure layout, bitrot block count), so we *cannot*
   pass `headerLen + tillOffset` up front — that's a chicken-and-egg. Open instead
   with **`length = -1`** (stream to EOF):
   `rc, _ := disk.ReadFileStream(bucket, currentPartPath, 0, -1)`. One local open,
   or one storage-REST/grid request for remote disks. The decoder reads only as
   far as it needs and we then close; we never have to know the exact length in
   advance.
2. **Header off the front of that stream.** `io.ReadFull(rc, hdrBuf[:headerLen])`
   (`headerLen` is the fixed `singleTripHeaderLen`), verify the header CRC. `rc`
   is now positioned exactly at the shard payload (byte `headerLen`); for the
   streams we keep it is **not** closed and **not** reopened.
3. **Quorum** — group validated headers by the direct-path signature (§1) **and**
   require, within the group, byte-equal **layout *and* bounded response-metadata
   fields** (§1/§1.1 — `Content-Type`, `Content-Encoding`, `Cache-Control`,
   `Expires`, storage class, `ETag`, `Size`, `ModTime`) plus distinct valid `ErasureIndex` values
   (§1). The response-metadata fields are part of the equality check, not merely
   "present," so the synthesized `ObjectInfo` cannot be assembled from a group
   that disagrees on what headers to send. Require ≥ `DataBlocks` agreeing. If not,
   cancel all open streams (step 6) and return `ok=false` (fallback).
4. **Stage-2 header validation** (§0): single-part, not delete-marker, not
   transitioned, not inline, not compressed, not object-locked, not SSE-S3/KMS,
   and (default §1.1 scoping) **no custom `x-amz-meta-*` user metadata or object
   tags**. Any failure → cancel + fallback.
5. **Keep only the data-shard streams; wrap them into an index-ordered slice.**
   Phase 1 serves only the *fully healthy* M-data case: select the streams whose
   `ErasureIndex` is a **data index `1..M`**, and wrap each in a streaming bitrot
   reader that reads *from the already-open `rc`* (see §4). If any data index
   `1..M` is missing/invalid, **fall back** (we do not pull parity on the fast
   path; degraded reads go through `xl.meta`). This fallback boundary is only
   before response streaming starts. Once the fast path has emitted bytes to the
   client, it cannot restart through `xl.meta` without corrupting HTTP semantics.
   The Phase 1 from-stream bitrot reader is forward-only (`ReadAt` must be at the
   current stream offset), so a mid-stream disk error or bad block at stripe K can
   fail/truncate a GET that the normal path could have reconstructed by opening a
   parity shard at the correct offset. This is an availability limitation of the
   healthy-path prototype, not a data-integrity issue: bitrot verification still
   prevents wrong bytes from being served.

   **Ordering is a correctness requirement, not a detail.** Reed-Solomon decode
   is only correct if `readers[i]` corresponds to shard index `i`. The production
   path enforces this via `shuffleDisksAndPartsMetadataByIndex`
   (`cmd/erasure-metadata-utils.go:222`), which places each disk at
   `shuffled[Index-1]` using `Erasure.Distribution`. The fast path must do the
   same: build a **full `M+N`-length `readers` slice** and assign
   `readers[ErasureIndex-1] = fromStreamReader(...)` for each kept data shard
   (1-based data indices `1..M` → zero-based positions `0..M-1`), leaving the
   parity positions (zero-based `M..M+N-1`) `nil`. **Never**
   append readers in disk-response order — that can decode a valid quorum into
   corrupted bytes. The cleanest implementation reuses the production machinery:
   synthesize `fi.Erasure.Distribution` from the header, build per-disk
   `onlineDisks`/`metaArr` in set order, and let the existing
   `shuffleDisksAndPartsMetadataByIndex` + decode loop order everything (with the
   readers carrying their pre-opened streams).
6. **Close with cancel, never drain — for *all* fast-path streams.** Because
   every stream was opened with `length = -1` (to EOF), draining on close pulls
   *whatever is left of the shard*. That is wrong for two distinct sets of
   streams:
   - **Non-kept streams** (parity, minority/loser, and all streams on fallback):
     nearly the whole shard is undrained, so a drain pulls the entire unwanted
     shard.
   - **Kept streams that are not fully consumed** — an offset-0 range shorter
     than the object, or a client that disconnects early — still have a tail of
     the EOF stream pending; draining that tail wastes bandwidth and distorts
     range/early-abort measurements.

   The normal `streamingBitrotReader.Close` calls `xhttp.DrainBody`
   (`cmd/bitrot-streaming.go:153-156`) precisely to reuse the connection, which
   is the opposite of what we want here. So the from-stream reader (§4) and the
   header-only readers must both expose a `cancelClose()` that, for remote disks,
   cancels the request context (or closes the underlying body without draining)
   and for local disks just closes the fd. **No fast-path stream is ever
   `Close`d via the drain path — kept or not.** (To keep the headline perf
   numbers free of this subtlety, §9 benchmarks full-object GETs for the main
   latency/throughput cells and treats short ranges as a correctness case.)
7. **Synthesize `FileInfo` + the index-ordered disk/meta slices.** Build `fi`
   from the quorum header (Erasure params incl. `Distribution`, single Part,
   Size, ModTime, ETag) — enough for `NewGetObjectReader`
   (`cmd/erasure-object.go:283`) and the decode loop. `onlineDisks` and `metaArr`
   must be **full `M+N`-length slices in erasure-index order** (the same shape the
   decode loop expects after `shuffleDisksAndPartsMetadataByIndex`), with each
   kept data shard at position `ErasureIndex-1` and **every non-kept/parity
   position set to `OfflineDisk`/invalid `FileInfo`** — *not* a compacted slice of
   only the kept disks. The decode path is index-sensitive (§3.1 step 5); a
   compacted slice would misalign shards and decode to garbage.
8. **Lock cleanup on success — mirror the non-inline path exactly.** The fast path
   only serves non-inline objects, so on a successful stream `tryFastGet` must do
   what `cmd/erasure-object.go:288-304` does for non-inline reads: set
   `*unlockOnDefer = false` and attach `nsUnlocker` to the returned
   `GetObjectReader`'s cleanup (i.e. `fn(pr, h, pipeCloser, nsUnlocker)`), so the
   RLock is released when the reader is closed — **not** by the caller's `defer`.
   Getting this wrong either unlocks while bytes are still streaming or leaks the
   RLock. On the fallback path `tryFastGet` returns `ok=false` having touched
   neither, and the existing code at line 239+ takes over unchanged.

On any fallback, every stream opened in step 1 is `cancelClose`d before calling
`getObjectFileInfo`. The wasted cost is the `headerLen` bytes read **plus
whatever the transport and the remote `ReadFileStreamHandler` already read,
buffered, or put on the wire before the cancel landed** — for remote disks the
server begins copying the EOF stream immediately (`cmd/storage-rest-server.go`),
so cancellation bounds but does not zero this. It is still far less than a drained
full shard, and never a leaked fd. If you need the exact wasted-bytes figure for
the writeup, instrument the cancel path rather than assuming it is just the
header.

**Observability (required for the evaluation).** Maintain two process counters,
`fastGetHits` and `fastGetFallbacks` (atomic, prototype-only), incremented on the
fast-path success and on every fallback respectively. Phase 1 exposes them under
the existing `/api/requests` metrics group as `fast_get_hits_total` and
`fast_get_fallbacks_total`, labeled `type="s3"`. Without this you cannot tell
whether the "ON" arm actually exercised the fast path or silently fell back to
`xl.meta`, which would make a null result unfalsifiable. The evaluation playbook
(§9) checks these counters after every run.

---

## 4. Decode — reusing the already-open stream (the surgical reader change)

The decode loop in `getObjectWithFileInfo` (`cmd/erasure-object.go:351-390`)
builds a `streamingBitrotReader` per disk, and that reader **opens its own**
`ReadFileStream` on first `ReadAt` (`cmd/bitrot-streaming.go:168-176`). For the
fast path we must *not* let it open a second time — it has to consume the stream
`tryFastGet` already opened in §3.1.

Why this works for Phase 1's scope: eligibility restricts the fast path to
**offset-0 reads** of single-part objects, so the decoder reads the payload
sequentially from byte 0. The header sits at file byte 0; once we have consumed
`headerLen` bytes, the open stream is positioned exactly where the streaming
bitrot reader's first `ReadAt(offset=0)` expects to begin. No seek, no reopen.
(Non-zero-offset ranges would require repositioning the shared stream; those are
excluded in Stage 1 and fall back — this is the honest boundary of the
single-trip claim.)

**Length semantics — raw shard offset vs encoded file bytes.** Be precise about
the two different "lengths" here, because conflating them under-reads the file:

- The decoder's `partOffset/partLength/tillOffset` are **raw shard** quantities
  (object bytes, no hashes).
- The **physical `current/part.1`** is `headerLen` + the *encoded* bitrot stream,
  where the encoded stream interleaves a hash before every `shardSize` block.
  `newStreamingBitrotReader` already performs this conversion internally
  (`cmd/bitrot-streaming.go:210`: `tillOffset = ceilFrac(tillOffset, shardSize)*hashSize + tillOffset`).

Because §3.1 opens the stream with **`length = -1` (to EOF)**, `tryFastGet`
never has to compute the encoded length itself — it just consumes `headerLen`
raw bytes and hands the rest to the bitrot reader, which reads the correct number
of hash+block bytes per its existing math. The header still carries `Size`,
`PartSize`, and `ActualPartSize` (raw quantities) so the synthesized `FileInfo`
and the range/offset arithmetic in `getObjectWithFileInfo` are correct; only the
*open length* is delegated to EOF.

Change to `streamingBitrotReader` (`cmd/bitrot-streaming.go`):

- Add an optional pre-opened reader field, e.g. `rc io.ReadCloser` supplied at
  construction via a new `newStreamingBitrotReaderFromStream(rc, ...)`.
- In `ReadAt`, when `b.rc` is already set (line 168 branch), skip the
  `disk.ReadFileStream(...)` open entirely and read straight from the supplied
  stream; initialize `currOffset = 0`. The existing lazy-open path is untouched
  for all other callers.
- The `streamOffset`/`tillOffset` math (lines 171, 210) and the per-block
  hash-then-block verification (lines 185-199) are **unchanged** — the only
  difference is *where the bytes come from* (a handed-in stream vs. a fresh
  open).
- **`Close` must cancel, not drain.** The default `Close`
  (`cmd/bitrot-streaming.go:153-156`) calls `xhttp.DrainBody` for connection
  reuse. The from-stream reader was opened to EOF (§3.1), so if it is only
  partially consumed (short range, early client abort, or it's a parity/loser
  stream) draining pulls the rest of the shard. The from-stream variant must
  therefore override `Close` to cancel the underlying request/body **without**
  draining — this is the `cancelClose()` referenced in §3.1 step 6.

Factor a `fastGetObjectStream` that clones the decode-loop body but feeds the
pre-opened readers, rather than overloading `getObjectWithFileInfo` with
conditionals — keeps the production path pristine and the prototype cleanly
removable.

**This is the change that makes or breaks the experiment:** verify by tracing
syscalls (or counting `ReadFileStream` calls) that an ON-arm GET issues exactly
**one** open per participating disk, not two. If you see two, the measurement is
invalid regardless of the numbers.

---

## 5. Fallback correctness

Every failure mode returns `ok=false`, and the caller runs today's
`getObjectFileInfo` path against untouched `xl.meta`: missing/short
`current/part.1`, header CRC failure, no signature quorum, out-of-scope range,
decode/bitrot failure. Because the shadow path is purely additive and `xl.meta`
is canonical, fallback is always correct. No request-time repair queue in the
prototype (design §5.2 is deferred).

---

## 6. Correctness tests (before any perf run)

`cmd/singletrip-get_test.go`, run with `-tags kqueue,dev`. Each test asserts on
the `fastGetHits`/`fastGetFallbacks` counters (§3) so "correct bytes" and "took
the path we think it did" are checked separately.

- **Shadow written:** PUT with flag on of a **non-inline** object → assert
  `current/part.1` exists with a valid header on quorum disks.
- **Byte- and header-identical on the fast path:** GET with flag on returns
  identical **body bytes and response headers** to a flag-off GET — assert the
  §1.1 bounded subset (`Content-Type`, `Content-Encoding`, `Cache-Control`,
  `Expires`, `x-amz-storage-class`, `ETag`, `Content-Length`, `Last-Modified`)
  matches, not just the body — with `fastGetHits` incremented. Sizes **above the
  inline cutoff only**: 64 KiB / 1 MiB / 16 MiB, full object + range starting at
  offset 0. (4 KiB is omitted here — see the inline case below.) Set a non-default
  `Content-Type` **and** a non-STANDARD storage class on at least one fixture so
  both header paths are actually exercised.
- **User-metadata/tags, compression, and object-lock fall back (default scoping,
  §1.1):** an object carrying `x-amz-meta-*`, object tags, compression metadata,
  or object-lock retention/legal-hold metadata must take the fallback path
  (`fastGetHits` unchanged) and return all bytes/metadata correctly — unless the
  variable-length/full-metadata header upgrade (§1.1) is implemented, in which
  case assert full fidelity on the fast path instead.
- **Over-cap headers fall back (§1.1):** an object whose `Content-Type`/
  `Cache-Control` exceeds the bounded byte budget must take the fallback path
  (`fastGetHits` unchanged) and return the full header — never truncated.
- **Single open:** assert the ON-arm GET issues exactly one `ReadFileStream` per
  participating disk (counter or trace), proving the single-trip invariant
  (§3.1/§4).
- **No RLock leak / early unlock:** after a fast-path GET (both fully consumed and
  early-closed by the client), assert the object's namespace RLock is released
  exactly once on reader close — guards the §3.1 step 8 cleanup. A subsequent
  `Lock()` on the same key must succeed without timeout.
- **Quorum/repair tolerance:** corrupt or delete one disk's `current/part.1` →
  still correct, either via the remaining quorum (hit) or via fallback.
- **Overwrite never serves stale (§2.1):** PUT object A (assert fast hit returns
  A), overwrite the same key with object B, then GET must return **B** (fast hit
  or fallback) — **never A**. Repeat with B larger and smaller than A.
- **Eligible → ineligible overwrite (the asymmetric case, §2.1):** PUT eligible
  object A (assert fast hit), overwrite the same key with a **4 KiB inline**
  object B; GET must return **B via fallback** (zero fast hits), **never A**.
  Assert `current/` was deleted across the set. Repeat cheaply for at least one
  more ineligible kind (multipart or SSE-C) to cover the non-inline ineligible
  transition.
- **Delete never serves stale (§2.1):** PUT object A so a shadow exists, then
  delete the key; GET must 404 / fall back — **never serve the old shadow**.
  Assert `current/` is gone (or no longer forms a quorum) across the set.
- **Old-layout invalidation threshold (§2.1, the heterogeneous-parity case):**
  PUT object A whose shadow uses one erasure layout `oldM/oldN`, then
  overwrite/delete the key under a condition that yields a *different* parity for
  the new object — e.g. a different storage class, a changed max-parity config, or
  an availability-optimized layout (`cmd/erasure-object.go:1298`) such that
  `newN < oldN`. Assert the invalidation deletes to **`oldN+1`** (derived from the
  old `current/` headers), **not** `newN+1`; and that a subsequent GET never
  returns A — this is the **production assertion** and it must pass.
  - *Mutation / fault-injection sub-case (a positive, passing assertion):* via a
    test-only seam, swap in an invalidator that deletes only `newN+1` disks (the
    wrong, too-small count) and **assert that this faulty version leaves stale A
    readable** — i.e. the test positively detects the stale read produced by the
    bug. This proves the threshold is load-bearing (the real test isn't passing by
    luck) while remaining a normal green assertion about the injected fault, not an
    "expect the suite to fail" construct.
  - *Conservative fallback:* when the old headers can't be read on enough disks,
    assert deletion uses `maxPossibleParity+1`.
- **Inline small object (4 KiB):** no `DataDir/part.1` exists, so no shadow is
  written; assert the GET is byte-correct **via fallback** with `fastGetHits`
  *not* incremented. This is expected behavior, not a defect.
- **Out-of-scope → fallback, zero fast hits:** versioned bucket (incl. a
  delete-marker object: assert normal 404 via the `xl.meta` path, `fastGetHits`
  unchanged), multipart, SSE, and non-zero-offset ranges each confirm the
  request takes the fallback path. (Phase 1 implements **no** delete-marker fast
  path; the marker case is purely a fallback assertion.)

---

## 7. Deliverables checklist

| Item | File |
|---|---|
| Flag + eligibility guard | `cmd/singletrip-get.go` |
| Header struct + encode/CRC | `cmd/singletrip-header.go` (+ generated `_gen.go`) |
| Write-side shadow install + replace (temp+`RenameFile`) | `cmd/erasure-object.go` (`putObject`) |
| Shadow invalidation on overwrite/delete (§2.1) | `cmd/erasure-object.go` (`deleteObject`, `DeleteObjects`) |
| Read-side single-trip fast path + decode clone | `cmd/singletrip-get.go` |
| Stream-reuse bitrot reader (`...FromStream`) | `cmd/bitrot-streaming.go` |
| Hit/fallback counters | `cmd/singletrip-get.go`, `cmd/metrics-v3-api.go`, `cmd/metrics-v3.go` |
| Correctness tests | `cmd/singletrip-get_test.go` |

---

## 7.1 Phase 1 implementation task list

Use this checklist as the execution order. Keep each step independently reviewable;
do not mix the measurement harness, write-side shadowing, and read-side stream
reuse in one change.

### A. Scaffolding and header model

- [x] Add startup feature gate `BUCKIT_FAST_GET=1` and process counters
  `fastGetHits` / `fastGetFallbacks`.
- [x] Add `cmd/singletrip-header.go` with fixed-size header encode/decode,
  CRC validation, direct signature computation, bounded response metadata caps,
  and explicit Stage-2 flags (`hasUserMeta`, `hasObjectTags`, compression,
  object-lock, over-cap marker).
- [x] Add header unit tests for valid round-trip, CRC failure, bad magic/version,
  signature/equality grouping, response-metadata caps, and 1-based
  `ErasureIndex` validation.
- [x] Add request/header eligibility helpers without wiring them into the hot path
  yet.

### B. Write-side shadow creation and invalidation

- [x] Implement shadow eligibility using the Phase 1 scope: single-part,
  non-inline, non-versioned, non-transitioned, non-encrypted, non-compressed,
  non-object-lock, no user metadata or tags under default §1.1 scoping, and
  bounded response metadata within cap.
- [x] Implement old-shadow invalidation under the object write lock before
  canonical commit, deriving `oldN` from existing `current/` headers or using the
  conservative `maxPossibleParity+1` threshold when old headers are not reliable.
  The prototype derives old parity from the strongest agreeing `current/` header
  group and requires `oldN+1` delete-or-absence confirmations; if no old header is
  readable, it falls back to the caller's conservative quorum.
- [x] Implement post-commit shadow install through `StorageAPI`: read committed
  `DataDir/part.1`, stream fixed header + shard bytes into temp
  `current.<uuid>/part.1`, then `RenameFile` to `current/part.1`.
- [x] Make failed post-commit shadow install performance-only: log and leave the
  object correct via fallback, but never leave an old readable shadow after a
  newer canonical commit.

### C. Read-side fast path

- [x] Insert `tryFastGet` before `getObjectFileInfo` in `GetObjectNInfo`, behind
  Stage-1 request eligibility.
- [x] Open `current/part.1` once per disk with `length=-1`, read and validate the
  fixed header, then keep the same stream positioned at the shard payload.
- [x] Group headers by direct signature and byte-equal layout/metadata fields;
  require distinct valid 1-based erasure indices and all data indices `1..M`.
- [x] Synthesize `FileInfo`, `ObjectInfo`, and full `M+N` index-ordered
  `onlineDisks`/reader slices; never compact by response order.
- [x] Add stream-reuse bitrot reader variant with cancel-not-drain close semantics.
- [x] Add an end-to-end assertion that fast GET uses one `ReadFileStream` per
  participating disk.
- [x] Mirror current non-inline RLock cleanup semantics: attach `nsUnlocker` to the
  returned `GetObjectReader` cleanup and clear caller defer on successful fast
  stream.

### D. Correctness and measurement

- [x] Add correctness tests from §6, including byte/header equality, fallback
  counters, stale-overwrite/delete cases, eligible-to-ineligible overwrite,
  old-layout invalidation threshold, over-cap metadata fallback, and single-open
  assertions. Byte/ObjectInfo equality, no-shadow fallback counters, single-open
  assertions, offset-0 range reads, stale overwrite/delete,
  eligible-to-ineligible overwrite, and over-cap fallback are covered;
  heterogeneous parity/old-layout threshold is covered by a targeted invalidation
  quorum test. Mid-stream corruption is covered as a documented availability
  limitation: canonical read succeeds, while fast read returns a partial errored
  stream.
- [x] Expose or log `fastGetHits` / `fastGetFallbacks` enough for the §9 playbook.
- [x] Run focused `go test -tags kqueue,dev ./cmd -run SingleTrip` plus any
  touched-package tests.
- [x] Run a local single-process 16-drive smoke validation to confirm shadow
  install, byte correctness, and fast-path counters before attempting the full
  A/B benchmark.
- [x] Fix and run the Docker cluster rig far enough to execute a container
  cold-TTFB A/B pilot on 4 nodes x 4 XFS loopback drives.
- [ ] Run the §9 A/B benchmark playbook only after all correctness tests pass.

Local smoke result (2026-06-04): with `BUCKIT_FAST_GET=1`, a disposable
single-process erasure server using 16 local drive directories accepted an 8 MiB
non-uniform object, installed `current/part.1` on all 16 drives, served a GET
whose bytes matched the uploaded payload, and reported
`minio_api_requests_fast_get_hits_total=1`. This is a mechanism check only; it
does not measure the Phase 1 performance delta because all drives are directories
on the same local filesystem and the run does not compare cold-cache OFF vs ON
arms.

Docker cluster A/B pilot (2026-06-04): after wiring the cluster image to install
the current Linux Buckit binary and start it through systemd, a 4-node x 4-drive
loopback-XFS cluster loaded 64 x 1 MiB objects with `BUCKIT_FAST_GET=1`, then
restarted against the same Docker volumes for OFF and ON arms. For 32 distinct
cold GETs with page cache dropped in all containers before each request:

| Arm | Samples | p50 TTFB | p95 TTFB | Average TTFB |
|---|---:|---:|---:|---:|
| `BUCKIT_FAST_GET=0` | 32 | 5.365 ms | 6.374 ms | 5.430 ms |
| `BUCKIT_FAST_GET=1` | 32 | 5.303 ms | 6.250 ms | 5.388 ms |

The ON arm reported `minio_api_requests_fast_get_hits_total=32`, so the fast
path was exercised. Treat this as a rig/mechanism check only: loopback storage on
Docker Desktop is too fast and too cache-heavy to model HDD seek collapse, and
`warp` was not available in this environment for the saturated throughput
measurement.

---

## 8. Out of scope for Phase 1

Deferred to the production design (`docs/single-trip-get-design.md`) and called
out in the measurement writeup:

- `renameat2(RENAME_EXCHANGE)` directory swap; directory move-not-copy.
- `versions/<versionId>/` for non-current versions.
- Crash recovery, request-time/disk-local repair, fsync ordering.
- Multipart and arbitrary-range fast path.
- Mid-stream degraded-read recovery from parity on the fast path. Phase 1 can
  fall back before streaming if required data shard headers are missing, but once
  streaming starts a later shard error returns a truncated/errored GET instead of
  reopening parity at the failed offset like the canonical `xl.meta` path can.
- Migration of pre-existing objects to direct paths.

Because the shadow copy makes PUT cost unrepresentative, **only GET latency and
throughput numbers are valid** from this prototype.

---

## 9. Evaluation playbook — using the Phase 1 build to measure the difference

This section is the operational recipe: how to drive the prototype and read a
number out of it. The whole evaluation is a clean **A/B on identical on-disk data
and one identical binary**, because `BUCKIT_FAST_GET` is a *runtime* switch — the
only variable that changes between arms is whether GET takes the single-trip path
or the `xl.meta` path.

### 9.1 Why this is a clean A/B

- The shadow `current/part.1` is written at PUT time whenever the flag is on. So
  load the dataset **once with the flag on**; every object then has both
  `xl.meta` + `DataDir/part.1` (canonical) and `current/part.1` (shadow).
- **Baseline arm (OFF):** GET ignores `current/` and reads `xl.meta` — exactly
  today's two-phase path. The unused shadow files do not affect the `xl.meta`
  read cost.
- **Fast arm (ON):** GET reads `current/part.1` headers and skips the `xl.meta`
  fan-out.
- Same data, same binary, same keys, same client — only the read path differs.

### 9.2 Bring up the rig

```sh
cd testing/cluster
./cluster.sh create --fast-get 1    # 4 nodes × 4 drives = 16 drive paths in one EC:4 set
# bigger drives if your dataset needs it (default 1G/drive):
# ./cluster.sh create --fast-get 1 --drive-size 4G
```

The rig's drives are XFS-on-loopback files, **not** independent physical
spindles (see the caveat in the overview). That makes this a sound *relative*
off-vs-on rig but a poor model of HDD seek latency. **For absolute HDD evidence,
run the same Phase 1 binary on a Linux host whose `BUCKIT_ENDPOINTS` point at
mount paths backed by real, separate spinning disks** and repeat §9.4–§9.5
there; the container rig is for mechanism validation and quick relative checks.

Facts the playbook relies on (from `cluster.sh`):

| Thing | Value |
|---|---|
| S3 API endpoints | `http://localhost:9000` (node1), `9002`, `9004`, `9006` |
| Console | odd ports `9001`, `9003`, … |
| Credentials | `buckitadmin` / `buckitadmin` |
| SSH (for `drop_caches`) | `localhost:2201`–`2204`, root password `buckitadmin` |
| Drive mounts inside a node | `/data/drive0` … `/data/drive3` (XFS loopback) |

`cluster.sh create` builds the current repo's Phase 1 binary for Linux, copies it
into the node image, and starts it through systemd. Toggle the arm with
`--fast-get`: use `1` while loading the dataset so shadows are written, then use
`0` for the baseline arm and `1` for the fast arm when regenerating/restarting
the rig. The same source binary is used; only the `BUCKIT_FAST_GET` service
environment changes.

Configure an `mc` alias once:

```sh
mc alias set lab http://localhost:9000 buckitadmin buckitadmin
```

### 9.3 Load the dataset (flag ON, so shadows exist)

Use distinct keys per object so neither arm gets a trivial cache hit. Spread
across size classes; keep total within usable capacity
(`16 × drive_size / 1.33`). Example with `warp`:

```sh
mc mb lab/perf
# one run per size class; --obj.randsize off, fixed sizes:
warp put --host localhost:9000 --access-key buckitadmin --secret-key buckitadmin \
  --bucket perf --obj.size 1MiB --objects 4000 --concurrent 32 --noclear
# repeat with --obj.size 4KiB / 64KiB / 16MiB into separate prefixes
```

(`mc cp`/a small PUT loop works too; `warp` is just convenient.) Sanity-check
that shadows were actually written:

```sh
sshpass -p buckitadmin ssh -p 2201 root@localhost \
  'ls /data/drive0/perf/*/current/part.1 2>/dev/null | head'
```

If that path is empty, the write-side hook didn't fire — fix that before
measuring, or the ON arm will silently fall back.

### 9.4 Measurement 1 — cold first-byte latency (the phase-collapse win)

This is where the design's §6 latency claim lives (≈30–50% on HDD). It is only
visible **cold**, because a warm `xl.meta` makes the first phase free.

Drop the page cache on every node before each cold GET:

```sh
drop_caches() {
  for p in 2201 2202 2203 2204; do
    sshpass -p buckitadmin ssh -p $p root@localhost 'sync; echo 3 > /proc/sys/vm/drop_caches'
  done
}
```

Measure time-to-first-byte for a single stream over a sweep of *distinct* cold
objects:

```sh
# presign once, then drop caches and GET, capturing TTFB:
url=$(mc share download --json lab/perf/<key> | jq -r .share)
drop_caches
curl -s -o /dev/null -w 'ttfb=%{time_starttransfer}s total=%{time_total}s\n' "$url"
```

Loop over ~100 distinct objects per size class, dropping caches before each (or
drop once, then GET a batch of never-touched keys). Record **median and p95
TTFB** per size class, for both arms. The delta is the latency result.

### 9.5 Measurement 2 — saturated throughput (the per-disk seek win)

This is the §6 throughput claim (≈1.5–1.75× for seek-bound small/medium GETs).
Drive many concurrent GETs over a working set larger than the VM's RAM so the
drives actually seek and the metadata cache is pressured (most meaningful on the
real-disk host; on the container rig read it as a relative number):

```sh
warp get --host localhost:9000 --access-key buckitadmin --secret-key buckitadmin \
  --bucket perf --obj.size 64KiB --objects 4000 --concurrent 64 --duration 2m --noclear
```

Record aggregate **GET req/s** and **p50/p99 latency**, both arms. Stay in the
seek-bound size regime (64 KiB–1 MiB); at 16 MiB transfer dominates and the
expected win shrinks to ~0–10%.

### 9.6 Measurement 3 — warm-cache control (the null check)

Repeatedly GET a small hot set **without** dropping caches. Both arms should land
within noise of each other, because `xl.meta` is already cached so the baseline
pays no extra seek. **If the ON arm shows a large win here, the harness is
measuring something other than the metadata seek — stop and investigate before
trusting Measurements 1–2.**

### 9.7 Confirm the fast path actually fired

After each ON-arm run, read the `fast_get_hits_total` /
`fast_get_fallbacks_total` metrics from `/api/requests` (§3). A meaningful ON
result requires hits ≫ fallbacks. A near-zero hit count means requests fell out
of eligibility (wrong size/range/versioning) or the shadow was missing — the
measured "no difference" would be an artifact, not a verdict on the design.

### 9.8 Optional single-spindle IOPS pre-check

The only honest single-host experiment, and it covers **throughput only, not
latency**: saturate one loopback XFS device with GET-shaped I/O and count ops/sec
for a 2-seek-per-op access pattern (open+read meta, then open+read data) versus a
1-seek-per-op pattern (open+read the prefixed shard). This models §6's per-disk
seek argument directly, on a single disk, without the full server. A small
`fio` job-pair or a ~50-line Go harness suffices. Do **not** use it to reason
about latency — that needs the parallel cluster.

### 9.9 Pitfalls and how to read the result

- **Silent fallback** — always check §9.7 counters; a null result with zero hits
  is meaningless.
- **Cancel-close churn** — the fast path deliberately closes EOF streams without
  draining so it does not pull whole unused shards from remote disks. That may
  reduce connection reuse and add setup cost to subsequent requests, so ON-arm
  latency numbers are conservative with respect to connection reuse.
- **Not actually cold** — if the working set fits in the Docker VM's RAM, page
  cache hides the seek and Measurement 1 flatlines. Make the set larger than RAM
  and always `drop_caches`.
- **Docker on macOS is not bare-metal HDD** — Docker Desktop runs a LinuxKit VM
  and loopback-XFS-on-overlay has its own caching; treat the numbers as **ratios
  between arms, not absolute HDD figures**. For absolute numbers, run the rig on
  a Linux host with real spinning disks.
- **Inline small objects** — objects below the inline cutoff already return from
  the metadata read (design §1.2), so 4 KiB GETs should show ≈0 gain in both
  measurements; that is expected, not a failure.
- **Hold everything else fixed** — identical key set, concurrency, and request
  order across arms; run each cell ≥3× and report median + spread.

### 9.10 Decision criteria

The prototype supports the design's §6 claims if, on the parallel cluster:

- **Latency:** cold-cache median TTFB on ≥1 MiB objects drops by roughly **≥25%**
  in the ON arm, while the warm-cache control (§9.6) stays within noise; **and**
- **Throughput:** saturated GET req/s on 64 KiB–1 MiB objects improves by roughly
  **≥1.4×** in the ON arm, with fast-path hits ≫ fallbacks.

If the warm control shows the same gain as cold, or hits ≈ 0, the result is a
measurement artifact and must be fixed before drawing a conclusion. Report all
numbers against the §6 predictions with the caveats above (shadow-copy write cost
excluded; single-part only; container loopback ≠ bare-metal HDD).
