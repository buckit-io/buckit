# FastGet Full-Feature Design Gaps

**Status:** design backlog after Phase 1 prototype

**Inputs:**
- `docs/single-trip-get-design.md` - intended production direction.
- `docs/single-trip-get-phase1-implementation.md` - measurement prototype plan.
- Current `feature/single-trip-get-phase1` prototype - additive `current/part.1`
  shadow, fixed header, EAGERSTABLE default, stable shard selection, parity
  reconstruction, and prototype metrics.

This document lists the remaining design work needed to turn FastGet from a
measurement prototype into a full system feature. It is not an implementation
plan yet. Each section should produce a concrete design decision, invariants,
failure handling, and tests before code lands.

---

## 1. Current Prototype Boundary

The current branch proves the main latency mechanism: a latest GET can skip the
`xl.meta` fan-out and reconstruct `FileInfo` from a checked header next to shard
bytes.

Important prototype properties:

- `current/part.1` is an additive shadow copy, not the only physical data path.
- FastGet is gated by `BUCKIT_FAST_GET`.
- Default candidate is EAGERSTABLE: eager first-block prefetch is on, stable
  spread selection is on, hedging is off.
- Only latest, non-versioned, single-part, non-inline, local, unencrypted,
  uncompressed, non-object-lock objects are eligible.
- FastGet is true no-`xl.meta` only on the `SinglePool()` path. Multi-pool GET
  still resolves the owner through metadata first.
- The fixed 1024-byte header carries only a bounded response-metadata subset.
- There is no request-time repair, scanner reconciliation, fsync protocol, or
  production migration path.

Design implication: the prototype is good evidence for the GET-side mechanism,
but it is not production-safe and does not define the final on-disk contract.

---

## 2. Product Scope and Compatibility

### Gaps

- Decide which GET API surfaces FastGet must cover beyond Phase 1. FastGet is
  GET-only for all planned phases; HEAD stays on the existing metadata-only
  `GetObjectInfo` path and is out of FastGet scope.
- Decide whether FastGet is a supported persistent feature or still an
  experimental server flag.
- Define compatibility behavior for old objects without `current` aliases or
  embedded direct headers, old header versions, mixed-version clusters, and
  downgrade.
- Define user-visible behavior: FastGet must not change response bytes, response
  headers, errors, version semantics, lifecycle behavior, replication behavior, or
  object-lock semantics.

### Design tasks

- Define the supported request matrix and fallback matrix.
- Specify config shape: environment-only, config subsystem, dynamic cluster-wide
  toggle, or per-bucket policy.
- Define upgrade/downgrade rules and minimum mixed-cluster safety constraints.
- Decide whether FastGet can be enabled by default, and under what storage/media
  conditions.
- Define production observability: hits, fallbacks, fallback reasons, repair
  queue stats, stale-`current` repair stats, and latency counters without noisy
  per-request profiling.

---

## 3. Direct-Path Layout

### Gaps

The production design wants no duplicate latest data:

```
bucket/object/current/part.N
bucket/object/<DataDir>/part.N
bucket/object/xl.meta
```

The prototype instead writes:

```
bucket/object/<DataDir>/part.1
bucket/object/current/part.1
bucket/object/xl.meta
```

That doubles write cost and storage, makes PUT numbers invalid, and leaves the
system without a design for old-version direct GETs.

### Discussed direction

Keep the existing Buckit `xl.meta` plus opaque `DataDir` layout as the durable
layout. Add `current` only as a latest-version GET acceleration alias:

```
bucket/object/xl.meta
bucket/object/<DataDir>/part.N
bucket/object/current/part.N
```

The important invariant is that `xl.meta` remains the source-of-truth metadata and
version index, and each version's shard data remains under the `DataDir`
recorded in `xl.meta`. `current` is only an acceleration path for latest-version
GET. Explicit old-version GET, such as `GET ?versionId=<id>`, should use
the coalesced `xl.meta` route. That is slightly slower than a
direct version path, but old-version GET is not the common hot path and does not
justify a new `versions/<versionId>` directory layout in the initial full
feature.

Two `current` materializations should be designed and benchmarked behind the
same FastGet read path:

1. **Reserved real DataDir:** `current/part.N` contains the latest version's
   shard files directly. In this mode, `current` is a special `DataDir` name for
   the latest version. This avoids symlink semantics but makes PUT/overwrite
   harder because the previous latest `current` directory must be renamed to a
   generated `DataDir` and recorded in `xl.meta` without exposing mixed crash
   states.
