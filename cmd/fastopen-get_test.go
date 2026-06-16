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
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buckit-io/buckit/internal/bucket/lifecycle"
	"github.com/buckit-io/buckit/internal/bucket/replication"
	"github.com/buckit-io/buckit/internal/config/storageclass"
	"github.com/buckit-io/buckit/internal/crypto"
	"github.com/buckit-io/buckit/internal/etag"
	"github.com/buckit-io/buckit/internal/hash"
	xhttp "github.com/buckit-io/buckit/internal/http"
	"github.com/buckit-io/madmin-go/v3"
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	object := "object"
	data := makeFastOpenTestData(smallFileThreshold*16, 23)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	userDefined := map[string]string{
		"content-type":         "application/fastopen-test",
		"cache-control":        "max-age=120",
		xhttp.AmzObjectTagging: "tag1=value1&tag2=value2",
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{
		UserDefined: userDefined,
	}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	resetFastOpenMetrics()
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("fastopen bytes differ from baseline: got %d bytes, want %d", len(fast), len(baseline))
	}
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	if fastInfo.UserTags != baselineInfo.UserTags {
		t.Fatalf("tag parity differs\nfast: %#v\nwant: %#v", fastInfo, baselineInfo)
	}
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())

	resetFastOpenGETOpenCounts(countingDisks)
	rangeLen := smallFileThreshold*3 + 123
	gotRange, _ := readFastOpenTestObject(t, xl, bucket, object, &HTTPRangeSpec{Start: 0, End: int64(rangeLen - 1)})
	if !bytes.Equal(gotRange, data[:rangeLen]) {
		t.Fatalf("range bytes differ: got %d bytes, want %d", len(gotRange), rangeLen)
	}
	assertFastOpenGETOpens(t, xl, countingDisks, 0)
}

func TestFastOpenGETGoldenVersionedDeleteAndZero(t *testing.T) {
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	versionedObject := "versioned-object"
	zeroObject := "zero-object"
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	v1Data := makeFastOpenTestData(smallFileThreshold*16+12345, 11)
	v1, err := xl.PutObject(ctx, bucket, versionedObject, mustGetPutObjReader(t, bytes.NewReader(v1Data), int64(len(v1Data)), "", ""), ObjectOptions{Versioned: true})
	if err != nil {
		t.Fatal(err)
	}
	v2Data := makeFastOpenTestData(smallFileThreshold*16, 13)
	v2, err := xl.PutObject(ctx, bucket, versionedObject, mustGetPutObjReader(t, bytes.NewReader(v2Data), int64(len(v2Data)), "", ""), ObjectOptions{Versioned: true})
	if err != nil {
		t.Fatal(err)
	}
	deleteMarker, err := xl.DeleteObject(ctx, bucket, versionedObject, ObjectOptions{Versioned: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, zeroObject, mustGetPutObjReader(t, bytes.NewReader(nil), 0, "", ""), ObjectOptions{Versioned: true}); err != nil {
		t.Fatal(err)
	}

	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	for _, test := range []struct {
		name      string
		object    string
		opts      ObjectOptions
		wantBytes []byte
		wantErr   func(error) bool
	}{
		{
			name:      "explicit-v1",
			object:    versionedObject,
			opts:      ObjectOptions{Versioned: true, VersionID: v1.VersionID},
			wantBytes: v1Data,
		},
		{
			name:      "explicit-v2",
			object:    versionedObject,
			opts:      ObjectOptions{Versioned: true, VersionID: v2.VersionID},
			wantBytes: v2Data,
		},
		{
			name:    "latest-delete-marker",
			object:  versionedObject,
			opts:    ObjectOptions{Versioned: true},
			wantErr: isErrObjectNotFound,
		},
		{
			name:    "explicit-delete-marker",
			object:  versionedObject,
			opts:    ObjectOptions{Versioned: true, VersionID: deleteMarker.VersionID},
			wantErr: isErrMethodNotAllowed,
		},
		{
			name:      "zero-byte",
			object:    zeroObject,
			opts:      ObjectOptions{Versioned: true},
			wantBytes: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetFastOpenGETOpenCounts(countingDisks)
			baseline, baselineInfo, baselineErr := readFastOpenTestObjectOptions(t, xl, bucket, test.object, nil, http.Header{}, test.opts)

			globalFastGetEnabled = true
			resetFastOpenMetrics()
			fastOpts := test.opts
			fastOpts.FastGetObjInfo = true
			fast, fastInfo, fastErr := readFastOpenTestObjectOptions(t, xl, bucket, test.object, nil, http.Header{}, fastOpts)
			globalFastGetEnabled = false

			if test.wantErr != nil {
				if !test.wantErr(baselineErr) || !test.wantErr(fastErr) {
					t.Fatalf("errors = baseline:%T %v fast:%T %v", baselineErr, baselineErr, fastErr, fastErr)
				}
			} else if baselineErr != nil || fastErr != nil {
				t.Fatalf("errors = baseline:%v fast:%v", baselineErr, fastErr)
			}
			if !bytes.Equal(baseline, fast) || !bytes.Equal(fast, test.wantBytes) {
				t.Fatalf("bytes differ: baseline=%d fast=%d want=%d", len(baseline), len(fast), len(test.wantBytes))
			}
			assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
			if globalFastOpenMetrics.hits.Load() != 1 || globalFastOpenMetrics.unsupported.Load() != 0 {
				t.Fatalf("fast counters hits=%d fallbacks=%d, want 1/0", globalFastOpenMetrics.hits.Load(), globalFastOpenMetrics.unsupported.Load())
			}
			assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
		})
	}
}

