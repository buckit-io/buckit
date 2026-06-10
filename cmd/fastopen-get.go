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
	"sort"

	"github.com/buckit-io/buckit/internal/crypto"
	xioutil "github.com/buckit-io/buckit/internal/ioutil"
	"github.com/cespare/xxhash/v2"
	"github.com/minio/pkg/v3/sync/errgroup"
)

type fastOpenGETRead struct {
	disk      StorageAPI
	diskIndex int
	rc        io.ReadCloser
	frame     CoalescedMetadataFrame
	err       error
}

type fastOpenGETInfo struct {
	fi      FileInfo
	readers []io.ReaderAt
}

// fastOpenGETRequestEligible keeps FastOpen on the plain full-object GET path.
// The compact frame does not carry range/part metadata, SSE-C material, or
// replication/proxy request context, so those requests must use canonical GET.
func fastOpenGETRequestEligible(bucket string, h http.Header, rs *HTTPRangeSpec, opts ObjectOptions) bool {
	if !globalFastGetEnabled {
		return false
	}
	if !opts.FastGetObjInfo {
		return false
	}
	if bucket == minioMetaBucket {
		return false
	}
	if opts.PartNumber != 0 || rs != nil {
		return false
	}
	if opts.ReplicationRequest || opts.ProxyRequest || opts.ProxyHeaderSet {
		return false
	}
	if crypto.SSEC.IsRequested(h) {
		return false
	}
	return true
}

// tryFastOpenGET returns ok=false when FastOpen should be abandoned before the
// client response is committed. An ok=true result means FastOpen selected the
// object-level outcome, even when that outcome is an S3 error such as a delete
// marker or quorum not found.
func (er erasureObjects) tryFastOpenGET(ctx context.Context, bucket, object string, rs *HTTPRangeSpec, h http.Header, opts ObjectOptions, nsUnlocker func()) (*GetObjectReader, bool, error) {
	info, ok, err := er.openFastOpenGETInfo(ctx, bucket, object, opts)
	if err != nil {
		if ok {
			return nil, true, toObjectErr(err, bucket, object)
		}
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	objInfo := info.fi.ToObjectInfo(bucket, object, opts.Versioned || opts.VersionSuspended)
	// Metadata-only outcomes do not need shard readers. Close any streams opened
	// during selection before returning the canonical GET result for the object.
	if objInfo.DeleteMarker {
		closeBitrotReaders(info.readers)
		if opts.VersionID == "" {
			return &GetObjectReader{
				ObjInfo: objInfo,
			}, true, toObjectErr(errFileNotFound, bucket, object)
		}
		return &GetObjectReader{
			ObjInfo: objInfo,
		}, true, toObjectErr(errMethodNotAllowed, bucket, object)
	}

	if crypto.SSEC.IsEncrypted(objInfo.UserDefined) && opts.ReplicationRequest {
		opts.NoDecryption = true
	}

	if objInfo.Size == 0 {
		closeBitrotReaders(info.readers)
		gr, err := NewGetObjectReaderFromReader(bytes.NewReader(nil), objInfo, opts, nsUnlocker)
		return gr, true, err
	}

	if objInfo.IsRemote() {
		closeBitrotReaders(info.readers)
		gr, err := getTransitionedObjectReader(ctx, bucket, object, rs, h, objInfo, opts)
		if err != nil {
			return nil, true, err
		}
		return gr.WithCleanupFuncs(nsUnlocker), true, nil
	}

	fn, off, length, err := NewGetObjectReader(rs, objInfo, opts, h)
	if err != nil {
		closeBitrotReaders(info.readers)
		return nil, true, err
	}

	pr, pw := xioutil.WaitPipe()
	go func() {
		pw.CloseWithError(er.getObjectWithFastOpenInfo(ctx, bucket, object, off, length, pw, info))
	}()

	pipeCloser := func() {
		pr.CloseWithError(nil)
	}
	gr, err := fn(pr, h, pipeCloser, nsUnlocker)
	return gr, true, err
}

// openFastOpenGETInfo opens the initial read-quorum-sized disk set and consumes
// only the FastOpen frame from each stream. Body streams are left positioned
// immediately after their frame and are either selected for decode or closed.
func (er erasureObjects) openFastOpenGETInfo(ctx context.Context, bucket, object string, opts ObjectOptions) (fastOpenGETInfo, bool, error) {
	disks := er.getDisks()
	openCount := er.fastOpenInitialOpenCount()
	selected := selectFastOpenGETDisks(disks, openCount, bucket, object)
	if len(selected) < openCount {
		// Without a complete first wave there is no useful FastOpen decision to
		// make here; canonical GET can still fan out to all disks.
		return fastOpenGETInfo{}, false, nil
	}

	reads := make([]fastOpenGETRead, len(selected))
	g := errgroup.WithNErrs(len(selected))
	for gi, di := range selected {
		gi, di := gi, di
		disk := disks[di]
		g.Go(func() error {
			reads[gi] = openFastOpenGETRead(ctx, disk, di, bucket, object, opts.VersionID)
			return reads[gi].err
		}, gi)
	}
	g.Wait()

	info, ok, err := er.pickFastOpenGETInfo(ctx, bucket, object, disks, reads, opts)
	if !ok || err != nil {
		closeFastOpenGETReadsExcept(reads, nil)
		if !ok {
			return info, false, nil
		}
		return info, ok, err
	}
	return info, true, nil
}

// openFastOpenGETRead owns a cancellable child context for exactly one disk
// stream. Closing the returned rc cancels the remote/local FastOpenPart work in
// addition to closing the body reader.
func openFastOpenGETRead(ctx context.Context, disk StorageAPI, diskIndex int, bucket, object, versionID string) fastOpenGETRead {
	r := fastOpenGETRead{
		disk:      disk,
		diskIndex: diskIndex,
		err:       errDiskNotFound,
	}
	readCtx, cancel := context.WithCancel(ctx)
	rc, err := disk.FastOpenPart(readCtx, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		VersionID:  versionID,
		PartNumber: 1,
		Offset:     0,
		Length:     -1,
	})
	if err != nil {
		cancel()
		r.err = err
		return r
	}
	rc = &fastOpenCancelReadCloser{rc: rc, cancel: cancel}
	_, frame, err := readFastOpenFrame(rc)
	if err != nil {
		rc.Close()
		r.err = err
		return r
	}
	r.rc = rc
	r.frame = frame
	r.err = nil
	return r
}