2. **Object-level pointer:** `current` points to the latest generated `DataDir`,
   for example as a directory symlink on POSIX filesystems. This makes PUT
   simpler: write a new generated `DataDir`, update `xl.meta`, then atomically
   replace the single `current` pointer.

Avoid per-part symlinks such as:

```
bucket/object/current/part.1 -> ../<DataDir>/part.1
```

A per-part pointer can expose mixed state when an object has multiple S3 parts,
header sidecars, checksums, or future layout files. If indirection is used, it
should be a single object-level pointer:

```
bucket/object/current -> <DataDir>
```

The symlink/pointer mode is acceptable only if it is scoped as a rebuildable
GET acceleration index:

- PUT, DELETE, scanner, heal, lifecycle, replication, GC, and version listing
  must continue to use `xl.meta` and recorded `DataDir` paths.
- FastGet may open `current/part.N`, but it must validate the direct header and
  fall back to the coalesced `xl.meta` FastOpen tier if the pointer is missing,
  stale, invalid, or points at a header that does not represent the latest
  committed version.
- Delete-marker latest state must remove or invalidate `current`, or otherwise
  make FastGet fall back. It must never serve the previous data version as
  latest after a committed delete marker.
- Pointer targets must be relative, object-local, and unable to escape the
  object directory.
- The implementation should hide the choice behind a storage-layer publish
  operation, not expose generic symlink creation to unrelated code.

This keeps the FastGet reader stable (`current/part.N`) while allowing a
controlled comparison between real-directory and object-level-pointer publish
protocols.

### Design tasks

- Decide final directory names for:
  - latest object data;
  - generated `DataDir` directories;
  - null versions and non-versioned objects;
  - delete-marker current state;
  - staging directories;
  - quarantine/orphan directories.
- Decide whether production `current` is a real directory, an object-level
  pointer, or a runtime/feature-gated choice for benchmarking.
- If `current` is a pointer, define the storage API operation for atomic pointer
  replacement and the fallback behavior on platforms where symlink creation is
  unavailable or unreliable.
- Define whether generated `DataDir` names stay unchanged or whether `current`
  as a reserved real `DataDir` requires additional reserved-name handling.
- Define how `current` aliases and embedded direct headers coexist with existing
  opaque `DataDir` objects during migration.
- Define scanner behavior for `current` aliases, old `DataDir` paths, orphan
  staging paths, and partial migrations.

---

## 4. Header Format and Metadata Fidelity

### Gaps

The prototype header is fixed-size and intentionally scoped down. A full feature
needs a durable schema and enough metadata to make FastGet byte/header-equivalent
to existing GET behavior.

Metadata still needing design:

- user metadata: `x-amz-meta-*`;
- object tags: `x-amz-tagging`, tag count, optional tag response;
- object-lock metadata: retention and legal hold, permission-filtered on GET;
- checksum metadata for `x-amz-checksum-mode: ENABLED`;
- server-side encryption metadata for SSE-S3 and SSE-KMS, including enough
  sealed-key/KMS metadata for the landing node to run the existing decryption
  path after erasure decode;
- compression metadata and decompression behavior, with range-specific
  compressed-offset planning deferred until range FastGet is in scope;
- transition/restore metadata and remote-tier behavior;
- replication status and version purge status;
- multipart part metadata and per-part checksums;
- non-STANDARD storage class and lifecycle prediction headers.

### Discussed direction

Use a two-tier storage-node open protocol:

1. **Embedded direct header:** the storage node opens `current/part.1`, reads the
   embedded direct header, validates it, and streams the shard body after the
   header. This is the highest-performance FastGet path because it avoids
   request-time `xl.meta` reads.
2. **Coalesced `xl.meta` open:** if the direct header is missing, invalid,
   stale, or cannot represent the object metadata, the storage node reads
   `xl.meta`, selects the version/data directory, and returns a
   metadata frame followed by the shard stream in the same storage response.

The landing node should see both tiers through one framed `FastOpenPart`
response stream. The first frame identifies the tier and carries either direct
header data or coalesced `xl.meta` metadata; shard bytes follow in the same
stream:

```
FastOpenPart(bucket, object, version selector, part.N)
  -> DirectHeaderFrame, then direct shard bytes
  -> CoalescedMetadataFrame, then shard bytes
  -> miss/error
```

This means the embedded header does not need to encode every object case in the
first full version. It can stay optimized for the high-value direct path, while
the coalesced `xl.meta` open preserves full metadata fidelity and keeps one
storage RPC for broader object coverage.