func TestFastOpenGETMultipartFallsBack(t *testing.T) {
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	object := "multipart-object"
	part1 := makeFastOpenTestData(5*1024*1024+123, 19)
	part2 := makeFastOpenTestData(1024*1024+77, 29)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	upload, err := xl.NewMultipartUpload(ctx, bucket, object, ObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p1, err := xl.PutObjectPart(ctx, bucket, object, upload.UploadID, 1, mustGetPutObjReader(t, bytes.NewReader(part1), int64(len(part1)), "", ""), ObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := xl.PutObjectPart(ctx, bucket, object, upload.UploadID, 2, mustGetPutObjReader(t, bytes.NewReader(part2), int64(len(part2)), "", ""), ObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = xl.CompleteMultipartUpload(ctx, bucket, object, upload.UploadID, []CompletePart{
		{PartNumber: 1, ETag: p1.ETag},
		{PartNumber: 2, ETag: p2.ETag},
	}, ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	resetFastOpenMetrics()
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	globalFastGetEnabled = false
	if !bytes.Equal(fast, baseline) || !bytes.Equal(fast, append(append([]byte(nil), part1...), part2...)) {
		t.Fatalf("multipart bytes differ: fast=%d baseline=%d", len(fast), len(baseline))
	}
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	if globalFastOpenMetrics.hits.Load() != 0 || globalFastOpenMetrics.unsupported.Load() == 0 {
		t.Fatalf("fast counters hits=%d fallbacks=%d, want 0/>0", globalFastOpenMetrics.hits.Load(), globalFastOpenMetrics.unsupported.Load())
	}
	if got := globalFastOpenMetrics.unsupported.Load(); got != 1 {
		t.Fatalf("fastopen unsupported metric = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.streamCancels.Load(); got != 0 {
		t.Fatalf("fastopen stream cancels = %d, want 0 for metadata-only fallback", got)
	}
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
}

func TestFastOpenGETHandlerChecksumAndLifecycleHeaders(t *testing.T) {
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	object := "headers-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 61)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{
		Versioned:    true,
		WantChecksum: hash.NewChecksumFromData(hash.ChecksumCRC32, data),
	}); err != nil {
		t.Fatal(err)
	}

	router, accessKey, secretKey := initFastOpenGETAPIRouter(ctx, t, obj)
	lifecycleConfig := []byte(`<LifecycleConfiguration><Rule><ID>expire-fastopen</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`)
	if _, err = globalBucketMetadataSys.Update(ctx, bucket, bucketLifecycleConfig, lifecycleConfig); err != nil {
		t.Fatal(err)
	}
	lc, err := globalLifecycleSys.Get(bucket)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleInfo, err := xl.GetObjectInfo(ctx, bucket, object, ObjectOptions{Versioned: true})
	if err != nil {
		t.Fatal(err)
	}
	lifecycleRecorder := httptest.NewRecorder()
	lifecycleOpts := lifecycleInfo.ToLifecycleOpts()
	lc.SetPredictionHeaders(lifecycleRecorder, lifecycleOpts)
	if len(fastOpenHeaderValues(lifecycleRecorder.Header(), xhttp.AmzExpiration)) == 0 {
		t.Fatalf("lifecycle fixture produced no expiration header: opts=%#v rules=%d filtered=%d event=%#v", lifecycleOpts, len(lc.Rules), len(lc.FilterRules(lifecycleOpts)), lc.Eval(lifecycleOpts))
	}
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	headers := map[string]string{xhttp.AmzChecksumMode: "ENABLED"}

	globalFastGetEnabled = false
	baselineRec, baselineBody := doFastOpenGETHandlerRequest(t, router, bucket, object, accessKey, secretKey, headers)

	resetFastOpenGETOpenCounts(countingDisks)
	globalFastGetEnabled = true
	resetFastOpenMetrics()
	fastRec, fastBody := doFastOpenGETHandlerRequest(t, router, bucket, object, accessKey, secretKey, headers)
	globalFastGetEnabled = false

	if !bytes.Equal(fastBody, baselineBody) || !bytes.Equal(fastBody, data) {
		t.Fatalf("handler bytes differ: baseline=%d fast=%d want=%d", len(baselineBody), len(fastBody), len(data))
	}
	for _, header := range []string{xhttp.AmzChecksumCRC32, xhttp.AmzChecksumType, xhttp.AmzExpiration} {
		if len(fastOpenHeaderValues(baselineRec.Header(), header)) == 0 {
			t.Fatalf("baseline missing %s header", header)
		}
		if got, want := fastOpenHeaderValues(fastRec.Header(), header), fastOpenHeaderValues(baselineRec.Header(), header); !equalStringSlices(got, want) {
			t.Fatalf("%s header differs: fast=%v baseline=%v", header, got, want)
		}
	}
	if globalFastOpenMetrics.hits.Load() != 1 || globalFastOpenMetrics.unsupported.Load() != 0 {
		t.Fatalf("fast counters hits=%d fallbacks=%d, want 1/0", globalFastOpenMetrics.hits.Load(), globalFastOpenMetrics.unsupported.Load())
	}
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
}

func TestFastOpenGETRemoteTierWithBackend(t *testing.T) {
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	object := "transitioned-object"
	data := makeFastOpenTestData(smallFileThreshold*16+333, 67)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	info, err := xl.GetObjectInfo(ctx, bucket, object, ObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	backend := newFastOpenTestWarmBackend()
	installFastOpenTestWarmBackend(t, "WARM-TIER", backend)
	if err = xl.TransitionObject(ctx, bucket, object, ObjectOptions{
		MTime: info.ModTime,
		Transition: TransitionOptions{
			Tier: "WARM-TIER",
			ETag: info.ETag,
		},
	}); err != nil {
		t.Fatal(err)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	resetFastOpenMetrics()
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	globalFastGetEnabled = false

	if !bytes.Equal(fast, baseline) || !bytes.Equal(fast, data) {
		t.Fatalf("transitioned bytes differ: baseline=%d fast=%d want=%d", len(baseline), len(fast), len(data))
	}
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	if fastInfo.TransitionedObject.Status != lifecycle.TransitionComplete || fastInfo.TransitionedObject.Tier != "WARM-TIER" {
		t.Fatalf("transition info = %#v", fastInfo.TransitionedObject)
	}
	if backend.gets.Load() != 2 {
		t.Fatalf("warm backend GETs = %d, want 2", backend.gets.Load())
	}
	if globalFastOpenMetrics.hits.Load() != 1 || globalFastOpenMetrics.unsupported.Load() != 0 {
		t.Fatalf("fast counters hits=%d fallbacks=%d, want 1/0", globalFastOpenMetrics.hits.Load(), globalFastOpenMetrics.unsupported.Load())
	}
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
}

func TestFastOpenGETReplicationConfiguredMetadata(t *testing.T) {
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	object := "replicated-object"
	purgeObject := "purged-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 71)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	arn := "arn:minio:replication:::target"
	installFastOpenReplicationConfig(ctx, t, bucket, arn)
	dsc := ReplicateDecision{}
	dsc.Set(newReplicateTargetDecision(arn, true, false))

	putOpts := ObjectOptions{
		Versioned: true,
		UserDefined: map[string]string{
			ReservedMetadataPrefixLower + ReplicationStatus: dsc.PendingStatus(),
		},
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), putOpts); err != nil {
		t.Fatal(err)
	}

	purgeVersion, err := xl.PutObject(ctx, bucket, purgeObject, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{Versioned: true})
	if err != nil {
		t.Fatal(err)
	}
	deleteOpts := ObjectOptions{Versioned: true, VersionID: purgeVersion.VersionID}
	deleteOpts.SetDeleteReplicationState(dsc, purgeVersion.VersionID)
	if _, err = xl.DeleteObject(ctx, bucket, purgeObject, deleteOpts); err != nil {
		t.Fatal(err)
	}

	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	for _, test := range []struct {
		name    string
		object  string
		opts    ObjectOptions
		wantErr func(error) bool
		verify  func(t *testing.T, info ObjectInfo)
	}{
		{
			name:   "replication-status",
			object: object,
			opts:   ObjectOptions{Versioned: true},
			verify: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				if info.ReplicationStatus != replication.Pending {
					t.Fatalf("replication status = %q, want %q", info.ReplicationStatus, replication.Pending)
				}
				if info.ReplicationStatusInternal == "" {
					t.Fatal("missing internal replication status")
				}
			},
		},
		{
			name:    "version-purge",
			object:  purgeObject,
			opts:    ObjectOptions{Versioned: true, VersionID: purgeVersion.VersionID},
			wantErr: isErrMethodNotAllowed,
			verify: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				if info.VersionPurgeStatus != replication.VersionPurgePending {
					t.Fatalf("version purge status = %q, want %q", info.VersionPurgeStatus, replication.VersionPurgePending)
				}
				if info.VersionPurgeStatusInternal == "" {
					t.Fatal("missing internal version purge status")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetFastOpenGETOpenCounts(countingDisks)
			baseline, baselineInfo, baselineErr := readFastOpenTestObjectOptions(t, xl, bucket, test.object, nil, http.Header{}, test.opts)

			globalFastGetEnabled = true
			resetFastOpenMetrics()
			fastOpts := test.opts
			fastOpts.FastGetObjInfo = true
			fast, fastInfo, fastErr := readFastOpenTestObjectOptions(t, xl, bucket, test.object, nil, http.Header{}, fastOpts)
			globalFastGetEnabled = false

			if test.wantErr != nil {
				if !test.wantErr(baselineErr) || !test.wantErr(fastErr) {
					t.Fatalf("errors = baseline:%T %v fast:%T %v", baselineErr, baselineErr, fastErr, fastErr)
				}
			} else if baselineErr != nil || fastErr != nil {
				t.Fatalf("errors = baseline:%v fast:%v", baselineErr, fastErr)
			}
			if test.wantErr == nil && (!bytes.Equal(fast, baseline) || !bytes.Equal(fast, data)) {
				t.Fatalf("bytes differ: baseline=%d fast=%d want=%d", len(baseline), len(fast), len(data))
			}
			assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
			test.verify(t, fastInfo)
			if globalFastOpenMetrics.hits.Load() != 1 || globalFastOpenMetrics.unsupported.Load() != 0 {
				t.Fatalf("fast counters hits=%d fallbacks=%d, want 1/0", globalFastOpenMetrics.hits.Load(), globalFastOpenMetrics.unsupported.Load())
			}
			assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
		})
	}
}

func TestFastOpenGETScopeGates(t *testing.T) {
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	object := "scope-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 17)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	globalFastGetEnabled = true
	for _, test := range []struct {
		name string
		rs   *HTTPRangeSpec
		h    http.Header
		opts ObjectOptions
	}{
		{name: "fastget-objinfo-unset", opts: ObjectOptions{}},
		{name: "range", rs: &HTTPRangeSpec{Start: 0, End: 15}, opts: ObjectOptions{FastGetObjInfo: true}},
		{name: "part-number", opts: ObjectOptions{FastGetObjInfo: true, PartNumber: 1}},
		{name: "sse-c", h: http.Header{xhttp.AmzServerSideEncryptionCustomerAlgorithm: []string{xhttp.AmzEncryptionAES}}, opts: ObjectOptions{FastGetObjInfo: true}},
		{name: "replication", opts: ObjectOptions{FastGetObjInfo: true, ReplicationRequest: true}},
		{name: "proxy", opts: ObjectOptions{FastGetObjInfo: true, ProxyRequest: true}},
		{name: "proxy-header-set", opts: ObjectOptions{FastGetObjInfo: true, ProxyHeaderSet: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetFastOpenGETOpenCounts(countingDisks)
			gr, err := xl.GetObjectNInfo(t.Context(), bucket, object, test.rs, test.h, test.opts)
			if gr != nil {
				gr.Close()
			}
			if test.name != "sse-c" && err != nil {
				t.Fatalf("GetObjectNInfo error = %v", err)
			}
			assertFastOpenGETOpens(t, xl, countingDisks, 0)
		})
	}
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
	withFastOpenEnabled(t, false)

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
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
}

func TestFastOpenGETTransformedFullObjectEndToEnd(t *testing.T) {
	tests := []struct {
		name       string
		init       func(t *testing.T)
		putObject  func(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte)
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
			withFastOpenEnabled(t, false)
			if test.init != nil {
				test.init(t)
			}

			bucket := "bucket"
			object := "transformed-object-" + test.name
			data := bytes.Repeat([]byte("fastopen transformed object body\n"), 128*1024)
			if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			test.putObject(ctx, t, xl, bucket, object, data)

			baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
			test.verifyInfo(t, baselineInfo)
			countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

			globalFastGetEnabled = true
			fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
			if !bytes.Equal(fast, baseline) {
				t.Fatalf("fastopen %s bytes differ from baseline: got %d bytes, want %d", test.name, len(fast), len(baseline))
			}
			assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
			test.verifyInfo(t, fastInfo)
			assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
		})
	}
}

func TestFastOpenGETAdditionalGoldenMetadata(t *testing.T) {
	tests := []struct {
		name       string
		init       func(t *testing.T)
		opts       ObjectOptions
		putObject  func(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte)
		verifyInfo func(t *testing.T, info ObjectInfo)
	}{
		{
			name: "version-suspended-null",
			opts: ObjectOptions{VersionSuspended: true},
			putObject: func(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte) {
				t.Helper()
				if _, err := xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{VersionSuspended: true}); err != nil {
					t.Fatal(err)
				}
			},
			verifyInfo: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				if info.VersionID != nullVersionID || !info.IsLatest {
					t.Fatalf("version-suspended info = version %q latest=%v, want %q/latest", info.VersionID, info.IsLatest, nullVersionID)
				}
			},
		},
		{
			name: "restored-on-disk",
			putObject: func(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte) {
				t.Helper()
				if _, err := xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
					t.Fatal(err)
				}
				restoreExpiry := UTCNow().Add(24 * time.Hour)
				mutateFastOpenObjectFileInfo(t, xl, bucket, object, func(fi *FileInfo) {
					fi.TransitionStatus = lifecycle.TransitionComplete
					fi.TransitionedObjName = "remote-object"
					fi.TransitionTier = "WARM-TIER"
					fi.TransitionVersionID = "remote-version"
					if fi.Metadata == nil {
						fi.Metadata = make(map[string]string)
					}
					fi.Metadata[xhttp.AmzRestore] = completedRestoreObj(restoreExpiry).String()
				})
			},
			verifyInfo: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				if info.IsRemote() {
					t.Fatal("restored object still reports remote")
				}
				if info.TransitionedObject.Status != lifecycle.TransitionComplete || info.TransitionedObject.Tier != "WARM-TIER" {
					t.Fatalf("transition info = %#v", info.TransitionedObject)
				}
				if info.RestoreOngoing || info.RestoreExpires.IsZero() {
					t.Fatalf("restore info = ongoing:%v expires:%v", info.RestoreOngoing, info.RestoreExpires)
				}
			},
		},
		{
			name: "object-lock-metadata",
			putObject: func(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte) {
				t.Helper()
				retainUntil := UTCNow().Add(24 * time.Hour).Format(time.RFC3339)
				metadata := map[string]string{
					strings.ToLower(xhttp.AmzObjectLockMode):            "GOVERNANCE",
					strings.ToLower(xhttp.AmzObjectLockRetainUntilDate): retainUntil,
					strings.ToLower(xhttp.AmzObjectLockLegalHold):       "ON",
				}
				if _, err := xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{UserDefined: metadata}); err != nil {
					t.Fatal(err)
				}
			},
			verifyInfo: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				if info.UserDefined[strings.ToLower(xhttp.AmzObjectLockMode)] != "GOVERNANCE" ||
					info.UserDefined[strings.ToLower(xhttp.AmzObjectLockLegalHold)] != "ON" ||
					info.UserDefined[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] == "" {
					t.Fatalf("object-lock metadata = %#v", info.UserDefined)
				}
			},
		},
		{
			name: "sse-kms",
			init: func(t *testing.T) {
				enableEncryption(t)
			},
			putObject: putKMSFastOpenTestObject,
			verifyInfo: func(t *testing.T, info ObjectInfo) {
				t.Helper()
				if !crypto.S3KMS.IsEncrypted(info.UserDefined) {
					t.Fatalf("object is not SSE-KMS encrypted: %#v", info.UserDefined)
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
			withFastOpenEnabled(t, false)
			if test.init != nil {
				test.init(t)
			}

			bucket := "bucket"
			object := "additional-golden-" + test.name
			data := makeFastOpenTestData(smallFileThreshold*16+12345, 61)
			if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			test.putObject(ctx, t, xl, bucket, object, data)

			baseline, baselineInfo, baselineErr := readFastOpenTestObjectOptions(t, xl, bucket, object, nil, http.Header{}, test.opts)
			if baselineErr != nil {
				t.Fatal(baselineErr)
			}
			test.verifyInfo(t, baselineInfo)
			countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

			globalFastGetEnabled = true
			resetFastOpenMetrics()
			fastOpts := test.opts
			fastOpts.FastGetObjInfo = true
			fast, fastInfo, fastErr := readFastOpenTestObjectOptions(t, xl, bucket, object, nil, http.Header{}, fastOpts)
			globalFastGetEnabled = false
			if fastErr != nil {
				t.Fatal(fastErr)
			}
			if !bytes.Equal(fast, baseline) {
				t.Fatalf("%s bytes differ from baseline: got %d bytes, want %d", test.name, len(fast), len(baseline))
			}
			assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
			test.verifyInfo(t, fastInfo)
			if globalFastOpenMetrics.hits.Load() != 1 || globalFastOpenMetrics.unsupported.Load() != 0 {
				t.Fatalf("fast counters hits=%d fallbacks=%d, want 1/0", globalFastOpenMetrics.hits.Load(), globalFastOpenMetrics.unsupported.Load())
			}
			assertFastOpenGETOpens(t, xl, countingDisks, xl.fastOpenInitialOpenCount())
		})
	}
}

