// Copyright (c) 2015-2026 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buckit-io/buckit/internal/config/storageclass"
)

func unsetFastGetEnvForTest(t *testing.T) {
	t.Helper()

	keys := []string{
		envBuckitFastGet,
		envBuckitFastGetEager,
		envBuckitFastGetEagerSelect,
		envBuckitFastGetSpread,
		envBuckitFastGetHedge,
		envBuckitFastGetNoFallback,
	}
	old := make(map[string]string, len(keys))
	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			old[key] = value
			present[key] = true
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, key := range keys {
			if present[key] {
				if err := os.Setenv(key, old[key]); err != nil {
					t.Error(err)
				}
			} else if err := os.Unsetenv(key); err != nil {
				t.Error(err)
			}
		}
	})
}

func TestReadFastGetRuntimeConfigDefaultsToEagerStable(t *testing.T) {
	unsetFastGetEnvForTest(t)
	t.Setenv(envBuckitFastGet, "1")

	cfg := readFastGetRuntimeConfig()
	if !cfg.enabled || !cfg.eager || !cfg.spreadSelection {
		t.Fatalf("default fast-get config = %+v, want enabled eager spread", cfg)
	}
	if cfg.eagerSelected || cfg.hedgeShards || cfg.noFallback {
		t.Fatalf("default fast-get diagnostics = %+v, want eager-selected/hedge/no-fallback disabled", cfg)
	}
}

func TestReadFastGetRuntimeConfigHonorsExplicitOverrides(t *testing.T) {
	unsetFastGetEnvForTest(t)
	t.Setenv(envBuckitFastGet, "1")
	t.Setenv(envBuckitFastGetEager, "0")
	t.Setenv(envBuckitFastGetSpread, "0")
	t.Setenv(envBuckitFastGetHedge, "1")

	cfg := readFastGetRuntimeConfig()
	if !cfg.enabled {
		t.Fatalf("enabled = false, want true")
	}
	if cfg.eager || cfg.spreadSelection {
		t.Fatalf("explicit disabled fast-get config = %+v, want eager/spread disabled", cfg)
	}
	if !cfg.hedgeShards {
		t.Fatalf("hedgeShards = false, want true")
	}
}

// singleTripSlowReaderAt delays each ReadAt — mimics a slow/remote shard read,
// the ingredient missing from the all-local unit tests.
type singleTripSlowReaderAt struct {
	inner io.ReaderAt
	delay time.Duration
}

func (s singleTripSlowReaderAt) ReadAt(p []byte, off int64) (int, error) {
	time.Sleep(s.delay)
	return s.inner.ReadAt(p, off)
}

