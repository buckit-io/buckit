# FastGet Phase 1 FastOpen Implementation Plan

**Status:** implementation plan for full-feature Phase 1

**Companion docs:**

- `docs/single-trip-get-full-feature-gaps.md`
- `docs/single-trip-get-phase1-implementation.md` for prototype history only

---

## 1. Goal

Implement Phase 1 FastGet as a coalesced `xl.meta` FastOpen path:

```
FastOpenPart -> CoalescedMetadataFrame -> optional body stream
```

The goal is to remove the separate landing-node metadata fan-out before eligible
GET reads while preserving existing GET response bytes, headers, errors,
authorization behavior, version semantics, and erasure-read correctness.

Phase 1 must not introduce `current`, embedded `part.1` headers, symlinks, real
`current` directories, scanner repair, or write-path layout changes.

---

## 2. Scope

### Supported

- GET only. HEAD remains on existing `GetObjectInfo`.
- All non-range, non-multipart GET cases.
- Latest GET, explicit `versionId` GET, versioned/suspended buckets, null
  versions, and delete-marker responses.
- Single-part local shard-backed objects.
- Inline objects, with inline bytes streamed after the frame.
- Zero-byte objects, with metadata frame and no required body stream.
- Restored-on-disk objects, preserving `x-amz-restore`.
- Remote-tier objects not restored locally, with metadata frame only and existing
  transitioned-object reader on the landing node.
- Object-lock metadata, permission-filtered on the landing node.
- `x-amz-checksum-mode: ENABLED` for non-range GET.
- Non-STANDARD storage class when the object is local or restored locally.
- SSE-S3 and SSE-KMS, with encrypted shard bytes streamed and existing
  landing-node decrypt path preserved.
- Compressed single-part full-object GET, with existing landing-node
  decompression path preserved.

### Out Of Scope

- HEAD.
- SSE-C.
- Range GET.
- `partNumber`.
- Multipart object body FastOpen.
- Multi-pool safe-probe optimization.
- Mixed-version or rolling-upgrade FastOpen compatibility. Phase 1 assumes the
  cluster is running a homogeneous FastOpen-capable binary.
- `current` path, embedded direct headers, write-path publish/invalidation, and
  scanner repair.

Out-of-scope requests use the current non-FastGet implementation.
On multi-pool deployments, existing pool/owner resolution remains unchanged and
still happens before FastOpen. The true no-extra-`xl.meta` win for Phase 1 is the
single-pool path; multi-pool may still benefit after pool selection, but the
pool-selection metadata fan-out is not optimized in this phase.

---

## 3. Core Design Rules

- Reuse existing Buckit GET semantics. Do not invent a new metadata quorum,
  version selection, erasure ordering, decode, auth, object-lock, checksum,
  encryption, compression, or lifecycle behavior.
- The storage node may read `xl.meta` and open/stream local body bytes, but the
  landing node remains responsible for request-context behavior such as auth,
  preconditions, object-lock permission filtering, checksum headers, encryption
  response headers, decryption, decompression, and final HTTP response shaping.
- The frame carries a compact GET-only metadata shape. It must be sufficient to
  reconstruct the minimal `FileInfo`/`ObjectInfo` state used by existing GET, but
  it must not carry `FileInfo` fields that are only used by write, list, repair,
  or metadata-mutation paths.
- FastOpen failures before the first response byte are handled inside FastOpen by
  opening all remaining online disks as replacements. Do not fall back to a
  second landing-node metadata fan-out for supported Phase 1 requests.
- After the first response byte is committed, do not switch semantic paths.
  Surface errors through existing read/decode behavior.
- Body bytes in shard and inline modes are per-disk encoded data. The landing
  node must wrap selected body streams in the same bitrot-verifying decode path
  before exposing any bytes to `NewGetObjectReader`.

---

## 4. Interfaces And Data Types

This section is the review boundary before coding. Names are proposed Go names;
field layout can be adjusted during implementation, but the semantics should be
settled here first.

### Storage Interface

Add a streaming storage operation alongside existing `ReadVersion` and
`ReadFileStream`:

```go
type StorageAPI interface {
    FastOpenPart(ctx context.Context, volume, object string, req FastOpenPartRequest) (io.ReadCloser, error)
}
```