The embedded direct header should be a variable-length frame, not a fixed
reserved slot. A fixed 4 KiB or 16 KiB header area would permanently add padding
to every eligible object, which is especially wasteful for small objects. The
preferred shape is:

```
part.1 = fixed small prelude + variable metadata frame + shard body
```

The prelude must be just large enough to identify the format and locate the
frame, for example magic, format version, header length, checksum, and flags.
After decoding the prelude and frame, the reader computes:

```
bodyOffset = preludeLen + headerFrameLen
```

EAGERSTABLE still works with this model, but it cannot assume the prototype's
fixed 1024-byte body offset. Eager prefetch, bounded local body reads, and
streaming bitrot readers must use the decoded `bodyOffset`.

For multipart objects, the embedded direct header is anchored in `part.1` only.
Other S3 part files should remain ordinary shard files unless a future design
adds a direct part index. Requests that need to enter at `part.2` or later, such
as `GET ?partNumber=2` or ranges that start beyond `part.1`, should use the
coalesced `xl.meta` tier of `FastOpenPart`.

`xl.meta` remains the fallback source of truth, but reading it on the storage
node and appending the shard stream is different from the landing node doing a
separate metadata phase and then a separate shard phase. The coalesced path
optimizes transport shape; the embedded header path optimizes both transport and
disk access shape.

### Design tasks

- Replace the fixed positional 1024-byte prototype header with a small fixed
  prelude plus variable-length evolvable frame: length-prefixed, versioned,
  checksummed, and self-describing.
- Define the decoded header body offset and update Eager/read protocols to use
  that offset instead of a constant header length.
- Decide whether the header contains full metadata or a scoped subset plus
  fallback flags.
- Define the `CoalescedMetadataFrame` returned by storage-node coalesced open
  and prove it can reconstruct existing GET response metadata and `ObjectInfo`.
  For Phase 1, this frame must include SSE-S3/SSE-KMS metadata instead of
  treating encrypted objects as ineligible, because discovering that encryption
  state already required the storage node to read `xl.meta`; doing another
  landing-node `xl.meta` read would defeat the coalesced-open goal.
  It must also include compression metadata, actual uncompressed size, and any
  single-part compression index data needed for the landing node to reconstruct
  `ObjectInfo` and run the existing decompression wrapper after erasure decode.
- Define how the landing node merges direct-header results and coalesced
  `xl.meta` results when different disks return different tiers for the same
  request.
- Define a stable signature over all fields that affect decode, version
  identity, response headers, and fallback eligibility.
- Specify exact behavior for over-cap metadata.
- Define how metadata-only mutations update or invalidate `current` and embedded
  direct headers:
  `PutObjectTags`, `DeleteObjectTags`, `PutObjectMetadata`, legal hold,
  retention, replication metadata, restore metadata, and lifecycle metadata.
- Define tests proving FastGet response headers match existing GET behavior for
  all supported metadata.

---

## 5. Write, Delete, and Crash Protocol

### Gaps

The prototype uses invalidate-before-commit and post-commit shadow install. That
is safe enough for measurement, but production needs a durable `current` publish
protocol with crash recovery and no duplicate latest shard. The existing write
path should still commit `xl.meta` plus the version's `DataDir`; `current` is a
derived latest GET index.

Open design decisions:

- whether production `current` is a reserved real `DataDir`, an object-level
  pointer to a generated `DataDir`, or a benchmark-gated choice.
- atomic publish protocol for `current`:
  - real-directory mode may need a safe rename from `current` to a generated
    old-version `DataDir` before publishing the new latest;
  - pointer mode needs atomic pointer replacement after the new `DataDir` and
    `xl.meta` are durable.
- fsync ordering for shard files, `DataDir` directories, `xl.meta`, `current`,
  and staging paths.
- how to publish or invalidate `current` when the latest version is a delete
  marker, without serving older data.
- whether failed `current` publish aborts writes or commits `xl.meta` metadata
  and lets GET fall back to the coalesced `xl.meta` tier of `FastOpenPart`
  until repair rebuilds `current`.
- how to handle platforms where symlink/pointer replacement is unavailable or
  unreliable.

Current delete-marker representation:

- Delete markers are metadata-only `xl.meta` journal entries, not shard
  directories.
- The xl.meta entry has `Type: DeleteType` and an `xlMetaV2DeleteMarker`
  containing `VersionID`, `ModTime`, and `MetaSys`.
- When converted to `FileInfo`, a delete marker sets `Deleted: true`, carries the
  delete marker `VersionID` and `ModTime`, and exposes `MetaSys` as metadata.
