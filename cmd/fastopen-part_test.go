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
	"errors"
	"io"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buckit-io/buckit/internal/bucket/lifecycle"
	xhttp "github.com/buckit-io/buckit/internal/http"
)

func TestXLStorageFastOpenPartShard(t *testing.T) {
	disk, bucket, object := newFastOpenPartTestDisk(t)
	fi := testFastOpenPartFileInfo(bucket, object)
	body := []byte("encoded-shard-body")
	writeFastOpenPartObject(t, disk, fi, body)

	_, frame, gotBody := readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusOK || frame.BodyMode != FastOpenBodyShard {
		t.Fatalf("frame status/mode = %d/%d, want OK/shard", frame.Status, frame.BodyMode)
	}
	if frame.BodyLen != int64(len(body)) {
		t.Fatalf("body len = %d, want %d", frame.BodyLen, len(body))
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("body = %q, want %q", gotBody, body)
	}
	if frame.Meta.VersionID != fi.VersionID || frame.Meta.Part.Number != 1 {
		t.Fatalf("meta = %#v, want version %q part 1", frame.Meta, fi.VersionID)
	}
}

func TestXLStorageFastOpenPartExplicitVersion(t *testing.T) {
	disk, bucket, object := newFastOpenPartTestDisk(t)
	oldFI := testFastOpenPartFileInfo(bucket, object)
	oldFI.VersionID = mustGetUUID()
	oldFI.DataDir = mustGetUUID()
	oldFI.ModTime = time.Unix(100, 0).UTC()
	oldBody := []byte("old-version-body")
	writeFastOpenPartObject(t, disk, oldFI, oldBody)

	newFI := testFastOpenPartFileInfo(bucket, object)
	newFI.VersionID = mustGetUUID()
	newFI.DataDir = mustGetUUID()
	newFI.ModTime = time.Unix(200, 0).UTC()
	writeFastOpenPartObject(t, disk, newFI, []byte("new-version-body"))

	_, frame, gotBody := readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		VersionID:  oldFI.VersionID,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Meta.VersionID != oldFI.VersionID || frame.Meta.IsLatest {
		t.Fatalf("selected version = %q latest=%v, want old non-latest", frame.Meta.VersionID, frame.Meta.IsLatest)
	}
	if !bytes.Equal(gotBody, oldBody) {
		t.Fatalf("body = %q, want %q", gotBody, oldBody)
	}
}

func TestXLStorageFastOpenPartInline(t *testing.T) {
	disk, bucket, object := newFastOpenPartTestDisk(t)
	fi := testFastOpenPartFileInfo(bucket, object)
	inlineBody := []byte("inline-body")
	fi.Size = int64(len(inlineBody))
	fi.Parts[0].Size = fi.Size
	fi.Parts[0].ActualSize = fi.Size
	fi.Data = inlineBody
	fi.SetInlineData()
	if err := disk.WriteMetadata(t.Context(), "", bucket, object, fi); err != nil {
		t.Fatal(err)
	}

	_, frame, gotBody := readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusOK || frame.BodyMode != FastOpenBodyInline {
		t.Fatalf("frame status/mode = %d/%d, want OK/inline", frame.Status, frame.BodyMode)
	}
	if frame.BodyLen != int64(len(inlineBody)) || !bytes.Equal(gotBody, inlineBody) {
		t.Fatalf("inline body len/body = %d/%q, want %d/%q", frame.BodyLen, gotBody, len(inlineBody), inlineBody)
	}
}