The returned stream starts with a `FastOpenFramePrelude`, followed by an encoded
`CoalescedMetadataFrame`, followed by body bytes only when `BodyMode` requires a
local body stream. Errors returned from `FastOpenPart` happen before any frame is
available. Per-object semantic results such as delete marker are represented in
the frame, not as transport errors.

### Request

```go
type FastOpenPartRequest struct {
    Version       uint16
    VersionID     string
    PartNumber    int
    Offset        int64
    Length        int64
    Flags         FastOpenPartFlags
}
```

Request semantics:

- `Version` is the request protocol version.
- `VersionID == ""` means latest visible version.
- Phase 1 uses `PartNumber == 1` for shard-backed body streams.
- Phase 1 uses full-object reads only: `Offset == 0`, `Length == -1`.
- Range, `partNumber`, multipart, and SSE-C requests are rejected before
  FastOpen by the landing node.
- Future phases may add direct/current-path flags, but Phase 1 is coalesced
  `xl.meta` only.

### Prelude

```go
type FastOpenFramePrelude struct {
    Magic       [4]byte
    Version     uint16
    HeaderLen   uint32
    HeaderCRC32 uint32
    BodyMode    FastOpenBodyMode
}
```

Prelude rules:

- The prelude is fixed-size and appears at byte 0 of every successful
  `FastOpenPart` stream.
- `Magic` is `"BFG1"`; the `Version` field carries the protocol version.
- `HeaderLen` is the byte length of the encoded `CoalescedMetadataFrame`.
- `HeaderCRC32` covers only the encoded frame bytes, not body bytes.
- Unknown `Version` or invalid checksum is a FastOpen failure before client body
  bytes are sent.

### Body Mode

```go
type FastOpenBodyMode uint8

const (
    FastOpenBodyShard FastOpenBodyMode = iota + 1
    FastOpenBodyInline
    FastOpenBodyMetadataOnly
    FastOpenBodyTransitioned
)
```

Body mode semantics:

- `FastOpenBodyShard`: frame followed by this disk's encoded shard bytes from
  `DataDir/part.1`.
- `FastOpenBodyInline`: frame followed by this disk's encoded inline body bytes
  from `xl.meta`; the landing node decodes it through the same path as shard
  mode.
- `FastOpenBodyMetadataOnly`: frame only. Used for delete markers and zero-byte
  objects.
- `FastOpenBodyTransitioned`: frame only. Landing node uses existing
  transitioned-object reader.

### Coalesced Metadata Frame

```go
type CoalescedMetadataFrame struct {
    Status   FastOpenFrameStatus
    Meta     FastOpenGETMeta
    BodyMode FastOpenBodyMode
    BodyLen  int64
}

type FastOpenGETMeta struct {
    VersionID             string
    IsLatest              bool
    Legacy                bool
    ModTimeUnixNano       int64
    Size                  int64
    Metadata              map[string]string
    Transition            FastOpenTransitionMeta
    Part                  FastOpenPartMeta
    Erasure               FastOpenErasureMeta
    Checksum              []byte
    NumVersions           int
    SuccessorModTimeNanos int64
}

type FastOpenTransitionMeta struct {
    Status    string
    Object    string
    Tier      string
    VersionID string
}

type FastOpenPartMeta struct {
    Number     int
    Size       int64
    ActualSize int64
}

type FastOpenErasureMeta struct {
    DataBlocks   int
    ParityBlocks int
    BlockSize    int64
    Index        int
    Distribution []int
    Bitrot       FastOpenBitrotMeta
}

type FastOpenBitrotMeta struct {
    PartNumber int
    Algorithm  uint8
    Hash       []byte
}
```

Frame rules:

- `Meta` describes one selected object version on one disk, after the storage
  node has read `xl.meta` and applied the existing latest-version or explicit
  `versionId` selection.
- `Meta` is not raw `FileInfo`. It carries only fields needed by GET selection,
  response metadata, erasure decode, and bitrot verification.
- The landing node converts `Meta` into minimal `FileInfo` values before calling
  existing metadata quorum, erasure ordering, and decode helpers. This keeps
  quorum behavior aligned with current GET while avoiding full `FileInfo` wire
  size.
- `Metadata` is the raw, unfiltered metadata map from the selected `FileInfo`.
  The storage node must not call `cleanMetadata`, strip reserved keys, or apply
  object-lock permission filtering. The landing node reconstructs
  `ReplicationState` with `getInternalReplicationState(Metadata)`, then performs
  the same cleaning, object-lock filtering, encryption, compression, restore,
  storage-class, tag, ETag, and checksum handling as canonical GET.
