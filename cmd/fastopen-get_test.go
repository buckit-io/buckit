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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/buckit-io/buckit/internal/config/storageclass"
	"github.com/buckit-io/buckit/internal/crypto"
	"github.com/buckit-io/buckit/internal/etag"
	"github.com/buckit-io/buckit/internal/hash"
	xhttp "github.com/buckit-io/buckit/internal/http"
)

func TestFastOpenGETEndToEnd(t *testing.T) {
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
	withSingleTripEnabled(t, false)

	bucket := "bucket"
	object := "object"
	data := makeSingleTripTestData(smallFileThreshold*16, 23)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	userDefined := map[string]string{
		"content-type":  "application/fastopen-test",
		"cache-control": "max-age=120",
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{
		UserDefined: userDefined,
	}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("fastopen bytes differ from baseline: got %d bytes, want %d", len(fast), len(baseline))
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())

	resetFastOpenGETOpenCounts(countingDisks)
	rangeLen := smallFileThreshold*3 + 123
	gotRange, _ := readFastOpenTestObject(t, xl, bucket, object, &HTTPRangeSpec{Start: 0, End: int64(rangeLen - 1)})
	if !bytes.Equal(gotRange, data[:rangeLen]) {
		t.Fatalf("range bytes differ: got %d bytes, want %d", len(gotRange), rangeLen)
	}
	assertFastOpenGETOpens(t, xl, countingDisks, 0)
}

func TestFastOpenGETInlineEndToEnd(t *testing.T) {
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
	withSingleTripEnabled(t, false)

	bucket := "bucket"
	object := "inline-object"
	data := []byte("inline fastopen body")
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("fastopen inline bytes differ from baseline: got %q, want %q", fast, baseline)
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
}

func TestFastOpenGETTransformedFullObjectEndToEnd(t *testing.T) {
	tests := []struct {
		name       string
		init       func(t *testing.T)
		putObject  func(t *testing.T, ctx context.Context, xl *erasureObjects, bucket, object string, data []byte)
		verifyInfo func(t *testing.T, info ObjectInfo)
	}{
		{
			name:      "compressed",
			putObject: putCompressedFastOpenTestObject,
			verifyInfo: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				compressed, err := info.IsCompressedOK()
				if err != nil {
					t.Fatal(err)
				}
				if !compressed {
					t.Fatal("object is not compressed")
				}
			},
		},
		{
			name: "encrypted",
			init: func(t *testing.T) {
				enableEncryption(t)
			},
			putObject: putEncryptedFastOpenTestObject,
			verifyInfo: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				if _, encrypted := crypto.IsEncrypted(info.UserDefined); !encrypted {
					t.Fatal("object is not encrypted")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
			withSingleTripEnabled(t, false)
			if test.init != nil {
				test.init(t)
			}

			bucket := "bucket"
			object := "transformed-object-" + test.name
			data := bytes.Repeat([]byte("fastopen transformed object body\n"), 128*1024)
			if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			test.putObject(t, ctx, xl, bucket, object, data)

			baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
			test.verifyInfo(t, baselineInfo)
			countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

			globalFastGetEnabled = true
			fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
			if !bytes.Equal(fast, baseline) {
				t.Fatalf("fastopen %s bytes differ from baseline: got %d bytes, want %d", test.name, len(fast), len(baseline))
			}
			assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
			test.verifyInfo(t, fastInfo)
			assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
		})
	}
}

