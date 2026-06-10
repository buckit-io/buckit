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
	"io"
	"net/http"
	"os"
	"slices"
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
	resetFastOpenMetrics()
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
		resetFastOpenMetrics()
	})
}

func resetFastOpenMetrics() {
	globalFastOpenMetrics.attempted.Store(0)
	globalFastOpenMetrics.hits.Store(0)
	globalFastOpenMetrics.unsupported.Store(0)
	globalFastOpenMetrics.replacementPath.Store(0)
	globalFastOpenMetrics.streamsOpened.Store(0)
	globalFastOpenMetrics.replacementOpen.Store(0)
	globalFastOpenMetrics.streamCancels.Store(0)
	for i := range globalFastOpenMetrics.failures {
		globalFastOpenMetrics.failures[i].Store(0)
	}
	for i := range globalFastOpenMetrics.finalErrors {
		globalFastOpenMetrics.finalErrors[i].Store(0)
	}
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