// TestSingleTripDecodeMixedReadersReconstructNoHang reproduces the rig hang at the
// decode layer: the fast path hands erasure.Decode a mix of instant buffer-backed
// readers (eager-prefetched local shards) and one slow reader (remote shard), with
// a data shard withheld so parity reconstruction is required. Looped to trigger the
// timing-dependent parallelReader deadlock. A watchdog fails fast if Decode hangs.
func TestSingleTripDecodeMixedReadersReconstructNoHang(t *testing.T) {
	ctx := context.Background()
	const dataBlocks, parityBlocks = 4, 2
	const blockSize = int64(1 << 20)
	erasure, err := NewErasure(ctx, dataBlocks, parityBlocks, blockSize)
	if err != nil {
		t.Fatal(err)
	}
	payload := makeSingleTripTestData(640*1024, 33) // single-block, like the rig 640 KiB
	shards, err := erasure.EncodeData(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	shardSize := erasure.ShardSize()
	encoded := make([][]byte, len(shards))
	for i := range shards {
		encoded[i] = buildTestSingleTripBitrotPayload(t, shards[i], shardSize)
	}

	for iter := 0; iter < 200; iter++ {
		// Fresh readers each iteration (the streaming readers are forward-only).
		// Withhold data shard 0 AND the 2nd parity, leaving exactly M=4 usable shards:
		// 3 instant (data 1..3) + 1 slow (1st parity). parallelReader must read all 4
		// (3 instant local + 1 slow remote) and reconstruct data shard 0 from parity —
		// exactly the rig's "prefer-local + 1 remote + reconstruction" mix.
		readers := make([]io.ReaderAt, len(shards))
		prefer := make([]bool, len(shards))
		for i := range shards {
			switch i {
			case 0, dataBlocks + 1:
				readers[i] = nil // withheld
			case dataBlocks: // 1st parity: the slow "remote" reader
				br := newSingleTripStreamingBitrotReader(io.NopCloser(bytes.NewReader(encoded[i])), DefaultBitrotAlgorithm, shardSize)
				readers[i] = singleTripSlowReaderAt{inner: br, delay: 20 * time.Millisecond}
				prefer[i] = false
			default: // data 1..3: instant buffer-backed (eager-prefetched local)
				readers[i] = newSingleTripStreamingBitrotReader(io.NopCloser(bytes.NewReader(encoded[i])), DefaultBitrotAlgorithm, shardSize)
				prefer[i] = true
			}
		}

		done := make(chan error, 1)
		var out bytes.Buffer
		go func() {
			_, derr := erasure.Decode(ctx, &out, readers, 0, int64(len(payload)), int64(len(payload)), prefer)
			done <- derr
		}()
		select {
		case derr := <-done:
			if derr != nil {
				t.Fatalf("iter %d: decode error: %v", iter, derr)
			}
			if !bytes.Equal(out.Bytes(), payload) {
				t.Fatalf("iter %d: decoded bytes mismatch", iter)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("iter %d: DECODE HANG reproduced (parallelReader deadlock)", iter)
		}
	}
}

type singleTripCountingDisk struct {
	StorageAPI
	readFileStreamCalls atomic.Int64
}

func (d *singleTripCountingDisk) ReadFileStream(ctx context.Context, volume, path string, offset, length int64) (io.ReadCloser, error) {
	d.readFileStreamCalls.Add(1)
	return d.StorageAPI.ReadFileStream(ctx, volume, path, offset, length)
}

func TestSingleTripFastGetEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	obj, fsDirs, err := prepareErasure16(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Shutdown(t.Context())
	defer removeRoots(fsDirs)

	z := obj.(*erasureServerPools)
	sets := z.serverPools[0]
	xl := sets.sets[0]

	origDisks := xl.getDisks()
	countingDisks := make([]*singleTripCountingDisk, len(origDisks))
	wrappedDisks := make([]StorageAPI, len(origDisks))
	for i, disk := range origDisks {
		countingDisks[i] = &singleTripCountingDisk{StorageAPI: disk}
		wrappedDisks[i] = countingDisks[i]
	}

	sets.erasureDisksMu.Lock()
	xl.getDisks = func() []StorageAPI { return wrappedDisks }
	sets.erasureDisksMu.Unlock()

	t.Cleanup(func() {
		sets.erasureDisksMu.Lock()
		xl.getDisks = func() []StorageAPI { return origDisks }
		sets.erasureDisksMu.Unlock()
	})

	withSingleTripEnabled(t, true)

	bucket := "bucket"
	object := "object"
	data := makeSingleTripTestData(smallFileThreshold*16, 17)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	userDefined := map[string]string{
		"content-type":  "application/singletrip-test",
		"cache-control": "max-age=60",
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{
		UserDefined: userDefined,
	}); err != nil {
		t.Fatal(err)
	}
	assertSingleTripFastInfoAvailable(t, xl, bucket, object)

	resetSingleTripReadFileStreamCounts(countingDisks)
	globalFastGetEnabled = false
	hitsBefore, fallbacksBefore := singleTripCounterSnapshot()
	baseline, baselineInfo := readSingleTripTestObject(t, xl, bucket, object)
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 0, 0, "baseline")

	resetSingleTripReadFileStreamCounts(countingDisks)
	globalFastGetEnabled = true
	hitsBefore, fallbacksBefore = singleTripCounterSnapshot()
	fast, fastInfo := readSingleTripTestObject(t, xl, bucket, object)

	if !bytes.Equal(fast, baseline) {
		t.Fatalf("fast bytes differ from baseline: got %d bytes, want %d", len(fast), len(baseline))
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 1, 0, "fast")
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertSingleTripQuorumOpens(t, xl, countingDisks, "fast")

	resetSingleTripReadFileStreamCounts(countingDisks)
	rangeLen := smallFileThreshold*3 + 123
	hitsBefore, fallbacksBefore = singleTripCounterSnapshot()
	fastRange, _ := readSingleTripTestObjectRange(t, xl, bucket, object, &HTTPRangeSpec{Start: 0, End: int64(rangeLen - 1)})
	if !bytes.Equal(fastRange, data[:rangeLen]) {
		t.Fatalf("fast range bytes differ: got %d bytes, want %d", len(fastRange), rangeLen)
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 1, 0, "range")
	assertSingleTripQuorumOpens(t, xl, countingDisks, "range")
}

// assertSingleTripQuorumOpens verifies the fast path opened exactly the read quorum
// of shadow streams (prefer-local) — not all M+N — so a healthy GET reads the
// minimum number of shards.
func assertSingleTripQuorumOpens(t *testing.T, xl *erasureObjects, disks []*singleTripCountingDisk, label string) {
	t.Helper()
	dataCount := xl.setDriveCount - xl.defaultParityCount
	want := dataCount
	if dataCount == xl.defaultParityCount {
		want = dataCount + 1
	}
	total := 0
	for i, d := range disks {
		got := int(d.readFileStreamCalls.Load())
		if got > 1 {
			t.Fatalf("%s: disk %d ReadFileStream calls = %d, want 0 or 1", label, i, got)
		}
		total += got
	}
	if total != want {
		t.Fatalf("%s: total ReadFileStream calls = %d, want %d", label, total, want)
	}
}

func TestSingleTripFastOpenRemoteSelectionIsStable(t *testing.T) {
	oldSpread := globalFastGetSpreadSelection
	globalFastGetSpreadSelection = false
	t.Cleanup(func() {
		globalFastGetSpreadSelection = oldSpread
	})

	disks := make([]StorageAPI, 3) // nil disks are treated as non-local candidates.
	first := selectSingleTripFastOpenDisks(disks, 1, "bucket", "object")
	if len(first) != 1 {
		t.Fatalf("selection length = %d, want 1", len(first))
	}
	for i := 0; i < 9; i++ {
		sel := selectSingleTripFastOpenDisks(disks, 1, "bucket", "object")
		if !slices.Equal(sel, first) {
			t.Fatalf("selection %d = %v, want stable %v", i, sel, first)
		}
	}
}

func TestSingleTripFastOpenSpreadSelectionIsStable(t *testing.T) {
	first := selectSingleTripSpreadDisks(6, 4, "bucket", "object")
	if len(first) != 4 {
		t.Fatalf("selection length = %d, want 4", len(first))
	}
	local := 0
	for _, disk := range first {
		if disk%2 == 0 { // EC:2 interleaved order: local, remote, local, remote...
			local++
		}
	}
	if local != 2 {
		t.Fatalf("selection %v has %d local slots, want 2", first, local)
	}
	for i := 0; i < 6; i++ {
		sel := selectSingleTripSpreadDisks(6, 4, "bucket", "object")
		if !slices.Equal(sel, first) {
			t.Fatalf("selection %d = %v, want stable %v", i, sel, first)
		}
	}
}

func TestSingleTripFastGetFallbackWithoutShadow(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	obj, fsDirs, err := prepareErasure16(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Shutdown(t.Context())
	defer removeRoots(fsDirs)

	z := obj.(*erasureServerPools)
	xl := z.serverPools[0].sets[0]

	withSingleTripEnabled(t, false)

	bucket := "bucket"
	object := "object"
	data := makeSingleTripTestData(smallFileThreshold*16, 23)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	globalFastGetEnabled = true
	hitsBefore, fallbacksBefore := singleTripCounterSnapshot()
	got, _ := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(got, data) {
		t.Fatalf("fallback bytes differ: got %d bytes, want %d", len(got), len(data))
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 0, 1, "fallback")
}

func TestSingleTripFastGetOverwriteAndDeleteInvalidatesShadow(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	z, cleanup := prepareSingleTripErasureObject(t, ctx)
	defer cleanup()
	withSingleTripEnabled(t, true)

	xl := z.serverPools[0].sets[0]
	bucket := "bucket"
	object := "object"
	dataA := makeSingleTripTestData(smallFileThreshold*16, 31)
	dataB := makeSingleTripTestData(smallFileThreshold*16, 47)
	if err := z.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(dataA), int64(len(dataA)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	hitsBefore, fallbacksBefore := singleTripCounterSnapshot()
	if got, _ := readSingleTripTestObject(t, xl, bucket, object); !bytes.Equal(got, dataA) {
		t.Fatal("initial fast GET did not return object A")
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 1, 0, "initial")

	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(dataB), int64(len(dataB)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	hitsBefore, fallbacksBefore = singleTripCounterSnapshot()
	if got, _ := readSingleTripTestObject(t, xl, bucket, object); !bytes.Equal(got, dataB) {
		t.Fatal("overwrite fast GET did not return object B")
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 1, 0, "overwrite")

	if _, err := z.DeleteObject(ctx, bucket, object, ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	hitsBefore, fallbacksBefore = singleTripCounterSnapshot()
	gr, err := xl.GetObjectNInfo(ctx, bucket, object, nil, http.Header{}, ObjectOptions{})
	if err == nil {
		gr.Close()
		t.Fatal("GET after delete unexpectedly succeeded")
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 0, 1, "delete")
}

func TestSingleTripFastGetEligibleToIneligibleOverwriteFallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	z, cleanup := prepareSingleTripErasureObject(t, ctx)
	defer cleanup()
	withSingleTripEnabled(t, true)

	xl := z.serverPools[0].sets[0]
	bucket := "bucket"
	object := "object"
	dataA := makeSingleTripTestData(smallFileThreshold*16, 53)
	dataB := makeSingleTripTestData(smallFileThreshold*16, 71)
	if err := z.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(dataA), int64(len(dataA)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(dataB), int64(len(dataB)), "", ""), ObjectOptions{
		UserDefined: map[string]string{
			"x-amz-meta-test": "forces-fallback",
		},
	}); err != nil {
		t.Fatal(err)
	}
	hitsBefore, fallbacksBefore := singleTripCounterSnapshot()
	got, _ := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(got, dataB) {
		t.Fatal("eligible-to-ineligible overwrite did not return object B")
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 0, 1, "ineligible overwrite")
}

func TestSingleTripFastGetOverCapMetadataFallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	z, cleanup := prepareSingleTripErasureObject(t, ctx)
	defer cleanup()
	withSingleTripEnabled(t, true)

	xl := z.serverPools[0].sets[0]
	bucket := "bucket"
	object := "object"
	data := makeSingleTripTestData(smallFileThreshold*16, 89)
	longContentType := strings.Repeat("a", singleTripContentTypeMax+1)
	if err := z.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{
		UserDefined: map[string]string{
			"content-type": longContentType,
		},
	}); err != nil {
		t.Fatal(err)
	}
	hitsBefore, fallbacksBefore := singleTripCounterSnapshot()
	got, info := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(got, data) {
		t.Fatal("over-cap fallback did not return object bytes")
	}
	if info.ContentType != longContentType {
		t.Fatalf("content-type = len %d, want len %d", len(info.ContentType), len(longContentType))
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 0, 1, "over-cap")
}

func TestSingleTripFastGetMidStreamCorruptionErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	z, cleanup := prepareSingleTripErasureObject(t, ctx)
	defer cleanup()
	withSingleTripEnabled(t, true)

	xl := z.serverPools[0].sets[0]
	bucket := "bucket"
	object := "object"
	data := makeSingleTripTestData(3*blockSizeV2+12345, 101)
	if err := z.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	corruptSingleTripShadowDataShard(t, xl, bucket, object, 1)

	globalFastGetEnabled = false
	baseline, _ := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(baseline, data) {
		t.Fatal("canonical path did not return uncorrupted object")
	}

	globalFastGetEnabled = true
	hitsBefore, fallbacksBefore := singleTripCounterSnapshot()
	got, _, err := readSingleTripTestObjectRangeAllowError(t, xl, bucket, object, nil)
	if err == nil {
		t.Fatal("fast GET unexpectedly succeeded with corrupted direct shadow")
	}
	if len(got) == 0 {
		t.Fatal("fast GET failed before streaming any bytes; expected mid-stream failure")
	}
	if bytes.Equal(got, data) {
		t.Fatal("fast GET returned complete data despite corrupted direct shadow")
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 1, 0, "corruption")
}

