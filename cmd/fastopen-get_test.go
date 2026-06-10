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
	"sync/atomic"
	"testing"
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

type fastOpenCountingDisk struct {
	StorageAPI
	opens atomic.Int64
}

func (d *fastOpenCountingDisk) FastOpenPart(ctx context.Context, volume, path string, req FastOpenPartRequest) (io.ReadCloser, error) {
	d.opens.Add(1)
	return d.StorageAPI.FastOpenPart(ctx, volume, path, req)
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

func resetFastOpenGETOpenCounts(disks []*fastOpenCountingDisk) {
	for _, disk := range disks {
		disk.opens.Store(0)
	}
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
