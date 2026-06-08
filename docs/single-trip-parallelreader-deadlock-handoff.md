# Single-Trip `parallelReader` Deadlock Handoff

## Context

Branch: `fix/singletrip-recon-parallelreader`

The eager single-trip GET path prefetches the first encoded block from local
shards, keeps remote shard streams positioned after the direct header, and calls
`Erasure.Decode` with local readers marked as preferred.

Claude's current uncommitted `cmd/singletrip-read.go` change already replaces the
old sparse, preselected M-reader layout with the complete agreeing M+N reader
layout. The hang remained after that change.

Observed blocked stack:

```text
parallelReader.Read at cmd/erasure-decode.go:161 (chanrecv)
Erasure.Decode
getObjectWithSingleTripInfo at cmd/singletrip-read.go:434
```

## Root Cause

`parallelReader.preferReaders` reordered `p.readers` to put preferred local
readers first, but did not apply the same permutation correctly to
`p.readerToBuf`.

The old code was:

```go
p.readers[next], p.readers[i] = p.readers[i], p.readers[next]
p.readerToBuf[next] = i
p.readerToBuf[i] = next
```

`readerToBuf` may already contain a permutation from an earlier swap. Assigning
the current indices instead of swapping the existing mapping values can create
duplicate output-buffer mappings.

One relevant six-drive EC:2 case is M=4, N=2 with three local preferred readers.
For preferred positions `[1, 2, 3]`, the old logic produces:

```text
readers:     [1, 2, 3, 0, 4, 5]
readerToBuf: [1, 2, 3, 2, 4, 5]
```

The first four reads can all succeed, but readers at positions 1 and 3 both
write to output buffer 2. Only three distinct buffers become non-empty.
`canDecode()` therefore remains false for M=4.

Successful reads send `false` to `readTriggerCh`. Once those notifications are
consumed, no goroutine sends another trigger and the channel remains open.
The loop at `cmd/erasure-decode.go:161` then waits forever on an empty channel.

## Fix

File: `cmd/erasure-decode.go`

Apply the exact same swap to the mapping as to the readers:

```go
p.readers[next], p.readers[i] = p.readers[i], p.readers[next]
p.readerToBuf[next], p.readerToBuf[i] = p.readerToBuf[i], p.readerToBuf[next]
```

This preserves a one-to-one permutation from reordered reader positions to the
readers' original erasure-shard buffer positions.

## Regression Test

File: `cmd/erasure-decode_test.go`

Added `TestParallelReaderPreferReadersDoesNotDeadlock`:

- Creates six one-byte readers with M=4.
- Marks positions `[1, 2, 3]` preferred, matching three local disks in the
  two-host six-drive layout.
- Calls `parallelReader.Read` in a goroutine.
- Fails if it does not return within one second.
- Verifies that decode does not return an error.

The existing uncommitted
`TestSingleTripDecodeMixedReadersReconstructNoHang` in
`cmd/singletrip-get_test.go` was also run. It exercises M=4/N=2 with three fast
local readers, one slow remote parity reader, and reconstruction of a missing
data shard.

Targeted command:

```sh
GOCACHE=/tmp/buckit-go-cache CGO_ENABLED=0 \
  go test -tags kqueue,dev ./cmd \
  -run 'TestParallelReaderPreferReadersDoesNotDeadlock|TestSingleTripDecodeMixedReadersReconstructNoHang' \
  -count=1 -timeout=45s
```

Result: PASS.

`git diff --check` also passed.

## Broader Test Caveat

The existing `TestErasureDecode` suite was attempted separately:

```sh
GOCACHE=/tmp/buckit-go-cache CGO_ENABLED=0 \
  go test -tags kqueue,dev ./cmd -run '^TestErasureDecode$' \
  -count=1 -timeout=2m
```

It did not reach a useful result because an unrelated goroutine panicked with an
integer divide by zero in `internal/ringbuffer/ring_buffer.go:212`, reached from
`xlStorage.writeAllDirect`. This occurred during test storage setup, outside the
changed `parallelReader` logic.

## Validation Still Needed

1. Review the full-layout changes already present in the dirty working tree and
   keep them separate from this mapping fix when committing.
2. Build and deploy the patched binary to both benchmark hosts.
3. Repeat the stress workload that previously hung, ideally with goroutine dumps
   enabled so any remaining block can be compared with the old line-161 stack.
4. Validate returned bytes and fast-path hit/fallback counters after stress.
5. Run longer ON/OFF tests for 640 KiB, 1 MiB, and 2 MiB only after the hang is
   shown to be resolved.
6. Investigate remote `ReadFileStream` cancellation separately. It can cause
   resource pressure under load, but it does not explain the specific empty
   `readTriggerCh` receive described above.

## Working Tree Warning

At the time of this handoff, these files already contain uncommitted work:

```text
cmd/erasure-decode.go
cmd/erasure-decode_test.go
cmd/singletrip-get_test.go
cmd/singletrip-read.go
.tmp/
```

Do not discard the existing `singletrip-read.go` or `singletrip-get_test.go`
changes when isolating or committing this fix.