- `Part` contains exactly the selected single part for Phase 1. Multipart body
  paths, range GET, and `partNumber` are out of scope, so the frame does not
  carry a full multipart part list.
- `Checksum` is the object checksum blob used by `x-amz-checksum-mode` and by
  encrypted checksum handling.
- `NumVersions` and `SuccessorModTime` are carried because current GET response
  header logic feeds `ObjectInfo.ToLifecycleOpts()` into lifecycle prediction
  headers.
- `BodyLen` is the number of bytes following the frame for `shard` and
  `inline` body modes. It is `0` for metadata-only and transitioned modes.
- `Status` distinguishes normal object, delete marker, not found/version not
  found, and unsupported/capability cases when those should be represented as a
  frame.
- `Status` is the authoritative delete-marker signal. During frame-to-`FileInfo`
  conversion, `FileInfo.Deleted` is set only when
  `Status == FastOpenStatusDeleteMarker`. Version-purge state is reconstructed
  separately from raw metadata into `ReplicationState` and
  `VersionPurgeStatus`.
- Wire fields use stable protocol encodings, not Go implementation details:
  times are Unix nanoseconds, `FastOpenBitrotMeta.Algorithm` is a stable
  FastOpen bitrot algorithm code mapped to/from Buckit's `BitrotAlgorithm`, and
  enum values are protocol constants.
- Reconstruct `ModTimeUnixNano` and `SuccessorModTimeNanos` with
  `time.Unix(0, n).UTC()` so quorum `time.Time.Equal` comparisons match
  canonical metadata reads and carry no monotonic clock component.
- FastOpen bitrot algorithm codes are:
  - `1`: `SHA256`
  - `2`: `HighwayHash256`
  - `3`: `HighwayHash256S`
  - `4`: `BLAKE2b512`
  Unknown algorithm codes are fail-safe: treat the frame as unsupported/corrupt
  for quorum selection and never attempt decode with a guessed algorithm.
- `FastOpenBitrotMeta.Hash` is equivalent to existing `ChecksumInfo.Hash` for
  the selected part. For streaming bitrot, per-block hashes remain in the body
  stream and this hash may be empty, matching canonical verifier setup.
- The frame must not carry `Volume`, `Name`, `DataDir`, inline `Data`, full
  multipart `Parts`, `Mode`, `WrittenByVersion`, `MarkDeleted`, `Fresh`, `Idx`,
  or `Versioned`. The landing node already knows bucket/object/versioning
  context, Phase 1 streams body bytes after the frame, and these fields are not
  needed for GET.
- For the plain single-part objects used in the performance tests, the target
  frame size is approximately the selected-version `xl.meta` size plus small
  framing overhead. With observed `xl.meta` around 368-384 bytes, the compact
  frame should remain in the same range rather than growing to a multi-KB
  payload. This is not a hard bound; metadata size is data-dependent.

### Status

```go
type FastOpenFrameStatus uint8

const (
    FastOpenStatusOK FastOpenFrameStatus = iota
    FastOpenStatusDeleteMarker
    FastOpenStatusNotFound
    FastOpenStatusVersionNotFound
    FastOpenStatusUnsupported
)
```

Status rules:

- Transport/storage failures before frame creation return `error`.
- Delete marker is a valid frame status so the landing node can return existing
  delete-marker behavior with metadata.
- `FastOpenStatusNotFound` and `FastOpenStatusVersionNotFound` are disk-local
  results. They do not immediately become a 404; the landing node applies the
  same not-found/read-quorum logic as canonical GET, after any required
  replacement opens.
- Unsupported means the request or selected object cannot be served by
  FastOpen; the landing node may use current non-FastGet implementation only if
  the request type is out of Phase 1 scope or capability is missing. Supported
  Phase 1 requests should exhaust online-disk FastOpen replacement before
  returning an error.

### Encoding Choice

Use a small fixed prelude plus msgp-encoded frame payload unless implementation
review finds a strong reason to use another existing Buckit codec. The frame
must be versioned and checksummed from the first implementation so future direct
headers can reuse the framing shape.

---

## 5. Storage Node Work

Add local `xlStorage` support for `FastOpenPart`:

1. Read `xl.meta`.
2. Select latest visible version or explicit `versionId` using existing
   `xl.meta`/`FileInfo` helpers.
