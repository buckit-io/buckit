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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buckit-io/buckit/internal/config/storageclass"
)

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
	baseline, baselineInfo := readSingleTripTestObject(t, xl, bucket, object)
	if fastGetHits.Load() != 0 || fastGetFallbacks.Load() != 0 {
		t.Fatalf("baseline unexpectedly used fast path: hits=%d fallbacks=%d", fastGetHits.Load(), fastGetFallbacks.Load())
	}

	resetSingleTripReadFileStreamCounts(countingDisks)
	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	globalFastGetEnabled = true
	fast, fastInfo := readSingleTripTestObject(t, xl, bucket, object)

	if !bytes.Equal(fast, baseline) {
		t.Fatalf("fast bytes differ from baseline: got %d bytes, want %d", len(fast), len(baseline))
	}
	if fastGetHits.Load() != 1 || fastGetFallbacks.Load() != 0 {
		t.Fatalf("fast counters = hits:%d fallbacks:%d, want hits:1 fallbacks:0", fastGetHits.Load(), fastGetFallbacks.Load())
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	for i, disk := range countingDisks {
		if got := disk.readFileStreamCalls.Load(); got != 1 {
			t.Fatalf("disk %d ReadFileStream calls = %d, want 1", i, got)
		}
	}

	resetSingleTripReadFileStreamCounts(countingDisks)
	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	rangeLen := smallFileThreshold*3 + 123
	fastRange, _ := readSingleTripTestObjectRange(t, xl, bucket, object, &HTTPRangeSpec{Start: 0, End: int64(rangeLen - 1)})
	if !bytes.Equal(fastRange, data[:rangeLen]) {
		t.Fatalf("fast range bytes differ: got %d bytes, want %d", len(fastRange), rangeLen)
	}
	if fastGetHits.Load() != 1 || fastGetFallbacks.Load() != 0 {
		t.Fatalf("range counters = hits:%d fallbacks:%d, want hits:1 fallbacks:0", fastGetHits.Load(), fastGetFallbacks.Load())
	}
	for i, disk := range countingDisks {
		if got := disk.readFileStreamCalls.Load(); got != 1 {
			t.Fatalf("range disk %d ReadFileStream calls = %d, want 1", i, got)
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
	got, _ := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(got, data) {
		t.Fatalf("fallback bytes differ: got %d bytes, want %d", len(got), len(data))
	}
	if fastGetHits.Load() != 0 || fastGetFallbacks.Load() != 1 {
		t.Fatalf("fallback counters = hits:%d fallbacks:%d, want hits:0 fallbacks:1", fastGetHits.Load(), fastGetFallbacks.Load())
	}
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
	if got, _ := readSingleTripTestObject(t, xl, bucket, object); !bytes.Equal(got, dataA) {
		t.Fatal("initial fast GET did not return object A")
	}
	if fastGetHits.Load() != 1 {
		t.Fatalf("initial fast hits = %d, want 1", fastGetHits.Load())
	}

	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(dataB), int64(len(dataB)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readSingleTripTestObject(t, xl, bucket, object); !bytes.Equal(got, dataB) {
		t.Fatal("overwrite fast GET did not return object B")
	}
	if fastGetHits.Load() != 1 || fastGetFallbacks.Load() != 0 {
		t.Fatalf("overwrite counters = hits:%d fallbacks:%d, want hits:1 fallbacks:0", fastGetHits.Load(), fastGetFallbacks.Load())
	}

	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	if _, err := z.DeleteObject(ctx, bucket, object, ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	gr, err := xl.GetObjectNInfo(ctx, bucket, object, nil, http.Header{}, ObjectOptions{})
	if err == nil {
		gr.Close()
		t.Fatal("GET after delete unexpectedly succeeded")
	}
	if fastGetHits.Load() != 0 || fastGetFallbacks.Load() != 1 {
		t.Fatalf("delete counters = hits:%d fallbacks:%d, want hits:0 fallbacks:1", fastGetHits.Load(), fastGetFallbacks.Load())
	}
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

	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	if _, err := z.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(dataB), int64(len(dataB)), "", ""), ObjectOptions{
		UserDefined: map[string]string{
			"x-amz-meta-test": "forces-fallback",
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(got, dataB) {
		t.Fatal("eligible-to-ineligible overwrite did not return object B")
	}
	if fastGetHits.Load() != 0 || fastGetFallbacks.Load() != 1 {
		t.Fatalf("ineligible overwrite counters = hits:%d fallbacks:%d, want hits:0 fallbacks:1", fastGetHits.Load(), fastGetFallbacks.Load())
	}
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
	got, info := readSingleTripTestObject(t, xl, bucket, object)
	if !bytes.Equal(got, data) {
		t.Fatal("over-cap fallback did not return object bytes")
	}
	if info.ContentType != longContentType {
		t.Fatalf("content-type = len %d, want len %d", len(info.ContentType), len(longContentType))
	}
	if fastGetHits.Load() != 0 || fastGetFallbacks.Load() != 1 {
		t.Fatalf("over-cap counters = hits:%d fallbacks:%d, want hits:0 fallbacks:1", fastGetHits.Load(), fastGetFallbacks.Load())
	}
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
	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
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
	if fastGetHits.Load() != 1 || fastGetFallbacks.Load() != 0 {
		t.Fatalf("corruption counters = hits:%d fallbacks:%d, want hits:1 fallbacks:0", fastGetHits.Load(), fastGetFallbacks.Load())
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