- `MetaSys` is primarily used for internal state such as replication,
  version-purge, and tier/free-version metadata.

Because a delete marker has no `DataDir` and no `part.1`, the initial FastGet
rule should be to remove or invalidate `current` when the latest version is a
delete marker. Latest GET then uses the coalesced `xl.meta` tier of
`FastOpenPart`, reads `xl.meta` on the storage node, sees `Deleted: true`, and
returns the existing delete-marker response.

Metadata-only header replacement:

`PutObjectMetadata`, `PutObjectTags`, legal hold, retention, replication
metadata, restore metadata, and lifecycle metadata can change fields that the
embedded direct header would otherwise cache. For latest-version metadata
mutations, treat the operation like a `PutObject` commit that reuses the same
version, `DataDir`, and shard body. Do not rely on loose async repair that only
recreates `current`; that would republish a `part.1` whose embedded header still
contains old metadata.

1. Lock the object.
2. Read `xl.meta` and pick the target `FileInfo`.
3. Apply the metadata mutation to produce the updated `FileInfo`.
4. Temporarily invalidate `current` so FastGet cannot serve either the old header
   or a mixed header/metadata state during the commit.
5. If the updated latest version is no longer eligible for embedded-header
   FastGet, commit updated `xl.meta` and leave `current` invalidated.
6. If still eligible, rewrite the embedded `part.1` header synchronously using a
   temporary file and the same shard body.
7. Commit updated `xl.meta` with write quorum.
8. Republish `current` only after both the embedded header and `xl.meta` reflect
   the same updated `FileInfo`.

If the embedded header is stored in the source-of-truth `part.1`, do not patch it in
place. Use a staged rewrite:

```
<DataDir>/part.1
<DataDir>/part.1.fastget.tmp
```

The synchronous refresh decodes the old header to find `oldBodyOffset`, writes
the new variable header to the temporary file, stream-copies the existing shard
body from `oldBodyOffset`, fsyncs the temporary file, atomically renames it over
`part.1`, and fsyncs the parent directory.

If the header refresh fails, the safest initial behavior is to fail the
metadata-only operation before publishing `current`. A degraded mode that commits
`xl.meta` metadata and leaves `current` invalidated can be designed later, but it
must never republish `current` until the embedded header matches `xl.meta`.

This keeps correctness simple but makes metadata-only direct-header refresh a
file rewrite. A separate derived file would make refresh cheaper but would
reintroduce data duplication, so it is not the initial direction.

### Design tasks

- Specify the write-state machine for:
  - first PUT of a key;
  - overwrite in non-versioned bucket;
  - overwrite in versioned bucket;
  - overwrite from eligible to ineligible;
  - multipart complete;
  - copy object;
  - delete marker creation;
  - permanent delete by version ID.
- Specify crash states and deterministic recovery actions for every state.
- Define stale-read prevention invariants. `xl.meta` remains authoritative, but the
  `current` alias must never serve an older committed version as latest.
- Define delete-marker `current` invalidation and repair behavior.
- Define `current`/header publish rules: eligible writes publish a fresh header
  generated from the committed `FileInfo`; ineligible writes and metadata-only
  mutations invalidate or rebuild `current`; scanner repairs stale `current`
  aliases only after verifying that the existing embedded `part.1` header
  matches `xl.meta`.
- Define synchronous metadata-only header refresh and its failure behavior.
- Define a FastGet repair queue or scanner hook for stale/missing `current`
  discovered outside the locked metadata mutation path.
- Add design-level proof that FastGet's matching-header read quorum is safe
  under offline-disk rejoin. The proof should rely on normal erasure quorum
  intersection: once a newer version is committed to write quorum, stale
  `current` aliases from disks that missed the write cannot form a disjoint read
  quorum for the older version. Mixed old/new headers must not be combined, and
  scanner/repair only shortens the stale-`current` window; it should not be
  required for correctness when stale aliases are below read quorum.

---

## 6. Read Protocol

### Gaps

The prototype evolved from "open all to EOF once" into EAGERSTABLE: open exactly
the quorum, use stable spread selection, prefetch local first blocks, and use
bounded body reads for local multi-block eager. This is better for the measured
candidate. The production read protocol should keep the tight initial open set
and add deterministic before-first-byte replacement plus explicit cancellation.

Recommended read/open order:

1. Select only disks where `disk != nil && disk.IsOnline()`, matching the
   existing GET metadata read path.