func TestSingleTripFastGetFallsBackWhenQuorumShadowMissing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	z, cleanup := prepareSingleTripErasureObject(t, ctx)
	defer cleanup()
	withSingleTripEnabled(t, true)

	xl := z.serverPools[0].sets[0]
	bucket := "bucket"
	object := "object"
	// Multi-block, above the inline cutoff so a standalone shadow exists.
	data := makeSingleTripTestData(3*blockSizeV2+12345, 77)
	if err := z.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	// The fast path now opens exactly the read quorum of shadows (prefer-local), with
	// no spare. Removing the shadow on a disk that is in that quorum (disk 0, opened
	// first) means the fast path can't reach quorum and must fall back to the
	// canonical path — which still returns the correct bytes from xl.meta +
	// DataDir/part.1. (In-decode reconstruction of a *missing* shard was the cost of
	// dropping to exactly-quorum reads; reconstruction from parity that happens to be
	// among the opened quorum is still exercised by the end-to-end path.)
	deleteSingleTripShadowOnDisk(t, xl, bucket, object, 0)

	hitsBefore, fallbacksBefore := singleTripCounterSnapshot()
	got, _ := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(got, data) {
		t.Fatal("fallback GET returned wrong bytes")
	}
	assertSingleTripCounterDelta(t, hitsBefore, fallbacksBefore, 0, 1, "fallback")
}