func putCompressedFastOpenTestObject(t *testing.T, ctx context.Context, xl *erasureObjects, bucket, object string, data []byte) {
	t.Helper()

	rc, idxCB := newS2CompressReader(bytes.NewReader(data), int64(len(data)), false)
	compressed, err := io.ReadAll(rc)
	if err != nil {
		rc.Close()
		t.Fatal(err)
	}
	if err = rc.Close(); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{
		ReservedMetadataPrefix + "compression": compressionAlgorithmV2,
		ReservedMetadataPrefix + "actual-size": strconv.FormatInt(int64(len(data)), 10),
	}
	_, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(compressed), int64(len(compressed)), "", ""), ObjectOptions{
		UserDefined: metadata,
		IndexCB:     idxCB,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func putEncryptedFastOpenTestObject(t *testing.T, ctx context.Context, xl *erasureObjects, bucket, object string, data []byte) {
	t.Helper()

	metadata := make(map[string]string)
	req := &http.Request{
		Header: http.Header{
			xhttp.AmzServerSideEncryption: []string{xhttp.AmzEncryptionAES},
		},
		ContentLength: int64(len(data)),
	}
	rawReader, err := hash.NewReader(ctx, bytes.NewReader(data), int64(len(data)), "", "", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	encReader, objectEncryptionKey, err := EncryptRequest(rawReader, req, bucket, object, metadata)
	if err != nil {
		t.Fatal(err)
	}
	encInfo := ObjectInfo{Size: int64(len(data))}
	wantSize := encInfo.EncryptedSize()
	encryptedReader, err := hash.NewReader(ctx, etag.Wrap(encReader, rawReader), wantSize, "", "", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	pReader, err := NewPutObjReader(rawReader).WithEncryption(encryptedReader, &objectEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = xl.PutObject(ctx, bucket, object, pReader, ObjectOptions{
		UserDefined: metadata,
		EncryptFn:   metadataEncrypter(objectEncryptionKey),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFastOpenGETNotFoundCountsAsHit(t *testing.T) {
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
	withSingleTripEnabled(t, false)

	bucket := "bucket"
	object := "missing-object"
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	gr, err := xl.GetObjectNInfo(t.Context(), bucket, object, nil, http.Header{}, ObjectOptions{FastGetObjInfo: true})
	if err == nil {
		if gr != nil {
			gr.Close()
		}
		t.Fatal("expected missing object error")
	}
	if !isErrObjectNotFound(err) {
		t.Fatalf("error = %T %v, want ObjectNotFound", err, err)
	}
	if gr != nil {
		t.Fatalf("reader = %#v, want nil", gr)
	}
	assertSingleTripCounterDelta(t, 0, 0, 1, 0, "fastopen not-found")
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
}

func TestFastOpenGETReplacementOnInitialOpenFailure(t *testing.T) {
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
	withSingleTripEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "replacement-object"
	data := makeSingleTripTestData(smallFileThreshold*16, 31)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	countingDisks[0].fastOpenErr = errFileNotFound

	globalFastGetEnabled = true
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("replacement bytes differ from baseline: got %d bytes, want %d", len(fast), len(baseline))
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETOpens(t, xl, countingDisks, len(countingDisks))
}

func TestFastOpenGETReplacementOnAlteredParity(t *testing.T) {
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
	withSingleTripEnabled(t, false)
	withFastOpenSpreadSelection(t, false)
	oldStorageClass := globalStorageClass
	globalStorageClass.Update(storageclass.Config{
		Standard: storageclass.StorageClass{Parity: 4},
		RRS:      storageclass.StorageClass{Parity: 2},
	})
	t.Cleanup(func() {
		globalStorageClass.Update(oldStorageClass)
	})

	bucket := "bucket"
	object := "altered-parity-object"
	data := makeSingleTripTestData(smallFileThreshold*16, 41)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{
		UserDefined: map[string]string{xhttp.AmzStorageClass: storageclass.RRS},
	}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("altered-parity replacement bytes differ from baseline: got %d bytes, want %d", len(fast), len(baseline))
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETOpens(t, xl, countingDisks, len(countingDisks))
}

func TestFastOpenGETReplacementOnBlockZeroCorrupt(t *testing.T) {
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
	withSingleTripEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "corrupt-replacement-object"
	data := makeSingleTripTestData(smallFileThreshold*16, 37)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	countingDisks[0].corruptBody = true

	globalFastGetEnabled = true
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("block-zero replacement bytes differ from baseline: got %d bytes, want %d, first diff at %d, offsets=%v", len(fast), len(baseline), firstByteDiff(fast, baseline), fastOpenGETOpenOffsets(countingDisks))
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETOpensLessThan(t, countingDisks, xl.fastOpenInitialOpenCount()+len(countingDisks))
}

func TestFastOpenGETLazyReplacementOnMidStreamCorrupt(t *testing.T) {
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
	withSingleTripEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "midstream-corrupt-replacement-object"
	data := makeSingleTripTestData(smallFileThreshold*16, 43)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	corruptAt := fastOpenTestEncodedShardOffset(t, xl, bucket, object, 1)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	countingDisks[0].corruptBody = true
	countingDisks[0].corruptBodyAt = corruptAt
	countingDisks[0].corruptBodyAtSet = true

	globalFastGetEnabled = true
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("midstream replacement bytes differ from baseline: got %d bytes, want %d, first diff at %d, offsets=%v", len(fast), len(baseline), firstByteDiff(fast, baseline), fastOpenGETOpenOffsets(countingDisks))
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETHasNonZeroOffset(t, countingDisks)
}

func TestFastOpenGETLazyReplacementOnConcurrentMidStreamCorrupt(t *testing.T) {
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
	withSingleTripEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "concurrent-midstream-corrupt-replacement-object"
	data := makeSingleTripTestData(smallFileThreshold*16, 47)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	corruptAt := fastOpenTestEncodedShardOffset(t, xl, bucket, object, 1)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	for _, diskIndex := range []int{0, 1} {
		countingDisks[diskIndex].corruptBody = true
		countingDisks[diskIndex].corruptBodyAt = corruptAt
		countingDisks[diskIndex].corruptBodyAtSet = true
	}

	globalFastGetEnabled = true
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("concurrent midstream replacement bytes differ from baseline: got %d bytes, want %d, first diff at %d, offsets=%v", len(fast), len(baseline), firstByteDiff(fast, baseline), fastOpenGETOpenOffsets(countingDisks))
	}
	assertSingleTripObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETNonZeroOffsetCount(t, countingDisks, 2)
}

func fastOpenTestEncodedShardOffset(t *testing.T, xl *erasureObjects, bucket, object string, block int64) int64 {
	t.Helper()

	fi, _, _, err := xl.getObjectFileInfo(t.Context(), bucket, object, ObjectOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	shardOffset := block * fi.Erasure.ShardSize()
	algo := fi.Erasure.GetChecksumInfo(1).Algorithm
	return (shardOffset/fi.Erasure.ShardSize())*int64(algo.New().Size()) + shardOffset
}

func firstByteDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

type fastOpenCountingDisk struct {
	StorageAPI
	opens            atomic.Int64
	mu               sync.Mutex
	offsets          []int64
	fastOpenErr      error
	corruptBody      bool
	corruptBodyAt    int64
	corruptBodyAtSet bool
}

func (d *fastOpenCountingDisk) FastOpenPart(ctx context.Context, volume, path string, req FastOpenPartRequest) (io.ReadCloser, error) {
	d.opens.Add(1)
	d.mu.Lock()
	d.offsets = append(d.offsets, req.Offset)
	d.mu.Unlock()
	if d.fastOpenErr != nil {
		return nil, d.fastOpenErr
	}
	rc, err := d.StorageAPI.FastOpenPart(ctx, volume, path, req)
	if err != nil || !d.corruptBody {
		return rc, err
	}
	_, frame, err := readFastOpenFrame(rc)
	if err != nil {
		rc.Close()
		return nil, err
	}
	frameBytes, err := encodeFastOpenFrame(frame)
	if err != nil {
		rc.Close()
		return nil, err
	}
	corruptAt := int64(0)
	if d.corruptBodyAtSet {
		corruptAt = d.corruptBodyAt
	}
	relativeCorruptAt := corruptAt - req.Offset
	if relativeCorruptAt < 0 {
		return &fastOpenTestReadCloser{
			r: io.MultiReader(bytes.NewReader(frameBytes), rc),
			c: rc,
		}, nil
	}
	return &fastOpenTestReadCloser{
		r: io.MultiReader(bytes.NewReader(frameBytes), &fastOpenCorruptAtReader{r: rc, offset: relativeCorruptAt}),
		c: rc,
	}, nil
}

func readFastOpenTestObject(t *testing.T, obj singleTripGetObjectNInfo, bucket, object string, rs *HTTPRangeSpec) ([]byte, ObjectInfo) {
	t.Helper()

	gr, err := obj.GetObjectNInfo(t.Context(), bucket, object, rs, http.Header{}, ObjectOptions{FastGetObjInfo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()

	var out bytes.Buffer
	if _, err = io.Copy(&out, gr); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), gr.ObjInfo
}

func wrapFastOpenCountingDisks(t *testing.T, sets *erasureSets, xl *erasureObjects) []*fastOpenCountingDisk {
	t.Helper()

	origDisks := xl.getDisks()
	countingDisks := make([]*fastOpenCountingDisk, len(origDisks))
	wrappedDisks := make([]StorageAPI, len(origDisks))
	for i, disk := range origDisks {
		countingDisks[i] = &fastOpenCountingDisk{StorageAPI: disk}
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
	return countingDisks
}

func withFastOpenSpreadSelection(t *testing.T, enabled bool) {
	t.Helper()

	old := globalFastGetSpreadSelection
	globalFastGetSpreadSelection = enabled
	t.Cleanup(func() {
		globalFastGetSpreadSelection = old
	})
}

func resetFastOpenGETOpenCounts(disks []*fastOpenCountingDisk) {
	for _, disk := range disks {
		disk.opens.Store(0)
		disk.mu.Lock()
		disk.offsets = nil
		disk.mu.Unlock()
	}
}

func fastOpenGETOpenOffsets(disks []*fastOpenCountingDisk) [][]int64 {
	out := make([][]int64, len(disks))
	for i, disk := range disks {
		disk.mu.Lock()
		out[i] = append([]int64(nil), disk.offsets...)
		disk.mu.Unlock()
	}
	return out
}

type fastOpenTestReadCloser struct {
	r io.Reader
	c io.Closer
}

func (r *fastOpenTestReadCloser) Read(p []byte) (int, error) { return r.r.Read(p) }

func (r *fastOpenTestReadCloser) Close() error { return r.c.Close() }

type fastOpenCorruptAtReader struct {
	r      io.Reader
	offset int64
	seen   int64
	done   bool
}

func (r *fastOpenCorruptAtReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && !r.done {
		start := r.seen
		end := r.seen + int64(n)
		if r.offset >= start && r.offset < end {
			p[r.offset-start] ^= 0xff
			r.done = true
		}
	}
	r.seen += int64(n)
	return n, err
}

func assertFastOpenGETOpens(t *testing.T, xl *erasureObjects, disks []*fastOpenCountingDisk, want int) {
	t.Helper()

	got := 0
	for _, disk := range disks {
		got += int(disk.opens.Load())
	}
	if got != want {
		t.Fatalf("FastOpenPart opens = %d, want %d", got, want)
	}
}

func assertFastOpenGETOpensLessThan(t *testing.T, disks []*fastOpenCountingDisk, limit int) {
	t.Helper()

	got := 0
	for _, disk := range disks {
		got += int(disk.opens.Load())
	}
	if got >= limit {
		t.Fatalf("FastOpenPart opens = %d, want less than %d", got, limit)
	}
}

func assertFastOpenGETHasNonZeroOffset(t *testing.T, disks []*fastOpenCountingDisk) {
	t.Helper()

	assertFastOpenGETNonZeroOffsetCount(t, disks, 1)
}

func assertFastOpenGETNonZeroOffsetCount(t *testing.T, disks []*fastOpenCountingDisk, want int) {
	t.Helper()

	got := 0
	for _, offsets := range fastOpenGETOpenOffsets(disks) {
		for _, offset := range offsets {
			if offset > 0 {
				got++
			}
		}
	}
	if got < want {
		t.Fatalf("FastOpenPart offsets = %v, got %d nonzero offsets, want at least %d", fastOpenGETOpenOffsets(disks), got, want)
	}
}