2. Open the maximum configured read quorum for the erasure set, with no initial
   hedge. This should cover the largest possible data-block count across
   configured storage classes and write modes. For example, in a 16-drive set,
   if the lowest configured parity is 2, the initial open count is 14. If data
   and parity can be equal, apply the existing strict-majority rule.
3. Give every `FastOpenPart` call its own cancellable child context.
4. Ask each selected disk for `FastOpenPart`.
5. For latest GET that can enter through `part.1`, the disk first tries the
   embedded direct path: `current/part.1`.
6. If direct open fails before streaming, the disk may fall back locally to the
   coalesced `xl.meta` tier: read `xl.meta`, select the version and
   data directory, then return a metadata frame followed by the shard stream.
7. The landing node applies the existing Buckit metadata quorum, erasure
   distribution ordering, and decode semantics. FastGet must not invent a new
   metadata matching or shard scoring policy.
8. If the initial selected set fails before the first response byte, open all
   remaining online disks as replacements, then apply the same existing Buckit
   quorum/decode semantics over the combined responses.
9. After a winning read set is selected, close and cancel every unused stream
   immediately. Do not drain unused remote streams.
10. If the client disconnects, or FastOpen exits before response bytes are sent,
   cancel all outstanding child contexts so remote storage reads stop promptly.
11. After the first response byte is committed, do not switch to a semantic
   fallback path. Surface errors through the existing read/decode behavior and
   cancel all remaining streams on exit.

Explicit `versionId` GET and delete-marker responses should use the coalesced
`xl.meta` `FastOpenPart` tier starting in Phase 1. Requests that enter at
non-`part.1` multipart data should not require `current` or a direct embedded
header; they can stay on the current non-FastGet implementation until multipart
FastGet scope is added.

The embedded direct tier is the performance ceiling. The coalesced `xl.meta` tier
is the compatibility and fidelity tier. Both should use the same shard selection,
decode, cancellation, and before-first-byte fallback framework where possible.

### Design tasks

- Factor the existing Buckit metadata quorum and erasure decode selection so
  `FastOpenPart` responses feed the same logic as current GET.
- Define the framed `FastOpenPart` response shape and body modes.
- Define tests for initial selected-set failure, replacement opens, unused-stream
  cancellation, client disconnect, and post-first-byte stream errors.

---

## 7. Versioning

### Gaps

The prototype disables FastGet for versioned and version-suspended buckets. The
initial full-feature design should support versioned single-part GET through the
coalesced `xl.meta` route, including latest-version GET, explicit old-version
GET, and delete-marker responses.

Design constraints:

- Metadata is version-scoped in `xl.meta`.
- Tags and object-lock state can be updated for an old version.
- A plain GET without `versionId` must see only the latest version.
- A latest delete marker must return the same error and headers as existing GET
  behavior.
- `GET ?versionId=<old>` must not read metadata from a newer version.
- Only mutations that change the latest visible version should publish, refresh,
  or invalidate `current`.
- Mutations to non-latest versions should update `xl.meta` and leave
  `current` untouched.

### Design tasks

- Define latest-version direct lookup through `current`.
- Define explicit `versionId` GET through the coalesced `xl.meta` tier of
  `FastOpenPart`.
- Define old-version metadata mutation behavior: update `xl.meta` for that
  `VersionID`, do not refresh `part.1`, and do not touch `current`.
- Define how a new version publishes or invalidates `current` without changing
  the old-version `DataDir` contract.
- Define how permanent delete removes or quarantines recorded `DataDir`
  directories. If permanent delete changes the latest visible version, the
  simplest initial rule is to invalidate `current`; scanner/repair may rebuild
  it later after verifying the exposed latest version and header.
- Define versioned bucket tests covering tags, user metadata, object lock,
  delete markers, null versions, and permanent delete.

---

## 8. Multipart and Range

### Discussed direction

Embed the direct header only in `part.1`:

```
current/part.1   # direct header + shard body
current/part.2   # shard body only
current/part.3   # shard body only
```

This avoids duplicating object metadata into every S3 part and limits
metadata-only mutation work. Full-object multipart GET can use the `part.1`
header as the direct metadata anchor, then stream remaining parts from the same
latest `current` alias or recorded `DataDir`.

Requests whose first required shard is not `part.1` should use coalesced
`xl.meta` open unless a later design adds a direct multipart index. That includes
`GET ?partNumber=N` for `N > 1` and ranges that start in a later S3 part.

The initial Phase 1 implementation does not need to support multipart objects or
range GET. Multipart GET, `partNumber`, and ranges can stay on the existing
non-FastGet implementation until the single-part `FastOpenPart` protocol is
proven.