3. Build compact `FastOpenGETMeta` from the selected `FileInfo`, omitting fields
   that are not used by GET. Preserve the raw metadata map exactly as canonical
   `ToFileInfo` would expose it.
4. Decide body mode:
   - delete marker: metadata-only;
   - zero-byte: metadata-only;
   - inline: inline body;
   - transitioned and not restored: transitioned metadata-only;
   - local shard-backed: open selected `DataDir/part.1` and stream shard bytes.
5. Return errors before any body bytes when metadata cannot be read or the
   version cannot be selected.

Add remote storage transport support through the existing storage REST/grid
pattern. The final endpoint choice can follow the local code ownership and
streaming constraints, but it must support request-context cancellation.

Storage timeout behavior must follow the current GET path:

- `xl.meta` read and frame construction are bounded by the same drive timeout
  used by `ReadXL`/`ReadVersion` (`globalDriveConfig.GetMaxTimeout()` through
  the existing storage wrappers).
- Body stream open/read follows `ReadFileStream` plus existing bitrot/decode
  behavior. Phase 1 must not add a new per-body-read timeout policy.
- FastOpen adds cancellation for streams it opened before final shard selection.
  This cancellation is for abandoning unused/replaced streams, not for changing
  canonical body-read timeout semantics.

---

## 6. Landing Node Work

Add a Phase 1 FastOpen path in `erasureObjects.GetObjectNInfo` before the
current `getObjectFileInfo` fan-out for eligible requests.

Eligibility before FastOpen:

- FastGet enabled;
- GET path only;
- not SSE-C request;
- no range;
- no `partNumber`;
- request is not replication/proxy special case.

FastOpen read flow:

1. Select only disks where `disk != nil && disk.IsOnline()`.
2. Open maximum configured read quorum over online disks, with no hedge.
3. Give every `FastOpenPart` call its own cancellable child context.
4. Convert returned `FastOpenGETMeta` frames into minimal `FileInfo`/metadata
   arrays used by current Buckit GET selection.
   - Rebuild `ReplicationState` from raw metadata with
     `getInternalReplicationState`.
   - Rebuild `Parts` as a one-entry slice from `FastOpenPartMeta`.
   - Rebuild inline state, free-version state, encryption/compression state,
     restore state, tags, ETag, and object-lock metadata from the raw metadata
     map, matching canonical `FileInfo.ToObjectInfo`.
5. Apply existing Buckit metadata quorum, erasure distribution ordering, and
   decode semantics.
6. If the initial selected set fails before first response byte, open all
   remaining online disks as replacements and reapply the same selection logic.
7. Cancel every unused stream immediately after selecting the winning read set.
8. For shard body mode, wrap selected encoded shard streams in a streaming
   bitrot-verifying `ReaderAt`, erasure-decode with the existing decode path,
   and wrap with existing `NewGetObjectReader` behavior.
9. For inline body mode, use the same bitrot-verifying erasure-decode path as
   shard body mode.
10. For metadata-only delete marker/zero-byte, return existing GET behavior.
11. For transitioned mode, call existing transitioned-object reader using the
    reconstructed `ObjectInfo`.

No HTTP body bytes are committed until FastOpen has selected a metadata/body
quorum and the first decode attempt can start from offset 0. If the initial
selected set fails during frame processing or block-0 decode before any client
body byte is written, FastOpen opens all remaining online disks and restarts
selection/decode from offset 0. After the first client body byte is written,
FastOpen follows existing stream/decode error behavior.

The initial open count is intentionally the maximum configured read quorum only,
with no hedge. If a disk is reachable and reports online but consistently times
out or returns stale/corrupt data, requests may take two waves: selected set
first, then all remaining online disks. This is accepted for Phase 1 correctness;
later tuning may widen the initial open set when the erasure set is known
degraded.

---

## 7. Cancellation Contract

- One cancellable child context per `FastOpenPart` stream.
- Client request cancellation cancels every child context.
- Unused streams are closed and canceled immediately after a winner is selected.
- Replacement losers are closed and canceled immediately.
- If FastOpen exits before client bytes are sent, all opened streams are closed
  and canceled.
- Remote storage handlers must stop reading local files and stop writing the
  response when the request context is canceled.
- Do not drain unused remote streams.

---

## 8. Error And Fallback Rules