func deleteSingleTripShadowOnDisk(t *testing.T, xl *erasureObjects, bucket, object string, diskIdx int) {
	t.Helper()
	disk := xl.getDisks()[diskIdx]
	if err := disk.Delete(t.Context(), bucket, pathJoin(object, singleTripCurrentDir), DeleteOptions{Recursive: true}); err != nil {
		t.Fatal(err)
	}
}

type singleTripGetObjectNInfo interface {
	GetObjectNInfo(ctx context.Context, bucket, object string, rs *HTTPRangeSpec, h http.Header, opts ObjectOptions) (*GetObjectReader, error)
}

func readSingleTripTestObject(t *testing.T, obj singleTripGetObjectNInfo, bucket, object string) ([]byte, ObjectInfo) {
	return readSingleTripTestObjectRange(t, obj, bucket, object, nil)
}

func readSingleTripTestObjectRange(t *testing.T, obj singleTripGetObjectNInfo, bucket, object string, rs *HTTPRangeSpec) ([]byte, ObjectInfo) {
	t.Helper()

	out, info, err := readSingleTripTestObjectRangeAllowError(t, obj, bucket, object, rs)
	if err != nil {
		t.Fatal(err)
	}
	return out, info
}

func readSingleTripTestObjectRangeAllowError(t *testing.T, obj singleTripGetObjectNInfo, bucket, object string, rs *HTTPRangeSpec) ([]byte, ObjectInfo, error) {
	t.Helper()

	gr, err := obj.GetObjectNInfo(t.Context(), bucket, object, rs, http.Header{}, ObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	defer gr.Close()

	var out bytes.Buffer
	if _, err = io.Copy(&out, gr); err != nil {
		return out.Bytes(), gr.ObjInfo, err
	}
	return out.Bytes(), gr.ObjInfo, nil
}

func makeSingleTripTestData(size int, seed byte) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*31 + int(seed)) % 251)
	}
	return data
}