When multipart support is added, keep existing landing-node range planning. The
landing node already converts `Range` and `partNumber` into logical offsets,
maps those offsets to physical `part.N` spans, and decodes each physical part.
The multipart extension should replace each per-part remote shard open with one
`FastOpenPart` call for that physical part span. A range crossing multiple
multipart parts may issue multiple `FastOpenPart` calls. That is acceptable
because multipart parts are large, so the extra open cost is negligible compared
with transfer and decode time, and it keeps the remote storage node from needing
a cross-part object-range planner.

### Design tasks

- Define what the `part.1` direct header must carry for full-object multipart
  GET without reading `xl.meta`.
- Define the `FastOpenPart` per-part call contract for logical ranges that map
  to multiple physical `part.N` spans.
- Define which Phase 2 direct-header multipart/range requests must use
  coalesced `xl.meta` open because they do not enter through `part.1`.
- Decide whether a future direct multipart index is needed for non-`part.1`
  entrypoints.
- Define behavior for checksum-mode on full-object vs. range reads.
- Define tests for ranges crossing part boundaries and for `partNumber` GET.

---

## 9. Pool Routing and Cluster Topology

### Gaps

Current FastGet only avoids `xl.meta` in the single-pool path. Multi-pool
deployments still call pool-resolution code that reads metadata before the set
FastGet path can run.

### Discussed direction

Keep the current metadata-based pool selection for correctness, but reduce the
initial metadata fanout per pool with a quorum-intersection probe.

For each pool's hashed set, probe:

```
safeProbeCount = N - writeQuorum + 1
```

where `N` is the set drive count and `writeQuorum` is the minimum write quorum
for object layouts supported in that set. This count intersects any committed
version's write quorum. For example, in a 16-drive set with write quorum 12,
at most 4 drives can be stale/missing for a committed version, so probing 5
drives is enough to see that committed version if it exists.

Pool selection flow:

1. Probe `safeProbeCount` stable-spread drives in each pool's hashed set.
2. If exactly one pool reports a candidate and every other pool is proven absent
   by the probe, select that pool.
3. If multiple pools report candidates, run full `xl.meta` pool arbitration.
4. If any pool returns ambiguous results, errors, or insufficient proof of
   absence, expand that pool or run full `xl.meta` pool arbitration.
5. After selecting a pool, run `FastOpenPart` in that pool.

This reduces the common single-owner case without requiring cross-pool
`FastOpenPart` arbitration. It does not change GET availability: a 16-drive set
that has lost 6 drives can only serve objects whose actual read quorum can be
satisfied by the remaining 10 drives.

### Design tasks

- Define safe-probe pool selection and the expansion rules for ambiguity,
  multiple candidates, delete markers, versioned requests, and pool errors.
- Decide how to handle objects that may exist in multiple pools during rebalance
  or pool expansion.
- Define interaction with decommission, rebalance, site replication, and
  data-movement code.
- Add multi-pool correctness and performance tests.

---

## 10. Storage API and Transport

### Gaps

The prototype reuses `ReadFileStream`, `CreateFile`, `RenameFile`, and `Delete`.
Production likely needs `FastOpenPart` for reads and must extend existing
commit-style storage mutations so they update variable headers and `current`
atomically inside the storage node.

### Design tasks

- Decide whether to add storage APIs for:
  - `FastOpenPart` that can return either an embedded direct header plus shard
    stream or a coalesced metadata frame plus shard stream;
  - extensions to existing mutation APIs such as `RenameData`, `UpdateMetadata`,
    and delete-version paths so header refresh and `current` publish/invalidate
    happen atomically inside each storage node;
  - `WritePrefixedShard` or equivalent local helper to write variable-header
    `part.1` without cross-node shadow copy;
  - direct-path header read plus paused body stream, if this is not folded into
    `FastOpenPart`;
  - `current` reconcile/repair;
  - delete/invalidate `current` for latest delete markers, as part of the delete
    storage mutation.
- Define REST/grid wire behavior and cancellation semantics.
- Define how local `xlStorage` and remote `storageRESTClient` implementations
  differ, if at all.
- Define capability detection for symlink/pointer support, variable direct
  headers, and mixed nodes.

---

## 11. Repair, Scanner, and Migration

### Gaps

The prototype has no scanner or repair path for `current`. In the full design,
objects without a valid `current` alias or embedded direct header can still use
the coalesced `xl.meta` tier of `FastOpenPart`, but stale `current` must not
serve old latest data.