- Unsupported request type: use current non-FastGet implementation.
- Missing FastOpen capability on any required storage node/set is a deployment
  incompatibility. Phase 1 does not support rolling upgrade to this version. The
  implementation may disable FastOpen and use the current implementation if this
  is detected, but it does not need request-time mixed-version negotiation.
- Supported request, selected set fails or times out before first client byte:
  open all remaining online disks as replacements.
- Supported request, all online disks still cannot satisfy existing Buckit
  metadata/read quorum: return the same class of error as existing GET.
- After first client byte: no semantic fallback and no new timeout behavior;
  return existing stream/decode error behavior.

---

## 9. Testing Plan

### Functional Equivalence

- Gating golden test: for each supported scenario below, run canonical GET and
  FastOpen GET against the same object and byte-compare the resulting
  `ObjectInfo`, response headers, status/error, and body.
- Latest single-part GET body/header equality.
- Explicit `versionId` GET.
- Versioned bucket latest GET.
- Version-suspended/null version GET.
- Delete marker latest and explicit delete marker version.
- Zero-byte object.
- Inline object.
- Restored-on-disk object.
- Remote-tier not restored.
- Object-lock metadata with and without retention/legal-hold permissions.
- Tags and user metadata.
- `x-amz-checksum-mode: ENABLED`.
- Non-STANDARD storage class / altered parity layout.
- SSE-S3 and SSE-KMS.
- Compressed single-part full-object GET.
- Replication-configured source object, including replication status and version
  purge status headers.
- Lifecycle-eligible object where prediction headers are emitted.
- Legacy/XLV1 object if Phase 1 keeps XLV1 in scope.

### Failure And Availability

- Initial selected disk missing object/version.
- Initial selected disk stale `xl.meta`.
- Initial selected disk missing `DataDir/part.1`.
- Initial selected disk returns corrupt body and bitrot verification detects the
  corruption before or during decode.
- Initial selected set cannot reach quorum, replacement opens all remaining
  online disks.
- Initial selected stream fails during block-0 decode before response commit,
  replacement restarts from offset 0 and succeeds when quorum is available.
- Replacement set reaches quorum and response succeeds.
- All online disks cannot reach quorum and error matches existing GET.
- Client disconnect cancels remote reads.
- Unused selected streams are canceled and not drained.
- Post-first-byte stream failure follows existing decode/read error behavior.
- Inline object follows the same decode behavior as shard-backed objects.

### Regression

- Range, `partNumber`, multipart, HEAD, and SSE-C continue through current
  non-FastGet implementation.
- Multi-pool selection behavior is unchanged; FastOpen runs only after existing
  pool/owner resolution.
- Mixed-version cluster with missing FastOpen support disables FastOpen or uses
  the current implementation; rolling-upgrade compatibility is not provided.
- Existing FastGet prototype tests are either updated for the new Phase 1 path or
  retired if they only validate obsolete `current` shadow behavior.

---

## 10. Milestone Task List

### Milestone 1: Frame And Conversion Foundation

- [ ] Add `FastOpenFramePrelude`, `CoalescedMetadataFrame`,
  `FastOpenGETMeta`, body-mode/status enums, and stable bitrot algorithm code
  mapping.
- [ ] Add frame encode/decode helpers with `"BFG1"` magic, protocol version,
  header length, and frame CRC validation.
- [ ] Add `fileInfoToFastOpenGETMeta` and `fastOpenGETMetaToFileInfo`
  conversion helpers.
- [ ] In conversion tests, verify raw metadata is preserved, `ReplicationState`
  is rebuilt with `getInternalReplicationState`, `Deleted` comes only from
  `FastOpenStatusDeleteMarker`, time values round-trip through Unix nanos, and
  unknown bitrot algorithm codes fail safely.
- [ ] Add golden conversion tests comparing canonical `FileInfo.ToObjectInfo`
  output with compact-frame reconstructed output for representative metadata
  combinations.

### Milestone 2: Storage-Node xl.meta FastOpenPart

- [ ] Add disk-local `xlStorage.FastOpenPart` support that reads this disk's
  `xl.meta`, selects latest or explicit `versionId`, and emits a compact
  `CoalescedMetadataFrame`.
- [ ] Add metadata-only frame results for not found, version not found, delete
  marker, zero-byte object, and transitioned object not restored locally.
- [ ] Add inline object support by returning the frame plus encoded inline body
  bytes from `xl.meta`.
