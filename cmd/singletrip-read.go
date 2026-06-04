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
	"hash"
	"io"
	"net/http"
	"time"

	xhttp "github.com/buckit-io/buckit/internal/http"
	xioutil "github.com/buckit-io/buckit/internal/ioutil"
	"github.com/minio/pkg/v3/sync/errgroup"
)

type singleTripHeaderRead struct {
	disk   StorageAPI
	header singleTripHeader
	rc     io.ReadCloser
	err    error
}

type singleTripFastInfo struct {
	fi          FileInfo
	onlineDisks []StorageAPI
	readers     []io.ReaderAt
}

func (er erasureObjects) tryFastGet(ctx context.Context, bucket, object string, rs *HTTPRangeSpec, h http.Header, opts ObjectOptions, nsUnlocker func()) (*GetObjectReader, bool, error) {
	info, ok := er.openSingleTripFastInfo(ctx, bucket, object)
	if !ok {
		return nil, false, nil
	}

	objInfo := info.fi.ToObjectInfo(bucket, object, false)
	fn, off, length, err := NewGetObjectReader(rs, objInfo, opts, h)
	if err != nil {
		closeBitrotReaders(info.readers)
		return nil, true, err
	}

	if objInfo.Size == 0 {
		closeBitrotReaders(info.readers)
		gr, err := NewGetObjectReaderFromReader(bytes.NewReader(nil), objInfo, opts, nsUnlocker)
		return gr, true, err
	}

	pr, pw := xioutil.WaitPipe()
	go func() {
		pw.CloseWithError(er.getObjectWithSingleTripInfo(ctx, bucket, object, off, length, pw, info))
	}()

	pipeCloser := func() {
		pr.CloseWithError(nil)
	}
	gr, err := fn(pr, h, pipeCloser, nsUnlocker)
	return gr, true, err
}

func (er erasureObjects) openSingleTripFastInfo(ctx context.Context, bucket, object string) (singleTripFastInfo, bool) {
	disks := er.getDisks()
	reads := make([]singleTripHeaderRead, len(disks))
	g := errgroup.WithNErrs(len(disks))
	for index := range disks {
		g.Go(func() error {
			reads[index] = openSingleTripHeader(ctx, disks[index], bucket, object)
			return reads[index].err
		}, index)
	}
	g.Wait()

	info, ok := pickSingleTripFastInfo(reads)
	if !ok {
		closeSingleTripHeaderReads(reads, nil)
		return singleTripFastInfo{}, false
	}
	return info, true
}

func openSingleTripHeader(ctx context.Context, disk StorageAPI, bucket, object string) singleTripHeaderRead {
	return readSingleTripHeader(ctx, disk, bucket, object, true)
}

func readSingleTripHeader(ctx context.Context, disk StorageAPI, bucket, object string, keepStream bool) singleTripHeaderRead {
	r := singleTripHeaderRead{disk: disk, err: errDiskNotFound}
	if disk == nil || !disk.IsOnline() {
		return r
	}

	rc, err := disk.ReadFileStream(ctx, bucket, pathJoin(object, singleTripCurrentDir, "part.1"), 0, -1)
	if err != nil {
		r.err = err
		return r
	}
	headerBytes := make([]byte, singleTripHeaderLen)
	if _, err = io.ReadFull(rc, headerBytes); err != nil {
		rc.Close()
		r.err = err
		return r
	}
	header, err := decodeSingleTripHeader(headerBytes)
	if err != nil {
		rc.Close()
		r.err = err
		return r
	}
	if !fastGetHeaderValid(header) {
		rc.Close()
		r.err = errFileCorrupt
		return r
	}
	if !keepStream {
		rc.Close()
		rc = nil
	}
	r.header = header
	r.rc = rc
	r.err = nil
	return r
}

func pickSingleTripFastInfo(reads []singleTripHeaderRead) (singleTripFastInfo, bool) {
	var (
		bestHeader   singleTripHeader
		bestSelected map[uint16]int
		found        bool
	)
	for i := range reads {
		if reads[i].err != nil {
			continue
		}
		selected := make(map[uint16]int)
		for j := range reads {
			if reads[j].err != nil || reads[j].header.DirectSig != reads[i].header.DirectSig || !reads[j].header.commonEqual(reads[i].header) {
				continue
			}
			index := reads[j].header.ErasureIndex
			if _, ok := selected[index]; ok {
				continue
			}
			selected[index] = j
		}
		if !singleTripHasAllDataIndexes(reads[i].header, selected) {
			continue
		}
		if !found || reads[i].header.ModTimeNanos > bestHeader.ModTimeNanos {
			bestHeader = reads[i].header
			bestSelected = selected
			found = true
		}
	}
	if !found {
		return singleTripFastInfo{}, false
	}
	info := buildSingleTripFastInfo(bestHeader, reads, bestSelected)
	closeSingleTripHeaderReads(reads, bestSelected)
	return info, true
}

func singleTripHasAllDataIndexes(header singleTripHeader, selected map[uint16]int) bool {
	if len(selected) < int(header.ErasureM) {
		return false
	}
	for index := uint16(1); index <= header.ErasureM; index++ {
		if _, ok := selected[index]; !ok {
			return false
		}
	}
	return true
}