func putCompressedFastOpenTestObject(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte) {
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

func putEncryptedFastOpenTestObject(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte) {
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

func putKMSFastOpenTestObject(ctx context.Context, t *testing.T, xl *erasureObjects, bucket, object string, data []byte) {
	t.Helper()

	metadata := make(map[string]string)
	req := &http.Request{
		Header: http.Header{
			xhttp.AmzServerSideEncryption:      []string{xhttp.AmzEncryptionKMS},
			xhttp.AmzServerSideEncryptionKmsID: []string{"my-minio-key"},
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

func mutateFastOpenObjectFileInfo(t *testing.T, xl *erasureObjects, bucket, object string, mutate func(*FileInfo)) {
	t.Helper()

	disks := xl.getDisks()
	metaArr, errs := readAllXL(t.Context(), disks, bucket, object, false, false)
	for i, err := range errs {
		if err != nil || disks[i] == nil {
			continue
		}
		fi := metaArr[i]
		if !fi.IsValid() {
			continue
		}
		mutate(&fi)
		if err = disks[i].WriteMetadata(t.Context(), "", bucket, object, fi); err != nil {
			t.Fatal(err)
		}
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
	withFastOpenEnabled(t, false)

	bucket := "bucket"
	object := "missing-object"
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)

	globalFastGetEnabled = true
	resetFastOpenMetrics()
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
	if got := globalFastOpenMetrics.finalErrors[fastOpenFinalErrorNotFound].Load(); got != 1 {
		t.Fatalf("not-found final error metric = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.streamCancels.Load(); got != 0 {
		t.Fatalf("fastopen stream cancels = %d, want 0 for metadata-only not-found", got)
	}
	assertFastOpenCounterDelta(t, 0, 0, 1, 0, "fastopen not-found")
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
	withFastOpenEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "replacement-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 31)
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
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	if got := globalFastOpenMetrics.replacementPath.Load(); got != 1 {
		t.Fatalf("replacement path metric = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.failures[fastOpenFailureNoQuorum].Load(); got == 0 {
		t.Fatal("no-quorum failure metric = 0, want nonzero")
	}
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
	withFastOpenEnabled(t, false)
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
	data := makeFastOpenTestData(smallFileThreshold*16, 41)
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
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
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
	withFastOpenEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "corrupt-replacement-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 37)
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
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETOpensLessThan(t, countingDisks, xl.fastOpenInitialOpenCount()+len(countingDisks))
}

func TestFastOpenGETInlineReplacementOnBlockZeroCorrupt(t *testing.T) {
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
	withFastOpenEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "inline-corrupt-replacement-object"
	data := makeFastOpenTestData(smallFileThreshold, 38)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	fi, _, _, err := xl.getObjectFileInfo(ctx, bucket, object, ObjectOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if shardFileSize, shardSize := fi.Erasure.ShardFileSize(fi.Parts[0].Size), fi.Erasure.ShardSize(); shardFileSize > shardSize {
		t.Fatalf("test object is not inline-replacement safe: shard_file_size=%d shard_size=%d", shardFileSize, shardSize)
	}

	baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	for _, diskIndex := range []int{0, 1} {
		countingDisks[diskIndex].corruptBody = true
	}

	globalFastGetEnabled = true
	fast, fastInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
	if !bytes.Equal(fast, baseline) {
		t.Fatalf("inline block-zero replacement bytes differ from baseline: got %d bytes, want %d, first diff at %d, offsets=%v", len(fast), len(baseline), firstByteDiff(fast, baseline), fastOpenGETOpenOffsets(countingDisks))
	}
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	if !fastOpenGETSawBodyMode(countingDisks[0], FastOpenBodyInline) || !fastOpenGETSawBodyMode(countingDisks[1], FastOpenBodyInline) {
		t.Fatalf("corrupted selected disks did not serve inline bodies, modes[0]=%v modes[1]=%v", fastOpenGETBodyModes(countingDisks[0]), fastOpenGETBodyModes(countingDisks[1]))
	}
	if got, initial := fastOpenGETOpenCount(countingDisks), xl.fastOpenInitialOpenCount(); got <= initial {
		t.Fatalf("FastOpenPart opens = %d, want more than initial open count %d for inline replacement, offsets=%v", got, initial, fastOpenGETOpenOffsets(countingDisks))
	}
}

func TestFastOpenGETUnrecoverableBlockZeroCorruptReturnsReaderThenReadError(t *testing.T) {
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
	withFastOpenEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "unrecoverable-block-zero-corrupt-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 39)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	for _, diskIndex := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8} {
		countingDisks[diskIndex].corruptBody = true
	}

	globalFastGetEnabled = true
	gr, err := xl.GetObjectNInfo(ctx, bucket, object, nil, http.Header{}, ObjectOptions{FastGetObjInfo: true})
	if err != nil {
		t.Fatalf("GetObjectNInfo error = %v, want reader returned before block-0 decode", err)
	}
	if gr == nil {
		t.Fatal("GetObjectNInfo returned nil reader")
	}
	defer gr.Close()

	var out bytes.Buffer
	_, err = io.Copy(&out, gr)
	if !isErrReadQuorum(err) {
		t.Fatalf("read error = %T %v, want read quorum, offsets=%v", err, err, fastOpenGETOpenOffsets(countingDisks))
	}
	if out.Len() != 0 {
		t.Fatalf("bytes written before unrecoverable block-0 error = %d, want 0", out.Len())
	}
	if got := globalFastOpenMetrics.hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.finalErrors[fastOpenFinalErrorReadQuorum].Load(); got != 1 {
		t.Fatalf("read-quorum final errors = %d, want 1", got)
	}
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
	withFastOpenEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "midstream-corrupt-replacement-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 43)
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
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
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
	withFastOpenEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "concurrent-midstream-corrupt-replacement-object"
	data := makeFastOpenTestData(smallFileThreshold*16, 47)
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
	assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
	assertFastOpenGETNonZeroOffsetCount(t, countingDisks, 2)
}

func TestFastOpenGETMetrics(t *testing.T) {
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
	withFastOpenEnabled(t, false)
	withFastOpenSpreadSelection(t, false)

	bucket := "bucket"
	object := "metrics-object"
	data := makeFastOpenTestData(int(3*blockSizeV2+12345), 79)
	if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	corruptAt := fastOpenTestEncodedShardOffset(t, xl, bucket, object, 1)
	countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
	countingDisks[0].corruptBody = true
	countingDisks[0].corruptBodyAt = corruptAt
	countingDisks[0].corruptBodyAtSet = true

	globalFastGetEnabled = true
	fast, _, fastErr := readFastOpenTestObjectOptions(t, xl, bucket, object, nil, http.Header{}, ObjectOptions{FastGetObjInfo: true})
	if fastErr != nil {
		t.Fatal(fastErr)
	}
	if !bytes.Equal(fast, data) {
		t.Fatal("fastopen metrics object bytes differ")
	}
	if got := globalFastOpenMetrics.attempted.Load(); got != 1 {
		t.Fatalf("attempted = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.unsupported.Load(); got != 0 {
		t.Fatalf("unsupported = %d, want 0", got)
	}
	if got := globalFastOpenMetrics.tryCount.Load(); got != 1 {
		t.Fatalf("try count = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.tryNS.Load(); got == 0 {
		t.Fatal("try duration = 0, want nonzero")
	}
	if got := globalFastOpenMetrics.openInfoCount.Load(); got != 1 {
		t.Fatalf("open info count = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.openInfoNS.Load(); got == 0 {
		t.Fatal("open info duration = 0, want nonzero")
	}
	if got := globalFastOpenMetrics.bodyDecodeCount.Load(); got != 1 {
		t.Fatalf("body decode count = %d, want 1", got)
	}
	if got := globalFastOpenMetrics.bodyDecodeNS.Load(); got == 0 {
		t.Fatal("body decode duration = 0, want nonzero")
	}
	if got := globalFastOpenMetrics.replacementOpen.Load(); got == 0 {
		t.Fatal("replacement opens = 0, want nonzero")
	}
	if got := globalFastOpenMetrics.streamsOpened.Load(); got <= uint64(xl.fastOpenInitialOpenCount()) {
		t.Fatalf("streams opened = %d, want more than initial open count", got)
	}
	streamsOpened := globalFastOpenMetrics.streamsOpened.Load()
	streamCancels := globalFastOpenMetrics.streamCancels.Load()
	if streamCancels == 0 {
		t.Fatal("stream cancels = 0, want nonzero")
	}
	if streamCancels >= streamsOpened {
		t.Fatalf("stream cancels = %d, streams opened = %d; cancels should count early closes only", streamCancels, streamsOpened)
	}
}

func TestFastOpenGETLazyReplacementHardening(t *testing.T) {
	tests := []struct {
		name          string
		size          int
		corruptBlock  int64
		corruptDisks  []int
		wantErr       func(error) bool
		wantFinalErr  fastOpenFinalErrorCategory
		wantFinalErrN uint64
		wantNonZero   int
		wantOpenLimit func(xl *erasureObjects, disks []*fastOpenCountingDisk) int
	}{
		{
			name:         "non-block-aligned-object",
			size:         int(3*blockSizeV2 + 12345),
			corruptBlock: 1,
			corruptDisks: []int{0},
			wantNonZero:  1,
		},
		{
			name:         "multi-block-continuation",
			size:         int(4*blockSizeV2 + 12345),
			corruptBlock: 1,
			corruptDisks: []int{0},
			wantNonZero:  1,
			wantOpenLimit: func(xl *erasureObjects, disks []*fastOpenCountingDisk) int {
				return xl.fastOpenInitialOpenCount() + 2
			},
		},
		{
			name:          "post-commit-exhaustion",
			size:          int(3 * blockSizeV2),
			corruptBlock:  1,
			corruptDisks:  []int{0, 1, 2, 3, 4, 5, 6, 7, 8},
			wantErr:       isErrReadQuorum,
			wantFinalErr:  fastOpenFinalErrorReadQuorum,
			wantFinalErrN: 1,
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
			withFastOpenEnabled(t, false)
			withFastOpenSpreadSelection(t, false)

			bucket := "bucket"
			object := "lazy-hardening-" + test.name
			data := makeFastOpenTestData(test.size, 53)
			if err = obj.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err = xl.PutObject(ctx, bucket, object, mustGetPutObjReader(t, bytes.NewReader(data), int64(len(data)), "", ""), ObjectOptions{}); err != nil {
				t.Fatal(err)
			}

			baseline, baselineInfo := readFastOpenTestObject(t, xl, bucket, object, nil)
			corruptAt := fastOpenTestEncodedShardOffset(t, xl, bucket, object, test.corruptBlock)
			countingDisks := wrapFastOpenCountingDisks(t, sets, xl)
			for _, diskIndex := range test.corruptDisks {
				countingDisks[diskIndex].corruptBody = true
				countingDisks[diskIndex].corruptBodyAt = corruptAt
				countingDisks[diskIndex].corruptBodyAtSet = true
			}

			globalFastGetEnabled = true
			fast, fastInfo, fastErr := readFastOpenTestObjectOptions(t, xl, bucket, object, nil, http.Header{}, ObjectOptions{FastGetObjInfo: true})
			if test.wantErr != nil {
				if !test.wantErr(fastErr) {
					t.Fatalf("error = %T %v, want expected error, offsets=%v", fastErr, fastErr, fastOpenGETOpenOffsets(countingDisks))
				}
				if got := globalFastOpenMetrics.finalErrors[test.wantFinalErr].Load(); got != test.wantFinalErrN {
					t.Fatalf("final error metric[%s] = %d, want %d", test.wantFinalErr, got, test.wantFinalErrN)
				}
				return
			}
			if fastErr != nil {
				t.Fatal(fastErr)
			}
			if !bytes.Equal(fast, baseline) {
				t.Fatalf("%s bytes differ from baseline: got %d bytes, want %d, first diff at %d, offsets=%v", test.name, len(fast), len(baseline), firstByteDiff(fast, baseline), fastOpenGETOpenOffsets(countingDisks))
			}
			assertFastOpenGETObjectInfoEqual(t, fastInfo, baselineInfo)
			assertFastOpenGETNonZeroOffsetCount(t, countingDisks, test.wantNonZero)
			if test.wantOpenLimit != nil {
				assertFastOpenGETOpensLessThan(t, countingDisks, test.wantOpenLimit(xl, countingDisks))
			}
		})
	}
}

func TestFastOpenLazyReplacementRejectsMismatchedFrame(t *testing.T) {
	fi := testFastOpenFileInfo()
	pool := newFastOpenReplacementPool(t.Context(), nil, nil, fi.Volume, fi.Name, fi.VersionID, fi, HighwayHash256S)
	newFrame := func(t *testing.T) CoalescedMetadataFrame {
		t.Helper()
		meta, err := fileInfoToFastOpenGETMeta(fi)
		if err != nil {
			t.Fatal(err)
		}
		return CoalescedMetadataFrame{
			Status:   FastOpenStatusOK,
			Meta:     meta,
			BodyMode: FastOpenBodyShard,
			BodyLen:  pool.bodyLen,
		}
	}
	if err := pool.validateFrame(fi.Erasure.Index-1, 0, newFrame(t)); err != nil {
		t.Fatalf("valid frame rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*CoalescedMetadataFrame)
	}{
		{
			name: "wrong-version",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Meta.VersionID = "other-version"
			},
		},
		{
			name: "wrong-index",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Meta.Erasure.Index++
			},
		},
		{
			name: "wrong-modtime",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Meta.ModTimeUnixNano++
			},
		},
		{
			name: "wrong-body-length",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.BodyLen--
			},
		},
		{
			name: "wrong-distribution",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Meta.Erasure.Distribution[0], frame.Meta.Erasure.Distribution[1] = frame.Meta.Erasure.Distribution[1], frame.Meta.Erasure.Distribution[0]
			},
		},
		{
			name: "wrong-bitrot-algorithm",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Meta.Erasure.Bitrot.PartNumber = 1
				frame.Meta.Erasure.Bitrot.Algorithm = 4
				frame.Meta.Erasure.Bitrot.Hash = []byte("legacy-hash")
			},
		},
		{
			name: "not-ok-status",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Status = FastOpenStatusUnsupported
			},
		},
		{
			name: "not-shard-body",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.BodyMode = FastOpenBodyMetadataOnly
			},
		},
		{
			name: "wrong-size",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Meta.Size++
			},
		},
		{
			name: "wrong-part-size",
			mutate: func(frame *CoalescedMetadataFrame) {
				frame.Meta.Part.Size++
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := newFrame(t)
			test.mutate(&bad)
			if err := pool.validateFrame(fi.Erasure.Index-1, 0, bad); !errors.Is(err, errFileCorrupt) {
				t.Fatalf("validateFrame error = %v, want %v", err, errFileCorrupt)
			}
		})
	}
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
	bodyModes        []FastOpenBodyMode
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
	d.mu.Lock()
	d.bodyModes = append(d.bodyModes, frame.BodyMode)
	d.mu.Unlock()
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

type fastOpenGetObjectNInfo interface {
	GetObjectNInfo(ctx context.Context, bucket, object string, rs *HTTPRangeSpec, h http.Header, opts ObjectOptions) (*GetObjectReader, error)
}

func readFastOpenTestObject(t *testing.T, obj fastOpenGetObjectNInfo, bucket, object string, rs *HTTPRangeSpec) ([]byte, ObjectInfo) {
	t.Helper()

	out, info, err := readFastOpenTestObjectOptions(t, obj, bucket, object, rs, http.Header{}, ObjectOptions{FastGetObjInfo: true})
	if err != nil {
		t.Fatal(err)
	}
	return out, info
}

func readFastOpenTestObjectOptions(t *testing.T, obj fastOpenGetObjectNInfo, bucket, object string, rs *HTTPRangeSpec, h http.Header, opts ObjectOptions) ([]byte, ObjectInfo, error) {
	t.Helper()

	gr, err := obj.GetObjectNInfo(t.Context(), bucket, object, rs, h, opts)
	if gr == nil {
		return nil, ObjectInfo{}, err
	}
	defer gr.Close()

	var out bytes.Buffer
	if err == nil {
		_, err = io.Copy(&out, gr)
	}
	return out.Bytes(), gr.ObjInfo, err
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
		disk.bodyModes = nil
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

func fastOpenGETBodyModes(disk *fastOpenCountingDisk) []FastOpenBodyMode {
	disk.mu.Lock()
	defer disk.mu.Unlock()
	return append([]FastOpenBodyMode(nil), disk.bodyModes...)
}

func fastOpenGETSawBodyMode(disk *fastOpenCountingDisk, want FastOpenBodyMode) bool {
	for _, got := range fastOpenGETBodyModes(disk) {
		if got == want {
			return true
		}
	}
	return false
}

func initFastOpenGETAPIRouter(ctx context.Context, t *testing.T, obj ObjectLayer) (http.Handler, string, string) {
	t.Helper()

	oldObjectLayer := newObjectLayerFn()
	setObjectLayer(obj)
	t.Cleanup(func() {
		setObjectLayer(oldObjectLayer)
	})

	initConfigSubsystem(ctx, obj)
	globalIAMSys.Init(ctx, obj, globalEtcdClient, 2*time.Second)
	if err := newTestConfig(globalMinioDefaultRegion, obj); err != nil {
		t.Fatal(err)
	}

	router := initTestAPIEndPoints(obj, []string{"GetObject"})
	return router, globalActiveCred.AccessKey, globalActiveCred.SecretKey
}

func doFastOpenGETHandlerRequest(t *testing.T, router http.Handler, bucket, object, accessKey, secretKey string, headers map[string]string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()

	req, err := newTestSignedRequestV4(http.MethodGet, getGetObjectURL("", bucket, object), 0, nil, accessKey, secretKey, headers)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", rec.Code, string(body))
	}
	return rec, body
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fastOpenHeaderValues(h http.Header, key string) []string {
	if values := h.Values(key); len(values) > 0 {
		return values
	}
	return h[key]
}

type fastOpenTestWarmBackend struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    atomic.Int64
}

func newFastOpenTestWarmBackend() *fastOpenTestWarmBackend {
	return &fastOpenTestWarmBackend{objects: make(map[string][]byte)}
}

func (b *fastOpenTestWarmBackend) Put(ctx context.Context, object string, r io.Reader, length int64) (remoteVersionID, error) {
	return b.PutWithMeta(ctx, object, r, length, nil)
}

func (b *fastOpenTestWarmBackend) PutWithMeta(ctx context.Context, object string, r io.Reader, length int64, meta map[string]string) (remoteVersionID, error) {
	data, err := io.ReadAll(io.LimitReader(r, length))
	if err != nil {
		return "", err
	}
	if int64(len(data)) != length {
		return "", io.ErrUnexpectedEOF
	}
	b.mu.Lock()
	b.objects[object] = append([]byte(nil), data...)
	b.mu.Unlock()
	return remoteVersionID("v-" + object), nil
}

func (b *fastOpenTestWarmBackend) Get(ctx context.Context, object string, rv remoteVersionID, opts WarmBackendGetOpts) (io.ReadCloser, error) {
	b.gets.Add(1)
	b.mu.Lock()
	data, ok := b.objects[object]
	if ok {
		data = append([]byte(nil), data...)
	}
	b.mu.Unlock()
	if !ok {
		return nil, ObjectNotFound{Object: object}
	}
	start := opts.startOffset
	if start < 0 || start > int64(len(data)) {
		return nil, InvalidRange{}
	}
	end := int64(len(data))
	if opts.length > 0 && start+opts.length < end {
		end = start + opts.length
	}
	return io.NopCloser(bytes.NewReader(data[start:end])), nil
}

func (b *fastOpenTestWarmBackend) Remove(ctx context.Context, object string, rv remoteVersionID) error {
	b.mu.Lock()
	delete(b.objects, object)
	b.mu.Unlock()
	return nil
}

func (b *fastOpenTestWarmBackend) InUse(ctx context.Context) (bool, error) {
	return false, nil
}

func installFastOpenTestWarmBackend(t *testing.T, tier string, backend WarmBackend) {
	t.Helper()

	globalTierConfigMgr.Lock()
	oldDriver := globalTierConfigMgr.drivercache[tier]
	oldTier, oldTierOK := globalTierConfigMgr.Tiers[tier]
	globalTierConfigMgr.drivercache[tier] = backend
	globalTierConfigMgr.Tiers[tier] = madmin.TierConfig{Name: tier}
	globalTierConfigMgr.Unlock()
	t.Cleanup(func() {
		globalTierConfigMgr.Lock()
		if oldDriver == nil {
			delete(globalTierConfigMgr.drivercache, tier)
		} else {
			globalTierConfigMgr.drivercache[tier] = oldDriver
		}
		if oldTierOK {
			globalTierConfigMgr.Tiers[tier] = oldTier
		} else {
			delete(globalTierConfigMgr.Tiers, tier)
		}
		globalTierConfigMgr.Unlock()
	})
}

func installFastOpenReplicationConfig(ctx context.Context, t *testing.T, bucket, arn string) {
	t.Helper()

	cfg := replication.Config{
		Rules: []replication.Rule{
			{
				ID:                      "fastopen-replication",
				Status:                  replication.Enabled,
				Priority:                1,
				DeleteMarkerReplication: replication.DeleteMarkerReplication{Status: replication.Enabled},
				DeleteReplication:       replication.DeleteReplication{Status: replication.Enabled},
				Destination:             replication.Destination{ARN: arn},
				Filter:                  replication.Filter{},
			},
		},
	}
	data, err := xml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = globalBucketMetadataSys.Update(ctx, bucket, bucketReplicationConfig, data); err != nil {
		t.Fatal(err)
	}
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

	got := fastOpenGETOpenCount(disks)
	if got != want {
		t.Fatalf("FastOpenPart opens = %d, want %d", got, want)
	}
}

func assertFastOpenGETOpensLessThan(t *testing.T, disks []*fastOpenCountingDisk, limit int) {
	t.Helper()

	got := fastOpenGETOpenCount(disks)
	if got >= limit {
		t.Fatalf("FastOpenPart opens = %d, want less than %d", got, limit)
	}
}

func fastOpenGETOpenCount(disks []*fastOpenCountingDisk) int {
	got := 0
	for _, disk := range disks {
		got += int(disk.opens.Load())
	}
	return got
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