func TestXLStorageFastOpenPartHeaderlessInlineTiebreak(t *testing.T) {
	disk, bucket, object := newFastOpenPartTestDisk(t)
	fi := testFastOpenPartFileInfo(bucket, object)
	inlineBody := []byte("headerless-inline-body")
	fi.Size = int64(len(inlineBody))
	fi.Parts[0].Size = fi.Size
	fi.Parts[0].ActualSize = fi.Size
	fi.Data = inlineBody
	if err := disk.WriteMetadata(t.Context(), "", bucket, object, fi); err != nil {
		t.Fatal(err)
	}

	_, frame, gotBody := readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.BodyMode != FastOpenBodyInline || !bytes.Equal(gotBody, inlineBody) {
		t.Fatalf("headerless inline frame/body = %#v/%q, want inline %q", frame, gotBody, inlineBody)
	}

	conflictObject := object + "-conflict"
	conflictFI := testFastOpenPartFileInfo(bucket, conflictObject)
	conflictFI.Size = int64(len(inlineBody))
	conflictFI.Parts[0].Size = conflictFI.Size
	conflictFI.Parts[0].ActualSize = conflictFI.Size
	conflictFI.Data = inlineBody
	shardBody := []byte("newer-shard-body")
	writeFastOpenPartObject(t, disk, conflictFI, shardBody)

	_, frame, gotBody = readFastOpenPart(t, disk, bucket, conflictObject, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.BodyMode != FastOpenBodyShard || !bytes.Equal(gotBody, shardBody) {
		t.Fatalf("conflict frame/body = %#v/%q, want shard %q", frame, gotBody, shardBody)
	}
}

func TestXLStorageFastOpenPartMetadataOnlyStatuses(t *testing.T) {
	disk, bucket, object := newFastOpenPartTestDisk(t)

	_, frame, body := readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusNotFound || frame.BodyMode != FastOpenBodyMetadataOnly || len(body) != 0 {
		t.Fatalf("not-found frame/body = %#v/%q", frame, body)
	}

	fi := testFastOpenPartFileInfo(bucket, object)
	writeFastOpenPartObject(t, disk, fi, []byte("body"))
	_, frame, body = readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		VersionID:  mustGetUUID(),
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusVersionNotFound || frame.BodyMode != FastOpenBodyMetadataOnly || len(body) != 0 {
		t.Fatalf("version-not-found frame/body = %#v/%q", frame, body)
	}

	zeroObject := object + "-zero"
	zeroFI := testFastOpenPartFileInfo(bucket, zeroObject)
	zeroFI.Size = 0
	zeroFI.Parts[0].Size = 0
	zeroFI.Parts[0].ActualSize = 0
	if err := disk.WriteMetadata(t.Context(), "", bucket, zeroObject, zeroFI); err != nil {
		t.Fatal(err)
	}
	_, frame, body = readFastOpenPart(t, disk, bucket, zeroObject, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusOK || frame.BodyMode != FastOpenBodyMetadataOnly || frame.BodyLen != 0 || len(body) != 0 {
		t.Fatalf("zero-byte frame/body = %#v/%q", frame, body)
	}

	deleteFI := FileInfo{
		Volume:    bucket,
		Name:      object + "-deleted",
		VersionID: mustGetUUID(),
		Deleted:   true,
		ModTime:   time.Unix(300, 0).UTC(),
		Metadata:  map[string]string{"etag": "delete-marker"},
	}
	if err := disk.WriteMetadata(t.Context(), "", bucket, deleteFI.Name, deleteFI); err != nil {
		t.Fatal(err)
	}
	_, frame, body = readFastOpenPart(t, disk, bucket, deleteFI.Name, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusDeleteMarker || frame.BodyMode != FastOpenBodyMetadataOnly || len(body) != 0 {
		t.Fatalf("delete-marker frame/body = %#v/%q", frame, body)
	}
}

func TestXLStorageFastOpenPartTransitionedAndUnsupported(t *testing.T) {
	disk, bucket, object := newFastOpenPartTestDisk(t)
	fi := testFastOpenPartFileInfo(bucket, object)
	fi.TransitionStatus = lifecycle.TransitionComplete
	fi.TransitionedObjName = "remote-object"
	fi.TransitionTier = "WARM-TIER"
	fi.TransitionVersionID = "remote-version"
	writeFastOpenPartObject(t, disk, fi, []byte("local-restored-copy-ignored"))

	_, frame, body := readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusOK || frame.BodyMode != FastOpenBodyTransitioned || len(body) != 0 {
		t.Fatalf("transitioned frame/body = %#v/%q", frame, body)
	}

	restoredObject := object + "-restored"
	restoredFI := testFastOpenPartFileInfo(bucket, restoredObject)
	restoredFI.TransitionStatus = lifecycle.TransitionComplete
	restoredFI.TransitionedObjName = "remote-object"
	restoredFI.TransitionTier = "WARM-TIER"
	restoredFI.TransitionVersionID = "remote-version"
	restoredFI.Metadata[xhttp.AmzRestore] = completedRestoreObj(UTCNow().Add(time.Hour)).String()
	restoredBody := []byte("restored-local-shard")
	writeFastOpenPartObject(t, disk, restoredFI, restoredBody)
	_, frame, body = readFastOpenPart(t, disk, bucket, restoredObject, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusOK || frame.BodyMode != FastOpenBodyShard || !bytes.Equal(body, restoredBody) {
		t.Fatalf("restored transitioned frame/body = %#v/%q, want shard %q", frame, body, restoredBody)
	}

	_, frame, body = readFastOpenPart(t, disk, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Offset:     1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusUnsupported || frame.BodyMode != FastOpenBodyMetadataOnly || len(body) != 0 {
		t.Fatalf("unsupported request frame/body = %#v/%q", frame, body)
	}

	mpObject := object + "-multipart"
	mpFI := testFastOpenPartFileInfo(bucket, mpObject)
	mpFI.Parts = append(mpFI.Parts, ObjectPartInfo{Number: 2, Size: 1, ActualSize: 1})
	writeFastOpenPartObject(t, disk, mpFI, []byte("part-1"))
	_, frame, body = readFastOpenPart(t, disk, bucket, mpObject, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusUnsupported || frame.BodyMode != FastOpenBodyMetadataOnly || len(body) != 0 {
		t.Fatalf("multipart frame/body = %#v/%q", frame, body)
	}
}

func newFastOpenPartTestDisk(t *testing.T) (*xlStorageDiskIDCheck, string, string) {
	t.Helper()

	disk, _, err := newXLStorageTestSetup(t)
	if err != nil {
		t.Fatal(err)
	}
	bucket := "bucket"
	object := "object"
	if err = disk.MakeVol(t.Context(), bucket); err != nil {
		t.Fatal(err)
	}
	return disk, bucket, object
}

func testFastOpenPartFileInfo(bucket, object string) FileInfo {
	return FileInfo{
		Volume:    bucket,
		Name:      object,
		VersionID: mustGetUUID(),
		DataDir:   mustGetUUID(),
		IsLatest:  true,
		ModTime:   time.Unix(123, 456).UTC(),
		Size:      16,
		Metadata: map[string]string{
			"content-type": "application/octet-stream",
			"etag":         "0123456789abcdef0123456789abcdef",
		},
		Parts: []ObjectPartInfo{{
			Number:     1,
			Size:       16,
			ActualSize: 16,
		}},
		Erasure: ErasureInfo{
			Algorithm:    erasureAlgorithm,
			DataBlocks:   2,
			ParityBlocks: 2,
			BlockSize:    10,
			Index:        1,
			Distribution: []int{1, 2, 3, 4},
		},
	}
}

func writeFastOpenPartObject(t *testing.T, disk StorageAPI, fi FileInfo, body []byte) {
	t.Helper()

	if err := disk.WriteMetadata(t.Context(), "", fi.Volume, fi.Name, fi); err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || fi.Deleted || fi.InlineData() {
		return
	}
	if err := disk.CreateFile(t.Context(), "", fi.Volume, pathJoin(fi.Name, fi.DataDir, "part.1"), int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func readFastOpenPart(t *testing.T, disk StorageAPI, bucket, object string, req FastOpenPartRequest) (FastOpenFramePrelude, CoalescedMetadataFrame, []byte) {
	t.Helper()

	rc, err := disk.FastOpenPart(t.Context(), bucket, object, req)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	prelude, frame, err := readFastOpenFrame(rc)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return prelude, frame, body
}

func TestStorageRESTClientFastOpenPart(t *testing.T) {
	restClient := newStorageRESTHTTPServerClient(t)
	bucket := "foo"
	object := "fastopen/object"
	fi := testFastOpenPartFileInfo(bucket, object)
	body := []byte("remote-fastopen-shard-body")
	writeFastOpenPartObject(t, restClient, fi, body)

	_, frame, gotBody := readFastOpenPart(t, restClient, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusOK || frame.BodyMode != FastOpenBodyShard {
		t.Fatalf("remote frame status/mode = %d/%d, want OK/shard", frame.Status, frame.BodyMode)
	}
	if frame.BodyLen != int64(len(body)) || !bytes.Equal(gotBody, body) {
		t.Fatalf("remote body len/body = %d/%q, want %d/%q", frame.BodyLen, gotBody, len(body), body)
	}

	_, frame, gotBody = readFastOpenPart(t, restClient, bucket, object+"-missing", FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if frame.Status != FastOpenStatusNotFound || frame.BodyMode != FastOpenBodyMetadataOnly || len(gotBody) != 0 {
		t.Fatalf("remote not-found frame/body = %#v/%q", frame, gotBody)
	}

	rc, err := restClient.FastOpenPart(t.Context(), bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion + 1,
		PartNumber: 1,
		Length:     -1,
	})
	if err == nil {
		rc.Close()
		t.Fatal("FastOpenPart with bad protocol version succeeded")
	}
	if !errors.Is(err, errFastOpenFrameBadVersion) {
		t.Fatalf("FastOpenPart bad version error = %v, want %v", err, errFastOpenFrameBadVersion)
	}
}

func TestStorageRESTClientFastOpenPartMalformedParams(t *testing.T) {
	restClient := newStorageRESTHTTPServerClient(t)
	values := make(url.Values)
	values.Set(storageRESTVolume, "foo")
	values.Set(storageRESTFilePath, "fastopen/object")
	values.Set(storageRESTFastOpenVersion, "not-a-version")
	values.Set(storageRESTVersionID, "")
	values.Set(storageRESTPartNumber, "1")
	values.Set(storageRESTOffset, "0")
	values.Set(storageRESTLength, "-1")
	values.Set(storageRESTFastOpenFlags, "0")

	respBody, err := restClient.callGet(t.Context(), storageRESTMethodFastOpenPart, values, nil, -1)
	if err == nil {
		respBody.Close()
		t.Fatal("malformed FastOpenPart params succeeded")
	}
	if err.Error() != strconv.ErrSyntax.Error() {
		t.Fatalf("malformed FastOpenPart params error = %v, want %v", err, strconv.ErrSyntax)
	}
}

func TestStorageRESTClientFastOpenPartCloseCancelsRemoteWork(t *testing.T) {
	restClient := newStorageRESTHTTPServerClient(t)
	orig := globalLocalSetDrives[0][0][0]
	disk := &cancelAwareFastOpenDisk{
		StorageAPI: orig,
		closed:     make(chan struct{}),
	}
	globalLocalSetDrives[0][0][0] = disk
	t.Cleanup(func() {
		globalLocalSetDrives[0][0][0] = orig
	})

	rc, err := restClient.FastOpenPart(t.Context(), "foo", "fastopen/cancel", FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		PartNumber: 1,
		Length:     -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = readFastOpenFrame(rc); err != nil {
		rc.Close()
		t.Fatal(err)
	}
	if err = rc.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-disk.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("remote FastOpenPart stream was not closed after client Close")
	}
}

func TestFastOpenRemoteReadCloserCloseCancels(t *testing.T) {
	body := &trackingReadCloser{Reader: bytes.NewReader([]byte("body"))}
	var canceled atomic.Bool
	rc := &fastOpenRemoteReadCloser{
		ReadCloser: body,
		cancel: func() {
			canceled.Store(true)
		},
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !canceled.Load() {
		t.Fatal("Close did not cancel the remote stream context")
	}
	if !body.closed.Load() {
		t.Fatal("Close did not close the response body")
	}
}

type trackingReadCloser struct {
	io.Reader
	closed atomic.Bool
}

func (rc *trackingReadCloser) Close() error {
	rc.closed.Store(true)
	return nil
}

type cancelAwareFastOpenDisk struct {
	StorageAPI
	closed chan struct{}
}

func (d *cancelAwareFastOpenDisk) FastOpenPart(ctx context.Context, volume, path string, req FastOpenPartRequest) (io.ReadCloser, error) {
	body := bytes.Repeat([]byte("x"), 256<<10)
	frame, err := encodeFastOpenFrame(CoalescedMetadataFrame{
		Status:   FastOpenStatusOK,
		BodyMode: FastOpenBodyShard,
		BodyLen:  int64(len(body)),
	})
	if err != nil {
		return nil, err
	}
	return &cancelAwareFastOpenStream{
		ctx:     ctx,
		initial: bytes.NewReader(append(frame, body...)),
		closed:  d.closed,
	}, nil
}

type cancelAwareFastOpenStream struct {
	ctx     context.Context
	initial *bytes.Reader
	closed  chan struct{}
	done    atomic.Bool
}

func (s *cancelAwareFastOpenStream) Read(p []byte) (int, error) {
	if s.initial.Len() > 0 {
		return s.initial.Read(p)
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func (s *cancelAwareFastOpenStream) Close() error {
	if s.done.CompareAndSwap(false, true) {
		close(s.closed)
	}
	return nil
}
