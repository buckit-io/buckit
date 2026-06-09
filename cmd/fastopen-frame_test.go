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
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/buckit-io/buckit/internal/bucket/lifecycle"
	xhttp "github.com/buckit-io/buckit/internal/http"
)

func TestFastOpenFrameRoundTrip(t *testing.T) {
	frame := testFastOpenFrame(t)
	encoded, err := encodeFastOpenFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= fastOpenFramePreludeLen {
		t.Fatalf("encoded frame length = %d, want payload after prelude", len(encoded))
	}

	prelude, got, err := readFastOpenFrame(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if prelude.Magic != fastOpenFrameMagic {
		t.Fatalf("prelude magic = %q, want %q", prelude.Magic, fastOpenFrameMagic)
	}
	if prelude.Version != fastOpenFrameVersion {
		t.Fatalf("prelude version = %d, want %d", prelude.Version, fastOpenFrameVersion)
	}
	if prelude.BodyMode != frame.BodyMode {
		t.Fatalf("prelude body mode = %d, want %d", prelude.BodyMode, frame.BodyMode)
	}
	if !reflect.DeepEqual(got, frame) {
		t.Fatalf("decoded frame differs\n got: %#v\nwant: %#v", got, frame)
	}
}

func TestFastOpenFrameRejectsBadMagicVersionAndCRC(t *testing.T) {
	encoded, err := encodeFastOpenFrame(testFastOpenFrame(t))
	if err != nil {
		t.Fatal(err)
	}

	badMagic := append([]byte(nil), encoded...)
	badMagic[0] ^= 0xff
	if _, _, err = readFastOpenFrame(bytes.NewReader(badMagic)); !errors.Is(err, errFastOpenFrameBadMagic) {
		t.Fatalf("bad magic error = %v, want %v", err, errFastOpenFrameBadMagic)
	}

	badVersion := append([]byte(nil), encoded...)
	badVersion[5]++
	if _, _, err = readFastOpenFrame(bytes.NewReader(badVersion)); !errors.Is(err, errFastOpenFrameBadVersion) {
		t.Fatalf("bad version error = %v, want %v", err, errFastOpenFrameBadVersion)
	}

	badCRC := append([]byte(nil), encoded...)
	badCRC[len(badCRC)-1] ^= 0xff
	if _, _, err = readFastOpenFrame(bytes.NewReader(badCRC)); !errors.Is(err, errFastOpenFrameBadCRC) {
		t.Fatalf("bad crc error = %v, want %v", err, errFastOpenFrameBadCRC)
	}
}

func TestFastOpenFrameRejectsTruncatedOversizedAndMismatchedFrames(t *testing.T) {
	frame := testFastOpenFrame(t)
	encoded, err := encodeFastOpenFrame(frame)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err = readFastOpenFrame(bytes.NewReader(encoded[:fastOpenFramePreludeLen-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated prelude error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if _, _, err = readFastOpenFrame(bytes.NewReader(encoded[:len(encoded)-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated payload error = %v, want %v", err, io.ErrUnexpectedEOF)
	}

	var oversizedPrelude [fastOpenFramePreludeLen]byte
	encodeFastOpenFramePrelude(oversizedPrelude[:], FastOpenFramePrelude{
		Magic:     fastOpenFrameMagic,
		Version:   fastOpenFrameVersion,
		HeaderLen: fastOpenFrameMaxHeaderLen + 1,
		BodyMode:  FastOpenBodyShard,
	})
	if _, _, err = readFastOpenFrame(bytes.NewReader(oversizedPrelude[:])); !errors.Is(err, errFastOpenFrameHeaderTooLarge) {
		t.Fatalf("oversized header error = %v, want %v", err, errFastOpenFrameHeaderTooLarge)
	}

	mismatchedBodyMode := append([]byte(nil), encoded...)
	mismatchedBodyMode[14] = byte(FastOpenBodyInline)
	if _, _, err = readFastOpenFrame(bytes.NewReader(mismatchedBodyMode)); !errors.Is(err, errFastOpenFrameBadPayload) {
		t.Fatalf("body-mode mismatch error = %v, want %v", err, errFastOpenFrameBadPayload)
	}

	payload, err := marshalCoalescedMetadataFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unmarshalCoalescedMetadataFrame(append(payload, 0xc0)); !errors.Is(err, errFastOpenFrameBadPayload) {
		t.Fatalf("trailing payload error = %v, want %v", err, errFastOpenFrameBadPayload)
	}
}

func TestFastOpenFileInfoMetaRoundTrip(t *testing.T) {
	fi := testFastOpenFileInfo()
	meta, err := fileInfoToFastOpenGETMeta(fi)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fastOpenGETMetaToFileInfo(fi.Volume, fi.Name, FastOpenStatusOK, meta)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got.Metadata, fi.Metadata) {
		t.Fatalf("metadata differs\n got: %#v\nwant: %#v", got.Metadata, fi.Metadata)
	}
	got.Metadata["content-type"] = "mutated"
	if fi.Metadata["content-type"] == "mutated" {
		t.Fatal("metadata map was aliased")
	}
	if !got.ReplicationState.Equal(getInternalReplicationState(fi.Metadata)) {
		t.Fatalf("replication state = %#v, want rebuilt state from metadata", got.ReplicationState)
	}
	if got.Deleted {
		t.Fatal("non-delete status set Deleted")
	}
	if !got.ModTime.Equal(fi.ModTime) || got.ModTime.Location() != time.UTC {
		t.Fatalf("modtime = %v (%v), want UTC equal to %v", got.ModTime, got.ModTime.Location(), fi.ModTime)
	}
	if !got.SuccessorModTime.Equal(fi.SuccessorModTime) || got.SuccessorModTime.Location() != time.UTC {
		t.Fatalf("successor modtime = %v (%v), want UTC equal to %v", got.SuccessorModTime, got.SuccessorModTime.Location(), fi.SuccessorModTime)
	}
	gotChecksum := got.Erasure.GetChecksumInfo(1)
	if gotChecksum.Algorithm != HighwayHash256S {
		t.Fatalf("bitrot algorithm = %v, want %v", gotChecksum.Algorithm, HighwayHash256S)
	}
	if gotChecksum.Hash != nil {
		t.Fatalf("bitrot hash = %q, want nil for v2 streaming bitrot", gotChecksum.Hash)
	}
}

func TestFastOpenEmptyChecksumsUseCanonicalDefaultBitrot(t *testing.T) {
	fi := testFastOpenFileInfo()
	meta, err := fileInfoToFastOpenGETMeta(fi)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Erasure.Bitrot.Algorithm != 3 {
		t.Fatalf("empty checksum encoded algorithm = %d, want FastOpen HighwayHash256S code 3", meta.Erasure.Bitrot.Algorithm)
	}
	got, err := fastOpenGETMetaToFileInfo(fi.Volume, fi.Name, FastOpenStatusOK, meta)
	if err != nil {
		t.Fatal(err)
	}
	gotChecksum := got.Erasure.GetChecksumInfo(1)
	if gotChecksum.Algorithm != HighwayHash256S || gotChecksum.Hash != nil {
		t.Fatalf("checksum = %#v, want default streaming bitrot with nil hash", gotChecksum)
	}
}

func TestFastOpenExplicitChecksumIsPreserved(t *testing.T) {
	fi := testFastOpenFileInfo()
	fi.Erasure.Checksums = []ChecksumInfo{{
		PartNumber: 1,
		Algorithm:  BLAKE2b512,
		Hash:       []byte("legacy-whole-shard-hash"),
	}}
	meta, err := fileInfoToFastOpenGETMeta(fi)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Erasure.Bitrot.Algorithm != 4 {
		t.Fatalf("explicit checksum encoded algorithm = %d, want FastOpen BLAKE2b512 code 4", meta.Erasure.Bitrot.Algorithm)
	}
	got, err := fastOpenGETMetaToFileInfo(fi.Volume, fi.Name, FastOpenStatusOK, meta)
	if err != nil {
		t.Fatal(err)
	}
	gotChecksum := got.Erasure.GetChecksumInfo(1)
	if gotChecksum.Algorithm != BLAKE2b512 || !bytes.Equal(gotChecksum.Hash, []byte("legacy-whole-shard-hash")) {
		t.Fatalf("checksum = %#v, want explicit legacy checksum", gotChecksum)
	}
}

func TestFastOpenMetadataOnlyDoesNotFabricateErasureOrBitrot(t *testing.T) {
	fi := FileInfo{
		Volume:    "bucket",
		Name:      "object",
		VersionID: "delete-marker-version",
		IsLatest:  true,
		Deleted:   true,
		ModTime:   time.Unix(1710000000, 123).UTC(),
		Metadata:  map[string]string{"etag": "delete-marker-etag"},
	}
	meta, err := fileInfoToFastOpenGETMeta(fi)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fastOpenGETMetaToFileInfo(fi.Volume, fi.Name, FastOpenStatusDeleteMarker, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deleted {
		t.Fatal("delete-marker status did not set Deleted")
	}
	if !reflect.DeepEqual(got.Erasure, ErasureInfo{}) {
		t.Fatalf("erasure = %#v, want empty", got.Erasure)
	}
	if len(got.Parts) != 0 {
		t.Fatalf("parts = %#v, want none", got.Parts)
	}
}

func TestFastOpenZeroTimesStayZero(t *testing.T) {
	fi := testFastOpenFileInfo()
	fi.ModTime = time.Time{}
	fi.SuccessorModTime = time.Time{}

	meta, err := fileInfoToFastOpenGETMeta(fi)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fastOpenGETMetaToFileInfo(fi.Volume, fi.Name, FastOpenStatusOK, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ModTime.IsZero() {
		t.Fatalf("modtime = %v, want zero", got.ModTime)
	}
	if !got.SuccessorModTime.IsZero() {
		t.Fatalf("successor modtime = %v, want zero", got.SuccessorModTime)
	}
}

func TestFastOpenDeleteMarkerComesOnlyFromStatus(t *testing.T) {
	fi := testFastOpenFileInfo()
	fi.Deleted = true
	meta, err := fileInfoToFastOpenGETMeta(fi)
	if err != nil {
		t.Fatal(err)
	}

	got, err := fastOpenGETMetaToFileInfo(fi.Volume, fi.Name, FastOpenStatusOK, meta)
	if err != nil {
		t.Fatal(err)
	}
	if got.Deleted {
		t.Fatal("Deleted should not be reconstructed from source FileInfo state")
	}

	got, err = fastOpenGETMetaToFileInfo(fi.Volume, fi.Name, FastOpenStatusDeleteMarker, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deleted {
		t.Fatal("delete-marker status did not set Deleted")
	}
}

func TestFastOpenUnknownBitrotCodeFailsSafely(t *testing.T) {
	meta, err := fileInfoToFastOpenGETMeta(testFastOpenFileInfo())
	if err != nil {
		t.Fatal(err)
	}
	meta.Erasure.Bitrot.Algorithm = 99
	if _, err = fastOpenGETMetaToFileInfo("bucket", "object", FastOpenStatusOK, meta); !errors.Is(err, errFastOpenFrameBadBitrot) {
		t.Fatalf("unknown bitrot error = %v, want %v", err, errFastOpenFrameBadBitrot)
	}
}

func TestFastOpenUnsupportedStatusesAreNotConvertedToFileInfo(t *testing.T) {
	meta, err := fileInfoToFastOpenGETMeta(testFastOpenFileInfo())
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range []FastOpenFrameStatus{
		FastOpenStatusNotFound,
		FastOpenStatusVersionNotFound,
		FastOpenStatusUnsupported,
		FastOpenFrameStatus(99),
	} {
		if _, err = fastOpenGETMetaToFileInfo("bucket", "object", status, meta); !errors.Is(err, errFastOpenFrameBadStatus) {
			t.Fatalf("status %d conversion error = %v, want %v", status, err, errFastOpenFrameBadStatus)
		}
	}
}

func TestFastOpenObjectInfoMatchesCanonical(t *testing.T) {
	restoreExpiry := time.Unix(1710200000, 0).UTC()
	tests := []struct {
		name string
		fi   FileInfo
	}{
		{name: "base", fi: testFastOpenFileInfo()},
		{name: "inline", fi: withFastOpenInline(testFastOpenFileInfo())},
		{name: "restored", fi: withFastOpenMetadata(testFastOpenFileInfo(), xhttp.AmzRestore, completedRestoreObj(restoreExpiry).String())},
		{name: "compressed", fi: withFastOpenMetadata(testFastOpenFileInfo(), ReservedMetadataPrefix+"compression", compressionAlgorithmV2)},
		{name: "nil-metadata", fi: withFastOpenNilMetadata(testFastOpenFileInfo())},
		{name: "non-one-part", fi: withFastOpenPartNumber(testFastOpenFileInfo(), 7)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, err := fileInfoToFastOpenGETMeta(tt.fi)
			if err != nil {
				t.Fatal(err)
			}
			gotFI, err := fastOpenGETMetaToFileInfo(tt.fi.Volume, tt.fi.Name, FastOpenStatusOK, meta)
			if err != nil {
				t.Fatal(err)
			}

			want := tt.fi.ToObjectInfo(tt.fi.Volume, tt.fi.Name, true)
			got := gotFI.ToObjectInfo(tt.fi.Volume, tt.fi.Name, true)
			assertFastOpenObjectInfoEqual(t, got, want)
		})
	}
}

func testFastOpenFrame(t *testing.T) CoalescedMetadataFrame {
	t.Helper()

	meta, err := fileInfoToFastOpenGETMeta(testFastOpenFileInfo())
	if err != nil {
		t.Fatal(err)
	}
	return CoalescedMetadataFrame{
		Status:   FastOpenStatusOK,
		Meta:     meta,
		BodyMode: FastOpenBodyShard,
		BodyLen:  1234,
	}
}

func testFastOpenFileInfo() FileInfo {
	modTime := time.Unix(1710000000, 123).UTC()
	successorModTime := time.Unix(1710000600, 456).UTC()
	fi := FileInfo{
		Volume:      "bucket",
		Name:        "object",
		VersionID:   "version-1",
		IsLatest:    false,
		ModTime:     modTime,
		Size:        35,
		NumVersions: 3,
		Metadata: map[string]string{
			"content-type":         "application/octet-stream",
			"content-encoding":     "gzip",
			"cache-control":        "max-age=60",
			"expires":              "Wed, 21 Oct 2015 07:28:00 GMT",
			"etag":                 "0123456789abcdef0123456789abcdef",
			xhttp.AmzObjectTagging: "tag1=value1",
			xhttp.AmzStorageClass:  "REDUCED_REDUNDANCY",
			ReservedMetadataPrefixLower + ReplicationStatus: "arn1=PENDING;",
			VersionPurgeStatusKey:                           "arn1=FAILED;",
		},
		TransitionStatus:    lifecycle.TransitionComplete,
		TransitionedObjName: "remote-object",
		TransitionTier:      "WARM-TIER",
		TransitionVersionID: "remote-version",
		Parts: []ObjectPartInfo{{
			Number:     1,
			Size:       35,
			ActualSize: 99,
		}},
		Erasure: ErasureInfo{
			Algorithm:    erasureAlgorithm,
			DataBlocks:   2,
			ParityBlocks: 2,
			BlockSize:    10,
			Index:        1,
			Distribution: []int{1, 2, 3, 4},
		},
		SuccessorModTime: successorModTime,
		Checksum:         []byte("object-checksum"),
	}
	fi.ReplicationState = getInternalReplicationState(fi.Metadata)
	return fi
}

func withFastOpenInline(fi FileInfo) FileInfo {
	fi.SetInlineData()
	fi.ReplicationState = getInternalReplicationState(fi.Metadata)
	return fi
}

func withFastOpenMetadata(fi FileInfo, k, v string) FileInfo {
	if fi.Metadata == nil {
		fi.Metadata = make(map[string]string)
	}
	fi.Metadata[k] = v
	fi.ReplicationState = getInternalReplicationState(fi.Metadata)
	return fi
}

func withFastOpenNilMetadata(fi FileInfo) FileInfo {
	fi.Metadata = nil
	fi.ReplicationState = getInternalReplicationState(fi.Metadata)
	return fi
}

func withFastOpenPartNumber(fi FileInfo, partNumber int) FileInfo {
	fi.Parts[0].Number = partNumber
	return fi
}

func assertFastOpenObjectInfoEqual(t *testing.T, got, want ObjectInfo) {
	t.Helper()

	if got.Bucket != want.Bucket || got.Name != want.Name {
		t.Fatalf("identity = %s/%s, want %s/%s", got.Bucket, got.Name, want.Bucket, want.Name)
	}
	if got.VersionID != want.VersionID || got.IsLatest != want.IsLatest {
		t.Fatalf("version = %q latest=%v, want %q latest=%v", got.VersionID, got.IsLatest, want.VersionID, want.IsLatest)
	}
	if !got.ModTime.Equal(want.ModTime) || got.Size != want.Size || got.ETag != want.ETag {
		t.Fatalf("object core differs\ngot:  %#v\nwant: %#v", got, want)
	}
	if got.ContentType != want.ContentType || got.ContentEncoding != want.ContentEncoding || got.CacheControl != want.CacheControl {
		t.Fatalf("headers differ\ngot:  %#v\nwant: %#v", got, want)
	}
	if !got.Expires.Equal(want.Expires) || got.RestoreOngoing != want.RestoreOngoing || !got.RestoreExpires.Equal(want.RestoreExpires) {
		t.Fatalf("time/status headers differ\ngot:  %#v\nwant: %#v", got, want)
	}
	if got.StorageClass != want.StorageClass || got.UserTags != want.UserTags {
		t.Fatalf("storage/tags differ\ngot:  %#v\nwant: %#v", got, want)
	}
	if got.TransitionedObject != want.TransitionedObject {
		t.Fatalf("transition = %#v, want %#v", got.TransitionedObject, want.TransitionedObject)
	}
	if got.ReplicationStatus != want.ReplicationStatus || got.ReplicationStatusInternal != want.ReplicationStatusInternal {
		t.Fatalf("replication = %q, want %q", got.ReplicationStatus, want.ReplicationStatus)
	}
	if got.VersionPurgeStatus != want.VersionPurgeStatus || got.VersionPurgeStatusInternal != want.VersionPurgeStatusInternal {
		t.Fatalf("purge = %q, want %q", got.VersionPurgeStatus, want.VersionPurgeStatus)
	}
	if !reflect.DeepEqual(got.UserDefined, want.UserDefined) {
		t.Fatalf("user-defined metadata differs\n got: %#v\nwant: %#v", got.UserDefined, want.UserDefined)
	}
	if !reflect.DeepEqual(got.Parts, want.Parts) {
		t.Fatalf("parts differ\n got: %#v\nwant: %#v", got.Parts, want.Parts)
	}
	if !bytes.Equal(got.Checksum, want.Checksum) {
		t.Fatalf("checksum = %q, want %q", got.Checksum, want.Checksum)
	}
	if got.Inlined != want.Inlined || got.Legacy != want.Legacy || got.DeleteMarker != want.DeleteMarker {
		t.Fatalf("flags differ\ngot:  %#v\nwant: %#v", got, want)
	}
}