func buildSingleTripFastInfo(header singleTripHeader, reads []singleTripHeaderRead, selected map[uint16]int) singleTripFastInfo {
	totalShards := int(header.ErasureM + header.ErasureN)
	fi := header.toFileInfo()
	onlineDisks := make([]StorageAPI, totalShards)
	readers := make([]io.ReaderAt, totalShards)
	for erasureIndex, readIndex := range selected {
		pos := int(erasureIndex) - 1
		onlineDisks[pos] = reads[readIndex].disk
		readers[pos] = newSingleTripStreamingBitrotReader(reads[readIndex].rc, header.BitrotAlgo, fi.Erasure.ShardSize())
	}
	return singleTripFastInfo{
		fi:          fi,
		onlineDisks: onlineDisks,
		readers:     readers,
	}
}

func closeSingleTripHeaderReads(reads []singleTripHeaderRead, keep map[uint16]int) {
	kept := make(map[int]struct{}, len(keep))
	for _, readIndex := range keep {
		kept[readIndex] = struct{}{}
	}
	for index := range reads {
		if reads[index].rc == nil {
			continue
		}
		if _, ok := kept[index]; ok {
			continue
		}
		reads[index].rc.Close()
	}
}

func (h singleTripHeader) toFileInfo() FileInfo {
	distribution := make([]int, 0, len(h.ErasureDist))
	for _, index := range h.ErasureDist {
		distribution = append(distribution, int(index))
	}
	metadata := map[string]string{
		"etag": h.ETag,
	}
	if h.ContentType != "" {
		metadata["content-type"] = h.ContentType
	}
	if h.ContentEncoding != "" {
		metadata["content-encoding"] = h.ContentEncoding
	}
	if h.CacheControl != "" {
		metadata["cache-control"] = h.CacheControl
	}
	if h.Expires != "" {
		metadata["expires"] = h.Expires
	}
	if h.StorageClass != "" {
		metadata[xhttp.AmzStorageClass] = h.StorageClass
	}
	return FileInfo{
		VersionID: h.VersionID,
		IsLatest:  true,
		DataDir:   singleTripCurrentDir,
		ModTime:   time.Unix(0, h.ModTimeNanos),
		Size:      h.Size,
		Metadata:  metadata,
		Parts: []ObjectPartInfo{{
			Number:     1,
			Size:       h.PartSize,
			ActualSize: h.ActualPartSize,
			ModTime:    time.Unix(0, h.ModTimeNanos),
		}},
		Erasure: ErasureInfo{
			Algorithm:    erasureAlgorithm,
			DataBlocks:   int(h.ErasureM),
			ParityBlocks: int(h.ErasureN),
			BlockSize:    h.ErasureBlockSize,
			Index:        int(h.ErasureIndex),
			Distribution: distribution,
			Checksums: []ChecksumInfo{{
				PartNumber: 1,
				Algorithm:  h.BitrotAlgo,
			}},
		},
	}
}

func (er erasureObjects) getObjectWithSingleTripInfo(ctx context.Context, bucket, object string, startOffset int64, length int64, writer io.Writer, info singleTripFastInfo) error {
	defer closeBitrotReaders(info.readers)

	fi := info.fi
	if length < 0 {
		length = fi.Size - startOffset
	}
	if startOffset > fi.Size || startOffset+length > fi.Size {
		return InvalidRange{startOffset, length, fi.Size}
	}
	if length == 0 {
		return nil
	}

	partSize := fi.Parts[0].Size
	erasure, err := NewErasure(ctx, fi.Erasure.DataBlocks, fi.Erasure.ParityBlocks, fi.Erasure.BlockSize)
	if err != nil {
		return toObjectErr(err, bucket, object)
	}

	prefer := make([]bool, len(info.readers))
	for index, disk := range info.onlineDisks {
		prefer[index] = disk != nil && disk.Hostname() == ""
	}
	written, err := erasure.Decode(ctx, writer, info.readers, startOffset, length, partSize, prefer)
	if err != nil {
		if written == length && (errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt)) {
			return nil
		}
		return toObjectErr(err, bucket, object)
	}
	return nil
}

type singleTripStreamingBitrotReader struct {
	rc         io.ReadCloser
	h          hash.Hash
	shardSize  int64
	currOffset int64
	hashBytes  []byte
}

func newSingleTripStreamingBitrotReader(rc io.ReadCloser, algo BitrotAlgorithm, shardSize int64) *singleTripStreamingBitrotReader {
	h := algo.New()
	return &singleTripStreamingBitrotReader{
		rc:        rc,
		h:         h,
		shardSize: shardSize,
		hashBytes: make([]byte, h.Size()),
	}
}

func (r *singleTripStreamingBitrotReader) Close() error {
	if r.rc == nil {
		return nil
	}
	err := r.rc.Close()
	r.rc = nil
	return err
}

func (r *singleTripStreamingBitrotReader) ReadAt(buf []byte, offset int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if offset%r.shardSize != 0 || offset != r.currOffset {
		return 0, errUnexpected
	}
	if _, err := io.ReadFull(r.rc, r.hashBytes); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(r.rc, buf); err != nil {
		return 0, err
	}
	r.h.Reset()
	r.h.Write(buf)
	if !bytes.Equal(r.h.Sum(nil), r.hashBytes) {
		return 0, errFileCorrupt
	}
	r.currOffset += int64(len(buf))
	return len(buf), nil
}

var _ io.ReaderAt = (*singleTripStreamingBitrotReader)(nil)
var _ io.Closer = (*singleTripStreamingBitrotReader)(nil)
