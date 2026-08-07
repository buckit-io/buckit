# Single-Trip FastOpen GET - Design Note

**Status:** Implemented in this branch

**Summary:** Buckit's final FastOpen design does **not** add `current/` shadow
directories, `versions/<id>/` direct paths, or write-side shadow repair. Instead,
it adds a **read-side storage protocol** that lets each disk answer a GET with a
single streamed response:

1. read that disk's `xl.meta`;
2. encode the chosen object version into a compact FastOpen frame;
3. immediately continue with the shard body bytes from the same stream when the
   object is streamable.

The landing node opens FastOpen streams on a first wave of disks, reads only the
frame headers to establish the winning object/version/layout, then reuses the
already-open body streams for decode. If the first wave is not sufficient, it
opens additional disks or falls back to canonical GET. `xl.meta` remains
canonical; FastOpen is an opportunistic read optimization only.

This note describes the **shipped FastOpen design**. It supersedes earlier
proposals based on `current/part.1`, `versions/<versionId>/`, and write-side
shadow copies.

Read `request-flow.md` first; this note assumes its vocabulary (`xl.meta`,
erasure sets, parts, `FileInfo`, quorum, distribution, local vs remote disk
access).

---

## 1. Goal

Canonical GET pays two logical storage phases:

1. read `xl.meta` enough times to establish the visible version and layout;
2. open shard data files and decode from the selected readers.

FastOpen keeps the same correctness rules but collapses those into one per-disk
storage request. Each participating disk returns metadata and, when possible,
body bytes from the same open stream.

This is a **GET-only** optimization. It does not change the canonical write
format, object version layout, or healing authority model.

---

## 2. High-level architecture

### 2.1 Canonical state stays unchanged

FastOpen does not introduce a second on-disk object namespace.

- `xl.meta` remains the source of truth for visible versions, delete markers,
  transition state, erasure layout, and object metadata.
- The ordinary shard files under the version's existing `DataDir` remain the only
  physical shard bytes.
- PUT/DELETE do not create `current/` shadow copies or `versions/<id>/` direct
  paths for FastOpen.

FastOpen is therefore always allowed to fall back to the normal `xl.meta` path
without any repair or reconciliation step.

### 2.2 New storage primitive

Each disk implements:

- `StorageAPI.FastOpenPart`

This operation:

1. reads the disk-local `xl.meta`;
2. resolves the requested object version (`latest` or an explicit `VersionID`);
3. converts the chosen object version into a compact `FastOpenGETMeta`;
4. returns a stream whose first bytes are a checked FastOpen frame;
5. appends either:
   - shard bytes,
   - inline bytes,
   - no body for metadata-only outcomes,
   - or a transitioned marker.

This works both locally and over storage REST. The landing node therefore uses
the same abstraction for local and remote disks.

---

## 3. FastOpen frame protocol

The protocol is defined in [cmd/fastopen-frame.go](../cmd/fastopen-frame.go).

Each successful FastOpen stream begins with:

- a fixed prelude:
  - magic
  - protocol version
  - payload length
  - CRC
  - body mode
- a compact encoded payload:
  - object/version identity
  - part metadata
  - erasure metadata
  - transition metadata when relevant

Body modes are:

- `FastOpenBodyShard`
- `FastOpenBodyInline`
- `FastOpenBodyMetadataOnly`
- `FastOpenBodyTransitioned`

Frame statuses are:

- `FastOpenStatusOK`
- `FastOpenStatusDeleteMarker`
- `FastOpenStatusNotFound`
- `FastOpenStatusVersionNotFound`
- `FastOpenStatusUnsupported`

Important design point:

- FastOpen does **not** bypass `xl.meta` on the storage node.
- It bypasses a **second request phase** from the landing node by coalescing
  metadata and body into one stream per disk.

---

## 4. Request eligibility

FastOpen is intentionally narrow. The request-level gate is implemented in
[cmd/fastopen-get.go](../cmd/fastopen-get.go).

FastOpen is attempted only when:

- `BUCKIT_FAST_GET=1`
- `opts.FastGetObjInfo` is set by the caller
- the bucket is not the internal metadata bucket
- the request is not a `PartNumber` GET
- the request is not a range GET
- the request is not a replication or proxy request
- SSE-C is not requested

At object/layout selection time, FastOpen may still reject the object and fall
back. Important fallback cases include:

- unsupported frame status from any selected disk
- unsupported checksum algorithm
- inability to assemble enough valid readers
- body mode/layout combinations not handled by the FastOpen reader path
- non-zero body offset after `NewGetObjectReader`

Delete markers, zero-byte objects, and transitioned objects are still handled via
FastOpen metadata when possible, but they return metadata-only object-level
results rather than streamed shard decode.

---

## 5. Landing-node read flow

The read path lives in [cmd/fastopen-get.go](../cmd/fastopen-get.go).

### 5.1 First wave

`tryFastOpenGET` calls `openFastOpenGETInfo`, which:

1. chooses an initial wave of online disks;
2. opens `FastOpenPart` on each selected disk;
3. reads only the FastOpen frame from each stream;
4. leaves the stream positioned immediately after the frame.

Disk selection uses two modes:

- default: local-first, then stable disk order
- spread mode: rotate the first wave by object hash when
  `BUCKIT_FASTGET_SPREAD=1`

The initial open count is based on the set's configured data/parity shape, not
the object's eventual winning layout. If the object's actual layout needs more
help than the first wave provides, FastOpen opens additional disks.

### 5.2 Quorum and winning object selection

FastOpen does not invent new correctness rules. After reading the per-disk
frames, it rebuilds `FileInfo` values and reuses the same quorum and version
selection rules as canonical GET:

- `objectQuorumFromMeta`
- `reduceReadQuorumErrs`
- `listOnlineDisks`
- `pickValidFileInfo`
- `filterOnlineDisksInplace`

That means:

- `NotFound` and `VersionNotFound` are still object-level outcomes only after
  normal quorum rules are applied.
- stale but individually valid disk metadata does not win the read.
- `xl.meta` semantics remain canonical even though the landing node did not call
  the normal metadata fan-out helper.

### 5.3 Replacement wave

If the first wave is not enough before response commit:

1. FastOpen records a replacement-path attempt.
2. It opens the remaining online disks.
3. It retries object selection with the larger set.

If selection still cannot succeed, the request falls back to canonical GET unless
`BUCKIT_FASTGET_NO_FALLBACK=1`, in which case the request returns an error.

---

## 6. Reader assembly and decode

### 6.1 Reusing the opened body streams

Once the winning `FileInfo` is known, FastOpen maps the already-open per-disk
streams into erasure-slot readers.

For directly usable body streams:

- shard mode becomes a `fastOpenStreamingBitrotReader`
- inline mode is also accepted when it can be validated safely

Readers are placed by `Erasure.Index`, not by response order, so they match the
same slot semantics canonical decode expects.

### 6.2 Lazy replacement readers

If the initially selected body streams do not cover enough slots, FastOpen can
fill missing positions with lazy replacement readers.

The lazy replacement path:

1. records which disks are already engaged;
2. chooses the one disk that can satisfy each missing erasure slot;
3. opens a new `FastOpenPart` stream only when that slot is actually read;
4. validates that the replacement frame still matches the winning object/layout;
5. resumes decode from the requested shard offset.

This allows FastOpen to recover some pre-commit selection failures and some
decode-time missing-reader situations without abandoning the request before body
streaming begins.

### 6.3 Streaming bitrot verification

The FastOpen body path uses `HighwayHash256S` only. Other algorithms currently
force fallback.

The streaming reader verifies the existing on-disk bitrot layout inline:

1. read block hash
2. read block bytes
3. recompute hash
4. compare

No new shard-body format is introduced. FastOpen only changes how the landing
node gets to the shard bytes.

### 6.4 Range behavior

The current FastOpen path is effectively full-object only:

- request-level range GETs are rejected up front
- if `NewGetObjectReader` computes a non-zero offset, FastOpen falls back