func corruptSingleTripShadowDataShard(t *testing.T, xl *erasureObjects, bucket, object string, erasureIndex uint16) {
	t.Helper()

	for _, disk := range xl.getDisks() {
		read := openSingleTripHeader(t.Context(), disk, bucket, object)
		if read.rc != nil {
			read.rc.Close()
		}
		if read.err != nil || read.header.ErasureIndex != erasureIndex {
			continue
		}
		partPath := pathJoin(object, singleTripCurrentDir, "part.1")
		data, err := disk.ReadAll(t.Context(), bucket, partPath)
		if err != nil {
			t.Fatal(err)
		}
		hashSize := read.header.BitrotAlgo.New().Size()
		shardSize := int(read.header.toFileInfo().Erasure.ShardSize())
		corruptOffset := singleTripHeaderLen + hashSize + shardSize + hashSize
		if corruptOffset >= len(data) {
			t.Fatalf("shadow part too small for mid-stream corruption: len=%d offset=%d", len(data), corruptOffset)
		}
		data[corruptOffset] ^= 0xff
		if err = disk.WriteAll(t.Context(), bucket, partPath, data); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("data shard with erasure index %d not found", erasureIndex)
}

func resetSingleTripReadFileStreamCounts(disks []*singleTripCountingDisk) {
	for _, disk := range disks {
		disk.readFileStreamCalls.Store(0)
	}
}

func prepareSingleTripErasureObject(t *testing.T, ctx context.Context) (*erasureServerPools, func()) {
	t.Helper()

	obj, fsDirs, err := prepareErasure16(ctx)
	if err != nil {
		t.Fatal(err)
	}
	z := obj.(*erasureServerPools)
	return z, func() {
		obj.Shutdown(t.Context())
		removeRoots(fsDirs)
	}
}

func withSingleTripEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldEnabled := globalFastGetEnabled
	globalCompressConfigMu.Lock()
	oldCompressConfig := globalCompressConfig
	globalCompressConfig.Enabled = false
	globalCompressConfigMu.Unlock()
	oldAutoEncryption := globalAutoEncryption
	oldKMS := GlobalKMS
	oldStorageClass := globalStorageClass
	globalAutoEncryption = false
	GlobalKMS = nil
	defaultStorageClass, err := storageclass.LookupConfig(storageclass.DefaultKVS, 16)
	if err != nil {
		t.Fatal(err)
	}
	globalStorageClass.Update(defaultStorageClass)
	globalFastGetEnabled = enabled
	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	t.Cleanup(func() {
		globalFastGetEnabled = oldEnabled
		globalAutoEncryption = oldAutoEncryption
		GlobalKMS = oldKMS
		globalStorageClass.Update(oldStorageClass)
		globalCompressConfigMu.Lock()
		globalCompressConfig = oldCompressConfig
		globalCompressConfigMu.Unlock()
		fastGetHits.Store(0)
		fastGetFallbacks.Store(0)
	})
}