// pickFastOpenGETInfo maps per-disk FastOpen frames into the same FileInfo,
// error, and online-disk arrays that canonical GET uses for quorum selection.
// It returns ok=false only for pre-commit cases where canonical GET should be
// tried instead.
func (er erasureObjects) pickFastOpenGETInfo(ctx context.Context, bucket, object string, disks []StorageAPI, reads []fastOpenGETRead, opts ObjectOptions) (fastOpenGETInfo, bool, error) {
	metaArr := make([]FileInfo, er.setDriveCount)
	errs := make([]error, er.setDriveCount)
	for i := range errs {
		errs[i] = errDiskOngoingReq
	}
	for i := range reads {
		read := &reads[i]
		if read.diskIndex < 0 || read.diskIndex >= len(errs) {
			continue
		}
		if read.err != nil {
			errs[read.diskIndex] = read.err
			continue
		}
		switch read.frame.Status {
		case FastOpenStatusOK, FastOpenStatusDeleteMarker:
			fi, err := fastOpenGETMetaToFileInfo(bucket, object, read.frame.Status, read.frame.Meta)
			if err != nil {
				errs[read.diskIndex] = err
				continue
			}
			metaArr[read.diskIndex] = fi
			errs[read.diskIndex] = nil
		case FastOpenStatusNotFound:
			errs[read.diskIndex] = errFileNotFound
		case FastOpenStatusVersionNotFound:
			errs[read.diskIndex] = errFileVersionNotFound
		case FastOpenStatusUnsupported:
			return fastOpenGETInfo{}, false, nil
		default:
			errs[read.diskIndex] = errFastOpenFrameBadStatus
		}
	}

	// NotFound and VersionNotFound are disk-local frame statuses. They become an
	// object-level error only after the same quorum rules canonical GET applies.
	readQuorum, _, err := objectQuorumFromMeta(ctx, metaArr, errs, er.defaultParityCount)
	if err != nil {
		if errors.Is(err, errFileNotFound) || errors.Is(err, errFileVersionNotFound) {
			return fastOpenGETInfo{}, true, err
		}
		return fastOpenGETInfo{}, false, err
	}
	if err = reduceReadQuorumErrs(ctx, errs, objectOpIgnoredErrs, readQuorum); err != nil {
		if errors.Is(err, errFileNotFound) || errors.Is(err, errFileVersionNotFound) {
			return fastOpenGETInfo{}, true, err
		}
		return fastOpenGETInfo{}, false, err
	}
	onlineDisks, modTime, etag := listOnlineDisks(disks, metaArr, errs, readQuorum)
	fi, err := pickValidFileInfo(ctx, metaArr, modTime, etag, readQuorum)
	if err != nil {
		return fastOpenGETInfo{}, false, err
	}

	// Reapply the canonical winning-version filters before selecting body
	// streams. A disk whose compact metadata is valid but stale must not
	// contribute a shard to the decode set.
	onlineMeta := make([]FileInfo, len(metaArr))
	for i, disk := range onlineDisks {
		if disk != nil {
			onlineMeta[i] = metaArr[i]
		}
	}
	filterOnlineDisksInplace(fi, onlineMeta, onlineDisks)
	for i := range onlineMeta {
		if onlineMeta[i].IsValid() && onlineMeta[i].Erasure.Equal(fi.Erasure) {
			ok := onlineMeta[i].ModTime.Equal(modTime)
			if modTime.IsZero() || modTime.Equal(timeSentinel) {
				ok = etag != "" && etag == fi.Metadata["etag"]
			}
			if ok {
				continue
			}
		}
		onlineMeta[i] = FileInfo{}
		onlineDisks[i] = nil
	}

	info := fastOpenGETInfo{fi: fi}
	if fi.Deleted || fi.Size == 0 || fi.IsRemote() {
		closeFastOpenGETReadsExcept(reads, nil)
		return info, true, nil
	}
	readers, used, ok := buildFastOpenGETReaders(fi, reads, onlineMeta, onlineDisks)
	if !ok {
		return fastOpenGETInfo{}, false, nil
	}
	closeFastOpenGETReadsExcept(reads, used)
	info.readers = readers
	return info, true, nil
}