Scanner/request-time repair should only reconcile the `current` alias. It should
not create or rewrite embedded `part.1` headers. Header creation and refresh are
owned by write-side mutation paths such as PUT, multipart complete, copy, and
latest-version metadata updates. If scanner finds a latest `DataDir` whose
`part.1` header is missing, stale, or invalid, it must leave `current` absent and
let GET use the coalesced `xl.meta` tier.

### Design tasks

- Define request-time repair triggers when FastGet falls back.
- Define disk-local reconciliation of `current` against `xl.meta` and the
  latest version's `DataDir`.
- Define scanner migration for existing objects as optional installation or
  repair of `current` only when the latest `DataDir` already has a valid
  embedded `part.1` header matching `xl.meta`.
- Define scanner cleanup for stale `current`, staging directories, orphan
  `DataDir`s, and uncommitted direct data.
- Define repair priority and throttling so FastGet repair does not starve normal
  healing.
- Define metrics for direct-path health: present, missing, stale, repaired,
  quarantined, and migrated.

---

## 12. Security and Correctness

### Gaps

The embedded direct tier reconstructs `ObjectInfo` without `xl.meta`; the
coalesced `xl.meta` tier reconstructs it from `xl.meta` on the storage node. The
full design must prove that neither tier can bypass checks performed by the
existing metadata path.

### Design tasks

- Audit every GET decision that depends on `ObjectInfo` or `FileInfo`.
- Ensure FastGet preserves:
  - IAM and bucket policy behavior;
  - SSE-C/SSE-S3/SSE-KMS behavior;
  - object lock retention/legal hold filtering;
  - lifecycle expiry and transition behavior;
  - replication proxy behavior;
  - conditional request behavior;
  - checksum response behavior;
  - delete-marker errors and headers.
- Define what must be in the direct header vs. what forces fallback.
- Define fuzz/corruption tests for malformed headers, signature collisions,
  mismatched metadata, duplicated erasure indexes, and path/header disagreement.

---

## 13. Test and Validation Plan

### Required design outputs

- A correctness matrix covering request type, bucket versioning state, object
  type, metadata type, and expected FastGet/fallback behavior.
- A crash-state matrix covering every write/delete stage and recovery result.
- A migration matrix covering old objects, mixed object layouts, and mixed
  software versions. Mixed object layouts here means objects with and without
  `current` aliases or variable direct headers, not a new dedicated version
  directory layout.
- A performance matrix covering HDD, SSD/NVMe, local single-pool, distributed
  single-pool, and multi-pool.

### Required test groups

- Header encode/decode compatibility and corruption tests.
- End-to-end GET byte and header equality tests.
- Versioned object tests for tags, metadata, delete markers, and old-version GET.
- Multipart/range tests for the later multipart FastGet extension.
- Metadata mutation tests.
- Crash/fault-injection tests around `DataDir` commit, `current` publish, pointer
  replacement, invalidation, and fsync points.
- Offline-disk rejoin and scanner repair tests.
- Multi-pool routing tests.
- HDD load tests that compare FastGet with existing GET under cold and warm
  cache conditions.

---

## 14. Suggested Design Sequence

### Phase 1: FastOpenPart with coalesced xl.meta frames

Goal: improve the transport shape without changing the on-disk object layout.

- Add `FastOpenPart` as a framed response stream:
  `CoalescedMetadataFrame`, then body bytes when the selected object has a
  FastOpen-streamable local body.
- On the storage node, read `xl.meta`, select the requested single-part object
  version or latest visible version, then return the selected version metadata
  before any body bytes. For local shard-backed objects, open the selected
  version's `DataDir` shard and stream it after the frame. For metadata-only or
  non-shard cases, the frame identifies the case and the body stream is omitted
  or handled by the landing node through the existing path.
- Support all non-range, non-multipart GET cases through this tier in Phase 1,
  including latest GET, explicit `versionId` GET, versioned/suspended buckets,
  null versions, and delete-marker responses.
- Support inline objects in Phase 1. Since the storage node already reads
  `xl.meta`, inline data can be returned through the coalesced frame/body path
  without a local shard open.
- Support object-lock metadata in Phase 1. The frame must carry retention and
  legal-hold metadata, and the landing node must run the existing permission
  filtering before setting response headers.
- Support `x-amz-checksum-mode: ENABLED` for non-range Phase 1 GET. The frame
  must carry checksum metadata so the landing node can run the existing checksum
  response-header logic. Range checksum behavior remains deferred with range
  FastGet.