func singleTripCounterSnapshot() (hits, fallbacks uint64) {
	return fastGetHits.Load(), fastGetFallbacks.Load()
}

func assertSingleTripCounterDelta(t *testing.T, hitsBefore, fallbacksBefore, wantHits, wantFallbacks uint64, label string) {
	t.Helper()

	gotHits := fastGetHits.Load() - hitsBefore
	gotFallbacks := fastGetFallbacks.Load() - fallbacksBefore
	if gotHits != wantHits || gotFallbacks != wantFallbacks {
		t.Fatalf("%s counters delta = hits:%d fallbacks:%d, want hits:%d fallbacks:%d", label, gotHits, gotFallbacks, wantHits, wantFallbacks)
	}
}

func assertSingleTripObjectInfoEqual(t *testing.T, got, want ObjectInfo) {
	t.Helper()

	if got.Size != want.Size {
		t.Fatalf("size = %d, want %d", got.Size, want.Size)
	}
	if got.ETag != want.ETag {
		t.Fatalf("etag = %q, want %q", got.ETag, want.ETag)
	}
	if got.ContentType != want.ContentType {
		t.Fatalf("content-type = %q, want %q", got.ContentType, want.ContentType)
	}
	if got.CacheControl != want.CacheControl {
		t.Fatalf("cache-control = %q, want %q", got.CacheControl, want.CacheControl)
	}
	if !got.ModTime.Equal(want.ModTime) {
		t.Fatalf("modtime = %s, want %s", got.ModTime, want.ModTime)
	}
}

func assertSingleTripFastInfoAvailable(t *testing.T, xl *erasureObjects, bucket, object string) {
	t.Helper()

	info, ok := xl.openSingleTripFastInfo(t.Context(), bucket, object)
	if ok {
		closeBitrotReaders(info.readers)
		return
	}

	disks := xl.getDisks()
	var details []string
	for i, disk := range disks {
		read := openSingleTripHeader(t.Context(), disk, bucket, object)
		if read.rc != nil {
			read.rc.Close()
		}
		if read.err != nil {
			details = append(details, fmt.Sprintf("disk %d: %v", i, read.err))
			continue
		}
		details = append(details, fmt.Sprintf("disk %d: index=%d m=%d n=%d flags=%x sig=%x", i, read.header.ErasureIndex, read.header.ErasureM, read.header.ErasureN, read.header.Flags, read.header.DirectSig))
	}
	t.Fatalf("single-trip shadow headers did not form a fast quorum: %s", strings.Join(details, "; "))
}