// buildFastOpenGETReaders transfers ownership of selected body streams to the
// returned ReaderAt slice. The used map tells the caller which opened streams
// must stay live; every other opened stream can be closed immediately.
func buildFastOpenGETReaders(fi FileInfo, reads []fastOpenGETRead, metaArr []FileInfo, onlineDisks []StorageAPI) ([]io.ReaderAt, map[int]bool, bool) {
	readByDisk := make(map[int]int, len(reads))
	for i := range reads {
		readByDisk[reads[i].diskIndex] = i
	}

	readers := make([]io.ReaderAt, len(fi.Erasure.Distribution))
	used := make(map[int]bool)
	for diskIndex := range metaArr {
		if onlineDisks[diskIndex] == nil || !metaArr[diskIndex].IsValid() {
			continue
		}
		readIndex, ok := readByDisk[diskIndex]
		if !ok || reads[readIndex].rc == nil {
			continue
		}
		mode := reads[readIndex].frame.BodyMode
		if mode != FastOpenBodyShard && mode != FastOpenBodyInline {
			continue
		}
		checksumInfo := metaArr[diskIndex].Erasure.GetChecksumInfo(1)
		if checksumInfo.Algorithm != HighwayHash256S {
			// The stream reader below understands Buckit's streaming bitrot
			// layout. Other algorithms/modes need canonical GET unless a
			// matching FastOpen reader is added.
			return nil, nil, false
		}
		// The stream and its compact metadata come from the same disk, so
		// Erasure.Index identifies the shard position directly. Canonical GET
		// also checks distribution[diskIndex] because it builds readers from a
		// full disk array; FastOpen has already paired this disk's index with
		// this disk's body stream before reaching this point.
		pos := metaArr[diskIndex].Erasure.Index - 1
		if pos < 0 || pos >= len(readers) || readers[pos] != nil {
			continue
		}
		readers[pos] = newFastOpenStreamingBitrotReader(reads[readIndex].rc, checksumInfo.Algorithm, fi.Erasure.ShardSize())
		used[readIndex] = true
	}
	if len(used) < fi.Erasure.DataBlocks {
		return nil, nil, false
	}
	return readers, used, true
}

