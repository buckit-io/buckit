# FastOpen Remove Block-0 Preflight Implementation Plan

Audience: Claude Code or an engineer reviewing/implementing the next FastOpen
change.

## 1. Summary

Remove FastOpen's pre-return block-0 decode preflight. FastOpen should align with
canonical GET semantics:

```text
metadata/quorum validation before returning GetObjectReader
data decode/bitrot validation during reader consumption
```

The current block-0 preflight decodes the first erasure block before returning
`GetObjectReader`. This is stricter than canonical GET and is the dominant TTFB
cost for small and medium objects. Recent local tests show that disabling it
improves latency materially while still returning byte-for-byte correct data for
the tested objects.

## 2. Current Behavior

Current FastOpen flow in `cmd/fastopen-get.go`:

```text
tryFastOpenGET
  openFastOpenGETInfo
  build ObjectInfo
  NewGetObjectReader
  readFastOpenGETPrefix       # block-0 preflight
    decodeFastOpenGETRange
      erasure.Decode
  return MultiReader(prefix, pipe)
```

The preflight validates the first object block before `GetObjectReader` is
returned. If it fails, FastOpen can reopen all disks or fallback before response
commit.

The performance problem is that the "prefix" is not tiny:

```text
prefixLen = min(request length, fi.Erasure.BlockSize)
```

With the current local EC:4 rig:

```text
640KiB object: prefix is the entire object
2MiB object:   prefix is 1MiB, half the object
```

This front-loads shard reads, HighwayHash verification, erasure decode, and
memory buffering before the S3 handler can start copying the response body.

## 3. Canonical GET Semantics

Canonical GET does not pre-decode block 0 before returning `GetObjectReader`.

Canonical flow in `cmd/erasure-object.go`:

```text
GetObjectNInfo
  getObjectFileInfo
    read xl.meta / versions
    objectQuorumFromMeta
    reduceReadQuorumErrs
    listOnlineDisks
    pickValidFileInfo
    filterOnlineDisksInplace
  NewGetObjectReader
  create pipe
  goroutine getObjectWithFileInfo -> erasure.Decode
  return fn(pipeReader, headers, cleanup)
```

The S3 GET handler uses `xioutil.WriteOnClose` for full-object GETs. If the
reader fails before any body bytes are written, the handler can still write an
error response. If a later read fails after bytes have been written, the stream
fails. This is canonical streaming behavior.

Therefore FastOpen does not need a pre-return block-0 decode to match canonical
safety.

## 4. Target Behavior

FastOpen should validate only metadata/protocol safety before returning the
reader:

```text
1. Request is FastOpen eligible.
2. Compact frames are syntactically valid.
3. Per-disk frame statuses are reduced using canonical quorum rules.
4. Winning FileInfo/version selection matches canonical.
5. Online disks are filtered like canonical.
6. Selected FastOpen body streams match the winning FileInfo:
   - erasure layout
   - erasure index/distribution
   - checksum algorithm supported by FastOpen streaming reader
   - body mode
   - body length
```

Actual data validation happens when the returned reader is consumed:

```text
1. First Read decodes block 0.
2. Per-shard bitrot hashes are verified by fastOpenStreamingBitrotReader.
3. Erasure decode reconstructs data.
4. Lazy replacement readers open only if a selected shard fails.
5. If decode fails before the first write, object handler can still return an error.
6. If decode fails after bytes were written, behavior matches canonical streaming failure.
```

## 5. Code Changes

### 5.1 Remove Runtime Flag

Remove the experimental no-block0 runtime flag introduced during testing:

```text
envBuckitFastOpenNoBlock0
fastGetRuntimeConfig.noBlock0
globalFastOpenNoBlock0
```

Files:

```text
cmd/fastget-config.go
cmd/fastopen-test-helpers_test.go
testing/fastopen-local/rig.sh
```

`BUCKIT_FASTOPEN_PROFILE` should remain. Only remove
`BUCKIT_FASTOPEN_NO_BLOCK0_PREFLIGHT`.

### 5.2 Simplify `tryFastOpenGET`

Replace both the original prefix path and the experimental no-block0 branch with
a single streaming path:

```text
pr, pw := xioutil.WaitPipe()
go func() {
    err := er.getObjectWithFastOpenInfo(ctx, bucket, object, off, length, pw, info)
    if err != nil {
        fastOpenRecordFinalError(ctx, err)
    }
    pw.CloseWithError(err)
}()

pipeCloser := func() {
    pr.CloseWithError(nil)
}
gr, err := fn(pr, h, pipeCloser, nsUnlocker)
return gr, true, err
```

Remove:

```text
readFastOpenGETPrefix
try_prefix_* profile events
try_no_block0_* profile events
prefix buffer / MultiReader(prefix, pipe)
prefix failure reopen-all branch
prefix-only full-object path
```

Keep:

```text
openFastOpenGETInfo
pickFastOpenGETInfo
buildFastOpenGETReaders
getObjectWithFastOpenInfo
decodeFastOpenGETRange
lazy replacement readers
streaming bitrot reader
```

### 5.3 Preserve Canonical Header/Error Timing

Do not add `WriteHeader` or any response behavior in the object layer.

The object layer should return a `GetObjectReader`; the S3 GET handler already
controls commit timing with `WriteOnClose`.

This means FastOpen should not attempt to detect first-write/commit state itself.

### 5.4 Confirm Body Stream Validation Remains

FastOpen should still validate that streams selected before return are consistent
with quorum-selected `FileInfo`.

Keep or strengthen checks in:

```text
buildFastOpenGETReaders
fastOpenReplacementPool.validateFrame
```

Required checks:

```text
frame.Status == FastOpenStatusOK for body streams
frame.BodyMode is FastOpenBodyShard or FastOpenBodyInline where allowed
frame.BodyLen matches expected bitrot shard body length or inline size
converted FileInfo matches winning object identity/version/size
erasure layout matches winning FileInfo
erasure index maps to the expected reader slot
checksum algorithm matches supported FastOpen reader algorithm
modtime/etag matches winning version identity
```

Do not replace these checks with block-0 decode. They are cheaper and validate
the FastOpen protocol trust boundary.

## 6. Test Plan

Do not run the full `./cmd` package automatically unless the user explicitly
asks. Focused tests are acceptable.

### 6.1 Existing Tests To Keep Passing

Run:

```sh
CGO_ENABLED=0 go test -v -tags kqueue,dev -run 'TestFastOpen|TestReadFastGetRuntimeConfig' ./cmd
```

Expected: pass.

### 6.2 Tests To Update

Update tests that currently expect prefix preflight behavior.

Likely affected areas:

```text
cmd/fastopen-get_test.go
cmd/fastopen-test-helpers_test.go
cmd/erasure-decode_test.go
```

Expected updates:

```text
Remove assertions for prefix preflight-specific fallback/reopen behavior.
Remove config tests for BUCKIT_FASTOPEN_NO_BLOCK0_PREFLIGHT.
Keep tests for replacement readers and body stream validation.
```

### 6.3 Tests To Add

Add or adapt tests around first-block failures. The key distinction is whether
the failure is recoverable before the first write or unrecoverable.

#### Recoverable Block-0 Corruption

Scenario:

```text
one initially selected shard corrupt/missing at block 0
enough replacement/candidate shards exist
FastOpen reader reconstructs object successfully
response bytes match canonical
```

Assert:

```text
FastOpen hit increments
fallbacks remain zero
replacement path/open metrics increment if lazy replacement is used
io.Copy succeeds
body matches expected payload
```

#### Unrecoverable Block-0 Corruption

Scenario:

```text
too many selected/candidate shards corrupt or unavailable at block 0
erasure decode cannot satisfy read quorum
```

Assert at object-layer level:

```text
GetObjectNInfo returns a reader
first io.Copy/read returns an error
no bytes or partial bytes according to the test setup
```

If there is an HTTP-level test harness available, assert canonical-compatible
behavior:

```text
if first read fails before any write, handler can emit an error response
```

Do not require FastOpen to fallback to canonical after returning the reader.

#### Successful Byte-For-Byte Read

For supported single-part objects:

```text
FastOpen ON bytes == canonical OFF bytes
640KiB and 2MiB object sizes
```

#### Metadata/Protocol Mismatch Still Fails Pre-Return

Keep/extend tests for:

```text
wrong version
wrong erasure index
wrong modtime
wrong body length
wrong distribution
wrong bitrot algorithm
wrong body mode
not OK status
```

These should still reject a stream before assigning it as a valid reader slot.

## 7. Performance Validation

After implementation, rebuild:

```sh
testing/fastopen-local/rig.sh build
```

Run clean local A/B with profiling disabled:

```text
640KiB, 200 objects, 1x-all, key-order, concurrency=1
2MiB,   200 objects, 1x-all, key-order, concurrency=1
```

Expected shape based on prior no-block0 runs:

```text
640KiB: ON improves mean/p50/p90 materially.
2MiB:   ON improves mean/p50/p90 modestly; p99 needs more rounds.
```

Record:

```text
min, mean, p50, p90, p99, max for TTFB
min, mean, p50, p90, p99, max for total latency
FastOpen hits/fallbacks/final errors/replacement metrics
```

Recommended next validation:

```text
5 rounds OFF and ON, alternating order:
OFF -> ON -> ON -> OFF, repeated or randomized
```

Use fresh object names or a reboot/drop-cache protocol if cold-cache claims are
needed.

## 8. Risks And Mitigations

### Risk: Loss Of FastOpen Fallback Before Reader Return

This is intentional. Canonical does not guarantee data-block validation before
reader return either. FastOpen should not keep a stricter and slower model unless
there is a specific product requirement.

Mitigation:

```text
Keep metadata/protocol validation pre-return.
Rely on WriteOnClose for first-read failures before response commit.
Keep final-error metrics.
Test first-block unrecoverable failure behavior.
```

### Risk: Lazy Replacement Does Not Recover Some First-Block Failures

Mitigation:

```text
Add recoverable block-0 corruption tests.
Ensure buildFastOpenGETReaders leaves enough lazy replacement readers when
available.
For inline objects, keep replacement constrained to offset 0 and only enable it
when the inline shard file fits in one erasure shard block.
Ensure replacement frame validation is strict.
```

### Risk: BodyLen Or BodyMode Mismatch Slips Through

Mitigation:

```text
Keep pre-return body stream validation.
Add tests for wrong body length and wrong body mode.
```

### Risk: Metrics Meaning Changes

FastOpen hit currently means FastOpen selected and returned the object-level
outcome. With streaming validation, a hit may later produce a read error.

Mitigation:

```text
Keep fast_open_final_errors_total for post-return decode/stream failures.
Document metric semantics.
```

## 9. Acceptance Criteria

Implementation is acceptable when:

```text
1. There is no block-0 decode before returning GetObjectReader.
2. BUCKIT_FASTOPEN_NO_BLOCK0_PREFLIGHT is removed.
3. FastOpen successful reads remain byte-for-byte identical to canonical reads.
4. Metadata/protocol mismatch tests still reject invalid streams.
5. Recoverable first-block corruption succeeds via reconstruction/replacement.
6. Unrecoverable first-block corruption returns a read error, not silent data corruption.
7. Focused FastOpen/config tests pass.
8. Clean local 640KiB and 2MiB A/B runs preserve the no-block0 latency shape.
```