- [ ] Add single-part shard-backed support by returning the frame plus an opened
  `DataDir/part.1` encoded shard stream.
- [ ] Preserve canonical storage timeout behavior: `xl.meta`/frame construction
  uses the existing drive timeout, while body stream behavior follows
  `ReadFileStream` plus existing bitrot/decode semantics.
- [ ] Reject or return unsupported for out-of-scope requests before body bytes:
  HEAD, SSE-C, range, `partNumber`, and multipart body paths.

### Milestone 3: Remote Transport And Cancellation

- [ ] Add storage REST/grid transport for `FastOpenPart` with the same framed
  response stream shape as disk-local storage.
- [ ] Ensure the remote handler invokes the storage-node `xl.meta`
  `FastOpenPart` operation on the host where the target disk is mounted.
- [ ] Give every opened `FastOpenPart` stream a cancellable child context.
- [ ] Ensure closing unused or replacement-loser streams cancels remote work
  without draining.
- [ ] Ensure client disconnect cancels all open FastOpen child contexts.
- [ ] Add transport tests for frame decode errors, context cancellation, and
  remote handler cleanup.

### Milestone 4: Landing-Node Selection And Decode

- [ ] Add eligible GET-only FastOpen entrypoint in `erasureObjects.GetObjectNInfo`
  before the current `getObjectFileInfo` fan-out.
- [ ] Select only `disk != nil && disk.IsOnline()` and initially open the maximum
  configured read quorum over online disks, with no hedge.
- [ ] Convert frames into minimal `FileInfo` arrays and reuse existing
  `objectQuorumFromMeta`, not-found quorum handling, `pickValidFileInfo`,
  erasure distribution ordering, and decode helpers.
- [ ] Wrap selected shard/inline body streams in a streaming bitrot-verifying
  `ReaderAt` before erasure decode.
- [ ] Do not commit HTTP body bytes until frame quorum and block-0 decode can
  start from offset 0.

### Milestone 5: Replacement, Errors, And Fallback

- [ ] If the initial selected set fails, times out, or cannot form quorum before
  the first client byte, open all remaining online disks and restart
  selection/decode from offset 0.
- [ ] Apply per-disk `NotFound` and `VersionNotFound` through canonical quorum
  logic rather than returning immediate 404.
- [ ] After first client byte, preserve canonical stream/decode error behavior
  and do not switch semantic paths.
- [ ] Treat missing FastOpen capability as deployment incompatibility for this
  version; rolling-upgrade compatibility is out of scope.
- [ ] Add failure tests for stale metadata, missing body, corrupt body with
  bitrot detection, replacement success, replacement failure, client disconnect,
  and unused-stream cancellation.

### Milestone 6: Golden Equivalence And Scope Gates

- [ ] Add the gating canonical-vs-FastOpen golden GET test across the full
  supported matrix in Section 9.
- [ ] Verify exact equality for `ObjectInfo`, response headers, status/error,
  and body bytes.
- [ ] Include replication-configured source objects, version purge status,
  object lock permissions, tags, checksum mode, SSE-S3/SSE-KMS, compression,
  restored-on-disk objects, remote-tier metadata-only objects, versioned/latest
  and explicit-version reads, inline objects, zero-byte objects, and altered
  storage class/parity.
- [ ] Verify out-of-scope requests continue through the current non-FastGet path:
  HEAD, SSE-C, range, `partNumber`, multipart body paths, and multi-pool pool
  selection.

### Milestone 7: Observability And Cleanup

- [ ] Add minimal metrics listed in Section 11.
- [ ] Remove or retire obsolete prototype-only FastGet tests that validate
  `current` shadow behavior rather than Phase 1 coalesced FastOpen behavior.
- [ ] Keep noisy profiling/tracing logs out of the final path.
- [ ] Run focused `go test -tags kqueue,dev ./cmd` coverage for new FastOpen
  tests, then broaden as needed for touched shared helpers.

---

## 11. Metrics

Keep observability minimal:

- FastOpen attempted.
- FastOpen hit.
- FastOpen unsupported request.
- FastOpen replacement path used.
- FastOpen streams opened per GET.
- FastOpen replacement opens per GET.
- FastOpen selected-set failure reason.
- FastOpen stream cancellation count.
- FastOpen final error category.

Do not reintroduce per-phase profiling logs or high-cardinality diagnostics.