Multipart part selection is also not supported in the FastOpen path.

---

## 7. Fallback model

FastOpen is allowed to fail only **before** it commits to a streaming response as
the request's object-selection path.

Pre-commit failures fall into two categories:

- `ok=false`: abandon FastOpen and fall back to canonical GET
- `ok=true, err!=nil`: FastOpen determined the object-level result, including
  delete marker or quorum failure cases

Once FastOpen has started the body goroutine and returned a `GetObjectReader`,
mid-stream failures are reported as stream errors, not by restarting through the
canonical path. This matches the fact that HTTP response commit has already
happened.

This is the central simplification versus the abandoned `current/` design:

- no request-time repair
- no direct-path reconciliation
- no crash-state cleanup
- no write-side invalidation rules

Fallback is always safe because the canonical layout was never changed.

---

## 8. Observability

FastOpen observability is exposed under
`/minio/metrics/v3/api/requests` in
[cmd/metrics-v3-api.go](../cmd/metrics-v3-api.go).

Counters include:

- `fast_open_attempted_total`
- `fast_open_hits_total`
- `fast_open_fallback_total`
- `fast_open_replacement_path_total`
- `fast_open_streams_opened_total`
- `fast_open_replacement_opens_total`
- `fast_open_selected_set_failures_total`
- `fast_open_stream_cancellations_total`
- `fast_open_final_errors_total`
- `fast_open_httptrace_connections_total`
- `fast_open_httptrace_reused_connections_total`
- `fast_open_httptrace_fresh_connections_total`
- `fast_open_httptrace_was_idle_connections_total`

Timing metrics include:

- `fast_open_try_seconds_total`
- `fast_open_try_seconds_count`
- `fast_open_open_info_seconds_total`
- `fast_open_open_info_seconds_count`
- `fast_open_body_decode_seconds_total`
- `fast_open_body_decode_seconds_count`

The old `BUCKIT_FASTOPEN_PROFILE` stderr logging path has been removed. Timing is
now metrics-based.

---

## 9. What the final implementation is not

The current FastOpen implementation does **not** do any of the following:

- no `current/part.N` shadow copy
- no `versions/<versionId>/part.N` direct namespace
- no `renameat2(RENAME_EXCHANGE)`-based latest swap
- no write-side direct-path install/invalidate protocol
- no crash-state repair for direct data paths
- no scanner-driven direct-path reconciliation
- no arbitrary range or multipart FastOpen read path
- no support for all checksum algorithms

Those ideas belonged to earlier design exploration. They are not part of the
implemented FastOpen shipped in this branch.

---

## 10. Expected behavior and tradeoffs

FastOpen improves the healthy full-object GET path by removing one landing-node
request phase to each participating disk. It does **not** remove the disk-local
`xl.meta` lookup itself.

Benefits:

- lower first-byte latency when the request stays on the FastOpen path
- fewer landing-node storage RPC phases
- opportunistic reuse of already-open body streams
- no write-path format migration

Costs and limitations:

- narrow eligibility
- fallback is still common for unsupported objects/requests
- some recovery cases require opening additional disks
- mid-stream errors are not converted into a new canonical GET
- observability is required to know whether a benchmark actually hit the path

This is therefore best understood as a conservative read-path optimization layered
on top of Buckit's existing metadata and erasure semantics.

---

## 11. References

- [cmd/fastopen-frame.go](../cmd/fastopen-frame.go) - FastOpen frame protocol and compact metadata encoding
- [cmd/fastopen-part.go](../cmd/fastopen-part.go) - disk-side `FastOpenPart` implementation
- [cmd/fastopen-get.go](../cmd/fastopen-get.go) - landing-node FastOpen GET path
- [cmd/fastget-config.go](../cmd/fastget-config.go) - runtime flags
- [cmd/metrics-v3-api.go](../cmd/metrics-v3-api.go) - exported metrics
- [docs/single-trip-get-phase1-implementation.md](single-trip-get-phase1-implementation.md) - historical prototype/implementation notes
