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
	"strings"
	"testing"
	"time"

	xhttp "github.com/buckit-io/buckit/internal/http"
)

func TestSingleTripHeaderRoundTrip(t *testing.T) {
	h := testSingleTripHeader()
	encoded, err := encodeSingleTripHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != singleTripHeaderLen {
		t.Fatalf("encoded length = %d, want %d", len(encoded), singleTripHeaderLen)
	}
	got, err := decodeSingleTripHeader(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !got.commonEqual(h) {
		t.Fatalf("decoded common fields differ\n got: %#v\nwant: %#v", got, h)
	}
	if got.ErasureIndex != h.ErasureIndex {
		t.Fatalf("decoded erasure index = %d, want %d", got.ErasureIndex, h.ErasureIndex)
	}
	if got.DirectSig == 0 {
		t.Fatal("direct signature was not set")
	}
	if !fastGetHeaderValid(got) {
		t.Fatal("round-tripped header is not valid for fast get")
	}
}

func TestSingleTripHeaderDetectsCorruption(t *testing.T) {
	encoded, err := encodeSingleTripHeader(testSingleTripHeader())
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	if _, err = decodeSingleTripHeader(encoded); err != errSingleTripHeaderBadCRC {
		t.Fatalf("decode error = %v, want %v", err, errSingleTripHeaderBadCRC)
	}
}

func TestSingleTripHeaderRejectsBadMagicAndVersion(t *testing.T) {
	encoded, err := encodeSingleTripHeader(testSingleTripHeader())
	if err != nil {
		t.Fatal(err)
	}
	badMagic := append([]byte(nil), encoded...)
	badMagic[0] ^= 0xff
	if _, err = decodeSingleTripHeader(badMagic); err != errSingleTripHeaderBadMagic {
		t.Fatalf("bad magic decode error = %v, want %v", err, errSingleTripHeaderBadMagic)
	}

	badVersion := append([]byte(nil), encoded...)
	badVersion[8]++
	if _, err = decodeSingleTripHeader(badVersion); err != errSingleTripHeaderBadVersion {
		t.Fatalf("bad version decode error = %v, want %v", err, errSingleTripHeaderBadVersion)
	}
}

func TestSingleTripHeaderDirectSigExcludesErasureIndex(t *testing.T) {
	a := testSingleTripHeader()
	b := a
	b.ErasureIndex = a.ErasureIndex + 1
	if sigA, sigB := singleTripDirectSig(a), singleTripDirectSig(b); sigA != sigB {
		t.Fatalf("signature changed with erasure index: %x != %x", sigA, sigB)
	}
	if !a.commonEqual(b) {
		t.Fatal("common fields should match when only erasure index differs")
	}
	b.PartSize++
	if sigA, sigB := singleTripDirectSig(a), singleTripDirectSig(b); sigA == sigB {
		t.Fatalf("signature did not change with decode-affecting field: %x", sigA)
	}
}

func TestSingleTripHeaderRejectsInvalidErasureIndex(t *testing.T) {
	h := testSingleTripHeader()
	h.ErasureIndex = 0
	if fastGetHeaderValid(h) {
		t.Fatal("zero erasure index should be invalid")
	}
	h.ErasureIndex = h.ErasureM + h.ErasureN + 1
	if fastGetHeaderValid(h) {
		t.Fatal("out-of-range erasure index should be invalid")
	}
}

func TestSingleTripHeaderRejectsOverCapMetadata(t *testing.T) {
	h := testSingleTripHeader()
	h.ContentType = string(bytes.Repeat([]byte{'a'}, singleTripContentTypeMax+1))
	encoded, err := encodeSingleTripHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeSingleTripHeader(encoded); err != errSingleTripHeaderBadPayload {
		t.Fatalf("decode error = %v, want %v", err, errSingleTripHeaderBadPayload)
	}
}

func TestSingleTripHeaderRejectsCompressedAndObjectLockMetadata(t *testing.T) {
	fi := testSingleTripFileInfo("bucket", "object", "data-dir")
	fi.Metadata[ReservedMetadataPrefix+"compression"] = compressionAlgorithmV2
	header, ok := newSingleTripHeaderFromFileInfo(fi, 1)
	if ok || header.Flags&singleTripFlagCompressed == 0 || fastGetHeaderValid(header) {
		t.Fatalf("compressed object header should be ineligible: ok=%v flags=%b", ok, header.Flags)
	}

	fi = testSingleTripFileInfo("bucket", "object", "data-dir")
	fi.Metadata[strings.ToLower(xhttp.AmzObjectLockMode)] = "GOVERNANCE"
	header, ok = newSingleTripHeaderFromFileInfo(fi, 1)
	if ok || header.Flags&singleTripFlagObjectLocked == 0 || fastGetHeaderValid(header) {
		t.Fatalf("object-lock header should be ineligible: ok=%v flags=%b", ok, header.Flags)
	}
}

func TestFastGetRequestEligible(t *testing.T) {
	old := globalFastGetEnabled
	globalFastGetEnabled = true
	t.Cleanup(func() { globalFastGetEnabled = old })

	if !fastGetRequestEligible(nil, nil, ObjectOptions{}) {
		t.Fatal("plain latest request should be eligible")
	}
	if fastGetRequestEligible(nil, &HTTPRangeSpec{Start: 1, End: -1}, ObjectOptions{}) {
		t.Fatal("non-zero range should be ineligible")
	}
	if fastGetRequestEligible(nil, nil, ObjectOptions{VersionID: "v1"}) {
		t.Fatal("versioned request should be ineligible")
	}
}

func TestSingleTripShadowInstallWritesExpectedSizeAndCleansTempParent(t *testing.T) {
	disk, _, err := newXLStorageTestSetup(t)
	if err != nil {
		t.Fatal(err)
	}
	bucket := "bucket"
	object := "object"
	dataDir := "data-dir"
	if err = disk.MakeVol(t.Context(), bucket); err != nil {
		t.Fatal(err)
	}

	fi := testSingleTripFileInfo(bucket, object, dataDir)
	shardSize := fi.Erasure.ShardFileSize(fi.Parts[0].Size)
	bitrotSize := bitrotShardFileSize(shardSize, fi.Erasure.ShardSize(), DefaultBitrotAlgorithm)
	if err = writeTestSingleTripCanonicalPart(t, disk, bucket, pathJoin(object, dataDir, "part.1"), shardSize, fi.Erasure.ShardSize()); err != nil {
		t.Fatal(err)
	}

	srcStats, err := disk.StatInfoFile(t.Context(), bucket, pathJoin(object, dataDir, "part.1"), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcStats) != 1 || srcStats[0].Size != bitrotSize {
		t.Fatalf("source part size = %#v, want %d", srcStats, bitrotSize)
	}

	if err = writeSingleTripShadow(t.Context(), disk, bucket, object, fi); err != nil {
		t.Fatal(err)
	}

	dstStats, err := disk.StatInfoFile(t.Context(), bucket, pathJoin(object, singleTripCurrentDir, "part.1"), false)
	if err != nil {
		t.Fatal(err)
	}
	wantShadowSize := int64(singleTripHeaderLen) + bitrotSize
	if len(dstStats) != 1 || dstStats[0].Size != wantShadowSize {
		t.Fatalf("shadow size = %#v, want %d", dstStats, wantShadowSize)
	}

	tmpStats, err := disk.StatInfoFile(t.Context(), bucket, pathJoin(object, singleTripCurrentDir+".*"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpStats) != 0 {
		t.Fatalf("temporary shadow parent was not cleaned: %#v", tmpStats)
	}
}

func TestSingleTripShadowInvalidationUsesOldParityQuorum(t *testing.T) {
	old := globalFastGetEnabled
	globalFastGetEnabled = true
	t.Cleanup(func() { globalFastGetEnabled = old })

	disks := make([]StorageAPI, 16)
	for i := range disks {
		disks[i] = &singleTripInvalidationDisk{
			header: singleTripHeaderBytesForIndex(t, 14, 2, i+1),
			err:    errors.New("delete failed"),
		}
	}
	for i := range 2 {
		disks[i].(*singleTripInvalidationDisk).err = nil
	}
	disks[2].(*singleTripInvalidationDisk).err = errVolumeNotFound

	var er erasureObjects
	if err := er.invalidateSingleTripShadow(t.Context(), "bucket", "object", disks, 15); err != nil {
		t.Fatalf("invalidate error = %v, want nil with old parity + 1 deletes", err)
	}

	disks[2].(*singleTripInvalidationDisk).err = errors.New("delete failed")
	if err := er.invalidateSingleTripShadow(t.Context(), "bucket", "object", disks, 15); err != errErasureWriteQuorum {
		t.Fatalf("invalidate error = %v, want %v with fewer than old parity + 1 deletes", err, errErasureWriteQuorum)
	}
}

func TestSingleTripPickFastInfoPrefersNewestCompleteGroup(t *testing.T) {
	oldHeader := singleTripHeaderForPickTest(time.Unix(100, 0))
	newHeader := singleTripHeaderForPickTest(time.Unix(200, 0))

	reads := []singleTripHeaderRead{
		{header: oldHeader, rc: io.NopCloser(bytes.NewReader(nil))},
		{header: withSingleTripErasureIndex(oldHeader, 2), rc: io.NopCloser(bytes.NewReader(nil))},
		{header: newHeader, rc: io.NopCloser(bytes.NewReader(nil))},
		{header: withSingleTripErasureIndex(newHeader, 2), rc: io.NopCloser(bytes.NewReader(nil))},
	}
	info, ok := pickSingleTripFastInfo(reads)
	if !ok {
		t.Fatal("expected a complete fast group")
	}
	t.Cleanup(func() { closeBitrotReaders(info.readers) })
	if got := info.fi.ModTime; !got.Equal(time.Unix(200, 0)) {
		t.Fatalf("selected modtime = %v, want newest complete group", got)
	}
}

func TestSingleTripStreamingBitrotReader(t *testing.T) {
	payload := buildTestSingleTripBitrotPayload(t, []byte("aaaaaaaaaabbbbb"), 10)
	reader := newSingleTripStreamingBitrotReader(io.NopCloser(bytes.NewReader(payload)), DefaultBitrotAlgorithm, 10)

	first := make([]byte, 10)
	if _, err := reader.ReadAt(first, 0); err != nil {
		t.Fatal(err)
	}
	if string(first) != "aaaaaaaaaa" {
		t.Fatalf("first chunk = %q", first)
	}

	second := make([]byte, 5)
	if _, err := reader.ReadAt(second, 10); err != nil {
		t.Fatal(err)
	}
	if string(second) != "bbbbb" {
		t.Fatalf("second chunk = %q", second)
	}
}

func TestSingleTripStreamingBitrotReaderRejectsCorruptAndNonSequential(t *testing.T) {
	payload := buildTestSingleTripBitrotPayload(t, []byte("aaaaaaaaaa"), 10)
	payload[0] ^= 0xff
	reader := newSingleTripStreamingBitrotReader(io.NopCloser(bytes.NewReader(payload)), DefaultBitrotAlgorithm, 10)
	if _, err := reader.ReadAt(make([]byte, 10), 0); err != errFileCorrupt {
		t.Fatalf("corrupt read error = %v, want %v", err, errFileCorrupt)
	}

	reader = newSingleTripStreamingBitrotReader(io.NopCloser(bytes.NewReader(buildTestSingleTripBitrotPayload(t, []byte("aaaaaaaaaabbbbb"), 10))), DefaultBitrotAlgorithm, 10)
	if _, err := reader.ReadAt(make([]byte, 5), 10); err != errUnexpected {
		t.Fatalf("non-sequential read error = %v, want %v", err, errUnexpected)
	}
}

func singleTripHeaderForPickTest(modTime time.Time) singleTripHeader {
	h := testSingleTripHeader()
	h.ModTimeNanos = modTime.UnixNano()
	h.ErasureM = 2
	h.ErasureN = 2
	h.ErasureIndex = 1
	h.ErasureDist = []uint8{1, 2, 3, 4}
	h.DirectSig = singleTripDirectSig(h)
	return h
}

func withSingleTripErasureIndex(h singleTripHeader, index uint16) singleTripHeader {
	h.ErasureIndex = index
	return h
}

type singleTripInvalidationDisk struct {
	StorageAPI
	header []byte
	err    error
}

func (d *singleTripInvalidationDisk) IsOnline() bool {
	return true
}

func (d *singleTripInvalidationDisk) ReadFileStream(ctx context.Context, volume, path string, offset, length int64) (io.ReadCloser, error) {
	if d.header == nil {
		return nil, errFileNotFound
	}
	return io.NopCloser(bytes.NewReader(d.header)), nil
}

func (d *singleTripInvalidationDisk) Delete(ctx context.Context, volume string, path string, opts DeleteOptions) error {
	return d.err
}

func singleTripHeaderBytesForIndex(t *testing.T, data, parity, index int) []byte {
	t.Helper()

	h := testSingleTripHeader()
	h.ErasureM = uint16(data)
	h.ErasureN = uint16(parity)
	h.ErasureIndex = uint16(index)
	h.ErasureDist = make([]uint8, data+parity)
	for i := range h.ErasureDist {
		h.ErasureDist[i] = uint8(i + 1)
	}
	h.DirectSig = singleTripDirectSig(h)
	encoded, err := encodeSingleTripHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func testSingleTripHeader() singleTripHeader {
	h := singleTripHeader{
		VersionID:        "",
		ModTimeNanos:     time.Unix(123, 456).UnixNano(),
		Size:             4 << 20,
		PartSize:         4 << 20,
		ActualPartSize:   4 << 20,
		ETag:             "0123456789abcdef0123456789abcdef",
		ErasureM:         8,
		ErasureN:         8,
		ErasureIndex:     3,
		ErasureBlockSize: 10 << 20,
		ErasureDist:      []uint8{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		BitrotAlgo:       DefaultBitrotAlgorithm,
		PartCount:        1,
		ContentType:      "application/octet-stream",
		ContentEncoding:  "gzip",
		CacheControl:     "max-age=60",
		Expires:          "Wed, 21 Oct 2015 07:28:00 GMT",
		StorageClass:     "REDUCED_REDUNDANCY",
	}
	h.DirectSig = singleTripDirectSig(h)
	return h
}

func testSingleTripFileInfo(bucket, object, dataDir string) FileInfo {
	return FileInfo{
		Volume:   bucket,
		Name:     object,
		DataDir:  dataDir,
		IsLatest: true,
		ModTime:  time.Unix(123, 456),
		Size:     35,
		Metadata: map[string]string{
			"content-type": "application/octet-stream",
			"etag":         "0123456789abcdef0123456789abcdef",
		},
		Parts: []ObjectPartInfo{{
			Number:     1,
			Size:       35,
			ActualSize: 99,
			ModTime:    time.Unix(123, 456),
		}},
		Erasure: ErasureInfo{
			Algorithm:    erasureAlgorithm,
			DataBlocks:   2,
			ParityBlocks: 2,
			BlockSize:    10,
			Index:        1,
			Distribution: []int{1, 2, 3, 4},
			Checksums: []ChecksumInfo{{
				PartNumber: 1,
				Algorithm:  DefaultBitrotAlgorithm,
			}},
		},
	}
}

func writeTestSingleTripCanonicalPart(t *testing.T, disk StorageAPI, bucket, partPath string, shardSize, erasureShardSize int64) error {
	t.Helper()

	var src bytes.Buffer
	h := DefaultBitrotAlgorithm.New()
	remaining := shardSize
	for remaining > 0 {
		n := min(remaining, erasureShardSize)
		chunk := bytes.Repeat([]byte{'a'}, int(n))
		h.Reset()
		if _, err := h.Write(chunk); err != nil {
			return err
		}
		src.Write(h.Sum(nil))
		src.Write(chunk)
		remaining -= n
	}
	return disk.CreateFile(t.Context(), "", bucket, partPath, int64(src.Len()), bytes.NewReader(src.Bytes()))
}

func buildTestSingleTripBitrotPayload(t *testing.T, payload []byte, shardSize int64) []byte {
	t.Helper()

	var src bytes.Buffer
	h := DefaultBitrotAlgorithm.New()
	for len(payload) > 0 {
		n := min(len(payload), int(shardSize))
		chunk := payload[:n]
		payload = payload[n:]
		h.Reset()
		if _, err := h.Write(chunk); err != nil {
			t.Fatal(err)
		}
		src.Write(h.Sum(nil))
		src.Write(chunk)
	}
	return src.Bytes()
}