- Support non-STANDARD storage classes in Phase 1 when the selected object is
  still local or restored locally. The frame must carry the actual storage class
  and erasure layout; the landing node must use the returned `ErasureM` instead
  of assuming the default class.
- Support SSE-S3 and SSE-KMS for single-part GET in Phase 1. The remote storage
  node streams encrypted shard bytes; the landing node reconstructs `ObjectInfo`
  from `CoalescedMetadataFrame`, runs existing encryption validation/response
  header logic, erasure-decodes, then decrypts with the existing landing-node
  stream path.
- Support compressed single-part full-object GET in Phase 1. The remote storage
  node streams stored compressed shard bytes; the landing node reconstructs
  `ObjectInfo` from `CoalescedMetadataFrame`, erasure-decodes, then uses the
  existing decompression path. Compressed range and compressed multipart GET stay
  out of scope until range/multipart FastGet is added.
- Support restore/transition metadata in Phase 1. Restored-on-disk objects can
  use the local shard/inline path while preserving `x-amz-restore` headers.
  Remote-tier objects that are not restored locally should return a coalesced
  metadata frame without a shard stream so the landing node can use the existing
  transitioned-object reader; FastOpen must not try to open a missing local
  `DataDir` shard for them.
- Keep SSE-C out of Phase 1. It is request-header/customer-key sensitive and can
  be rejected before FastOpen from request headers.
- Use maximum-configured-read-quorum initial drive selection over online disks,
  with no initial hedge. If the selected set fails before the first response
  byte, open all remaining online disks as replacements and then apply existing
  Buckit quorum/decode semantics.
- Give every `FastOpenPart` stream its own cancellable child context. Cancel
  unused streams immediately after selecting the winning read set, and propagate
  client disconnect/fallback cancellation to every outstanding remote read.
- Keep current multi-pool metadata selection, with the safe-probe optimization as
  a separate pool-selection improvement.

Phase 1 should establish the framed stream protocol, cancellation semantics,
metadata fidelity against existing GET behavior, selected-drive fallback, and
parity reconstruction without introducing `current` or embedded direct headers.

### Phase 2: current symlink plus embedded part.1 header

Goal: add true no-`xl.meta` latest GET for eligible objects while preserving
the Phase 1 coalesced `xl.meta` tier as fallback.

- Keep `xl.meta` plus generated `DataDir` directories as the durable layout. Do
  not add `versions/<versionId>` directories.
- Materialize `current` as an object-level pointer/symlink to the latest
  generated `DataDir`.
- Store a variable direct header only in `part.1`:
  small fixed prelude, variable metadata frame, then shard body.
- `FastOpenPart` tries `current/part.1` first for eligible latest GET.
  If the direct header is missing, stale, invalid, or ineligible, the storage
  node falls back to the Phase 1 coalesced `xl.meta` frame.
- Update write-side mutations to maintain the direct header and `current`:
  - `PutObject`, `CompleteMultipartUpload`, and `CopyObject` write eligible
    `part.1` headers from committed `FileInfo` and publish or invalidate
    `current`.
  - latest delete markers invalidate/remove `current`.
  - permanent delete invalidates `current` if it changes the latest visible
    version.
  - latest metadata-only mutations synchronously refresh `part.1` using the same
    `DataDir` and shard body, update `xl.meta`, and republish or invalidate
    `current`.
  - old-version metadata mutations update only `xl.meta` and do not touch
    `current`.
- Scanner/request-time repair only reconciles the `current` alias when the
  latest `DataDir` already has a valid embedded `part.1` header. Scanner must
  not create or rewrite embedded headers.
- Prove quorum safety: matching direct headers form the read quorum, mixed
  versions are not combined, and read/write quorum intersection prevents stale
  `current` aliases below read quorum from serving an older committed version.

Phase 2 is the main production FastGet feature.

### Phase 3: optional real current directory

Goal: benchmark whether avoiding symlink resolution is worth the additional
write/crash complexity.

- Materialize `current` as a reserved real latest `DataDir` instead of a symlink
  to a generated `DataDir`.
- Preserve the same `FastOpenPart` response stream and Phase 1 coalesced fallback.
- Preserve the same variable `part.1` header format from Phase 2.
- Design the overwrite protocol that safely turns the previous real `current`
  directory into a generated old-version `DataDir` before publishing the new
  latest.
- Compare real-directory and symlink modes for PUT cost, crash recovery,
  scanner behavior, platform support, cold-cache GET latency, and warmed-cache GET
  latency.

Phase 3 should remain optional unless benchmarks show that symlink resolution is
a material bottleneck or platform constraints require a non-symlink mode.