// getObjectWithFastOpenInfo decodes already-open encoded shard streams through
// the same erasure decoder used by canonical GET. The response body pipe is not
// created until metadata/body quorum has been selected, so decode always starts
// at object offset 0 for the selected FastOpen streams.
func (er erasureObjects) getObjectWithFastOpenInfo(ctx context.Context, bucket, object string, startOffset int64, length int64, writer io.Writer, info fastOpenGETInfo) error {
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

	// FastOpen readers are already selected and positioned by erasure index, so
	// the decoder does not need the canonical prefer-local reordering hint.
	prefer := make([]bool, len(info.readers))
	written, err := erasure.Decode(ctx, writer, info.readers, startOffset, length, partSize, prefer)
	if err != nil {
		if written == length && (errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt)) {
			return nil
		}
		return toObjectErr(err, bucket, object)
	}
	return nil
}

// selectFastOpenGETDisks chooses the first wave only from disks that can be
// called now. Local-first keeps the usual low-latency path hot; spread mode
// rotates the first wave by object name to distribute remote read pressure.
func selectFastOpenGETDisks(disks []StorageAPI, openCount int, bucket, object string) []int {
	type candidate struct {
		index int
		local bool
	}
	cands := make([]candidate, 0, len(disks))
	for i, disk := range disks {
		if disk == nil || !disk.IsOnline() {
			continue
		}
		cands = append(cands, candidate{index: i, local: disk.IsLocal()})
	}
	if globalFastGetSpreadSelection {
		if len(cands) > 0 {
			start := int(xxhash.Sum64String(bucket+SlashSeparator+object) % uint64(len(cands)))
			rotated := append(append([]candidate(nil), cands[start:]...), cands[:start]...)
			cands = rotated
		}
	} else {
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].local != cands[j].local {
				return cands[i].local
			}
			return cands[i].index < cands[j].index
		})
	}
	if openCount > len(cands) {
		openCount = len(cands)
	}
	out := make([]int, openCount)
	for i := range out {
		out[i] = cands[i].index
	}
	return out
}

// fastOpenInitialOpenCount matches the maximum configured read quorum for the
// set. Objects with a higher data-block count than this first wave cleanly fall
// back after their compact metadata reveals the actual layout.
func (er erasureObjects) fastOpenInitialOpenCount() int {
	dataCount := er.setDriveCount - er.defaultParityCount
	if dataCount == er.defaultParityCount {
		return dataCount + 1
	}
	return dataCount
}

// closeFastOpenGETReadsExcept closes every opened FastOpen stream that was not
// transferred to the selected erasure readers. Closing also cancels the stream's
// child context through fastOpenCancelReadCloser.
func closeFastOpenGETReadsExcept(reads []fastOpenGETRead, used map[int]bool) {
	for i := range reads {
		if reads[i].rc == nil || used[i] {
			continue
		}
		reads[i].rc.Close()
	}
}

type fastOpenCancelReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (c *fastOpenCancelReadCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c *fastOpenCancelReadCloser) Close() error {
	c.cancel()
	return c.rc.Close()
}

type fastOpenStreamingBitrotReader struct {
	rc         io.ReadCloser
	h          hash.Hash
	shardSize  int64
	currOffset int64
	hashBytes  []byte
}

func newFastOpenStreamingBitrotReader(rc io.ReadCloser, algo BitrotAlgorithm, shardSize int64) *fastOpenStreamingBitrotReader {
	h := algo.New()
	return &fastOpenStreamingBitrotReader{
		rc:        rc,
		h:         h,
		shardSize: shardSize,
		hashBytes: make([]byte, h.Size()),
	}
}

func (r *fastOpenStreamingBitrotReader) Close() error {
	if r.rc == nil {
		return nil
	}
	err := r.rc.Close()
	r.rc = nil
	return err
}

func (r *fastOpenStreamingBitrotReader) ReadAt(buf []byte, offset int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	// FastOpen streams are already open and positioned after the metadata
	// frame, so they can only be consumed sequentially from offset 0. A spare
	// stream cannot be joined later at a non-zero stripe without reopening it.
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

var _ io.ReaderAt = (*fastOpenStreamingBitrotReader)(nil)
var _ io.Closer = (*fastOpenStreamingBitrotReader)(nil)
