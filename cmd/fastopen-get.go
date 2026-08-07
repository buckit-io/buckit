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
	"sync"
	"sync/atomic"
	"time"

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
	prefer  []bool
}

type fastOpenGETOutcome uint8

const (
	fastOpenGETFallback fastOpenGETOutcome = iota
	fastOpenGETSuccess
	fastOpenGETTerminalError
)

type fastOpenGETResult struct {
	reader  *GetObjectReader
	outcome fastOpenGETOutcome
	err     error
}

type fastOpenGETInfoResult struct {
	info    fastOpenGETInfo
	outcome fastOpenGETOutcome
	err     error
}

type fastOpenFailureReason uint8

const (
	fastOpenFailureNoQuorum fastOpenFailureReason = iota
	fastOpenFailureUnsupported
	fastOpenFailureCorrupt
	fastOpenFailureOther
	fastOpenFailureCount
)

func (r fastOpenFailureReason) String() string {
	switch r {
	case fastOpenFailureNoQuorum:
		return "no_quorum"
	case fastOpenFailureUnsupported:
		return "unsupported"
	case fastOpenFailureCorrupt:
		return "corrupt"
	default:
		return "other"
	}
}

type fastOpenFinalErrorCategory uint8

const (
	fastOpenFinalErrorNotFound fastOpenFinalErrorCategory = iota
	fastOpenFinalErrorReadQuorum
	fastOpenFinalErrorCorrupt
	fastOpenFinalErrorOther
	fastOpenFinalErrorCount
)

func (c fastOpenFinalErrorCategory) String() string {
	switch c {
	case fastOpenFinalErrorNotFound:
		return "not_found"
	case fastOpenFinalErrorReadQuorum:
		return "read_quorum"
	case fastOpenFinalErrorCorrupt:
		return "corrupt"
	default:
		return "other"
	}
}

type fastOpenMetrics struct {
	attempted       atomic.Uint64
	hits            atomic.Uint64
	fallbacks       atomic.Uint64
	replacementPath atomic.Uint64
	streamsOpened   atomic.Uint64
	replacementOpen atomic.Uint64
	streamCancels   atomic.Uint64
	connGot         atomic.Uint64
	connReused      atomic.Uint64
	connFresh       atomic.Uint64
	connWasIdle     atomic.Uint64
	tryNS           atomic.Uint64
	tryCount        atomic.Uint64
	openInfoNS      atomic.Uint64
	openInfoCount   atomic.Uint64
	bodyDecodeNS    atomic.Uint64
	bodyDecodeCount atomic.Uint64
	failures        [fastOpenFailureCount]atomic.Uint64
	finalErrors     [fastOpenFinalErrorCount]atomic.Uint64
}

var globalFastOpenMetrics fastOpenMetrics

func fastOpenRecordDuration(totalNS, count *atomic.Uint64, start time.Time) {
	totalNS.Add(uint64(time.Since(start)))
	count.Add(1)
}

func fastOpenRecordFailure(err error) {
	switch {
	case errors.Is(err, errErasureReadQuorum):
		globalFastOpenMetrics.failures[fastOpenFailureNoQuorum].Add(1)
	case errors.Is(err, errFileCorrupt), errors.Is(err, errFastOpenFrameBadCRC), errors.Is(err, errFastOpenFrameBadPayload), errors.Is(err, errFastOpenFrameBadBitrot):
		globalFastOpenMetrics.failures[fastOpenFailureCorrupt].Add(1)
	default:
		globalFastOpenMetrics.failures[fastOpenFailureOther].Add(1)
	}
}

func fastOpenRecordFinalError(ctx context.Context, err error) {
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return
	}
	switch {
	case isErrObjectNotFound(err), isErrVersionNotFound(err), errors.Is(err, errFileNotFound), errors.Is(err, errFileVersionNotFound):
		globalFastOpenMetrics.finalErrors[fastOpenFinalErrorNotFound].Add(1)
	case isErrReadQuorum(err), errors.Is(err, errErasureReadQuorum):
		globalFastOpenMetrics.finalErrors[fastOpenFinalErrorReadQuorum].Add(1)
	case errors.Is(err, errFileCorrupt), errors.Is(err, errFastOpenFrameBadCRC), errors.Is(err, errFastOpenFrameBadPayload), errors.Is(err, errFastOpenFrameBadBitrot):
		globalFastOpenMetrics.finalErrors[fastOpenFinalErrorCorrupt].Add(1)
	default:
		globalFastOpenMetrics.finalErrors[fastOpenFinalErrorOther].Add(1)
	}
}

type fastOpenGETMetricsTracker struct {
	replacementRecorded bool
	failureRecorded     bool
}

func (t *fastOpenGETMetricsTracker) recordReplacement() {
	if t == nil || t.replacementRecorded {
		return
	}
	globalFastOpenMetrics.replacementPath.Add(1)
	t.replacementRecorded = true
}

func (t *fastOpenGETMetricsTracker) recordFailure(err error) {
	if t == nil || t.failureRecorded || err == nil {
		return
	}
	fastOpenRecordFailure(err)
	t.failureRecorded = true
}

func (t *fastOpenGETMetricsTracker) recordFallback(err error) {
	globalFastOpenMetrics.fallbacks.Add(1)
	if t == nil || t.failureRecorded {
		return
	}
	if err != nil {
		t.recordFailure(err)
		return
	}
	globalFastOpenMetrics.failures[fastOpenFailureUnsupported].Add(1)
	t.failureRecorded = true
}

type fastOpenNoFallbackError struct {
	cause error
}

func (e fastOpenNoFallbackError) Error() string {
	if e.cause == nil {
		return errFastGetNoFallback.Error()
	}
	return errFastGetNoFallback.Error() + ": " + e.cause.Error()
}

func (e fastOpenNoFallbackError) Unwrap() []error {
	// Keep this as a multi-error so toObjectErr's single-error unwrap leaves the
	// diagnostic wrapper intact while errors.Is can match both values.
	if e.cause == nil {
		return []error{errFastGetNoFallback}
	}
	return []error{errFastGetNoFallback, e.cause}
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

// tryFastOpenGET returns an explicit outcome so fallback causes can be retained
// for diagnostics without being mistaken for terminal GET errors by the caller.
func (er erasureObjects) tryFastOpenGET(ctx context.Context, bucket, object string, rs *HTTPRangeSpec, h http.Header, opts ObjectOptions, nsUnlocker func()) fastOpenGETResult {
	start := time.Now()
	defer func() {
		fastOpenRecordDuration(&globalFastOpenMetrics.tryNS, &globalFastOpenMetrics.tryCount, start)
	}()
	var metrics fastOpenGETMetricsTracker
	openStart := time.Now()
	infoResult := er.openFastOpenGETInfo(ctx, bucket, object, opts, &metrics)
	fastOpenRecordDuration(&globalFastOpenMetrics.openInfoNS, &globalFastOpenMetrics.openInfoCount, openStart)
	switch infoResult.outcome {
	case fastOpenGETFallback:
		metrics.recordFallback(infoResult.err)
		return fastOpenGETResult{outcome: fastOpenGETFallback, err: infoResult.err}
	case fastOpenGETTerminalError:
		return fastOpenGETResult{outcome: fastOpenGETTerminalError, err: toObjectErr(infoResult.err, bucket, object)}
	}
	info := infoResult.info

	objInfo := info.fi.ToObjectInfo(bucket, object, opts.Versioned || opts.VersionSuspended)
	// Metadata-only outcomes do not need shard readers. Close any streams opened
	// during selection before returning the canonical GET result for the object.
	if objInfo.DeleteMarker {
		closeBitrotReaders(info.readers)
		if opts.VersionID == "" {
			return fastOpenGETResult{
				reader:  &GetObjectReader{ObjInfo: objInfo},
				outcome: fastOpenGETTerminalError,
				err:     toObjectErr(errFileNotFound, bucket, object),
			}
		}
		return fastOpenGETResult{
			reader:  &GetObjectReader{ObjInfo: objInfo},
			outcome: fastOpenGETTerminalError,
			err:     toObjectErr(errMethodNotAllowed, bucket, object),
		}
	}

	if crypto.SSEC.IsEncrypted(objInfo.UserDefined) && opts.ReplicationRequest {
		opts.NoDecryption = true
	}

	if objInfo.Size == 0 {
		closeBitrotReaders(info.readers)
		// The constructor evaluates preconditions and runs any cleanup functions
		// itself on failure. Attach the namespace unlock only after it succeeds so
		// an error leaves ownership with GetObjectNInfo's defer.
		gr, err := NewGetObjectReaderFromReader(bytes.NewReader(nil), objInfo, opts)
		if err != nil {
			return fastOpenGETResult{reader: gr, outcome: fastOpenGETTerminalError, err: err}
		}
		return fastOpenGETResult{reader: gr.WithCleanupFuncs(nsUnlocker), outcome: fastOpenGETSuccess}
	}

	if objInfo.IsRemote() {
		closeBitrotReaders(info.readers)
		gr, err := getTransitionedObjectReader(ctx, bucket, object, rs, h, objInfo, opts)
		if err != nil {
			return fastOpenGETResult{outcome: fastOpenGETTerminalError, err: err}
		}
		return fastOpenGETResult{reader: gr.WithCleanupFuncs(nsUnlocker), outcome: fastOpenGETSuccess}
	}

	fn, off, length, err := NewGetObjectReader(rs, objInfo, opts, h)
	if err != nil {
		closeBitrotReaders(info.readers)
		return fastOpenGETResult{outcome: fastOpenGETTerminalError, err: err}
	}
	if off != 0 {
		closeBitrotReaders(info.readers)
		metrics.recordFallback(nil)
		return fastOpenGETResult{outcome: fastOpenGETFallback}
	}

	pr, pw := xioutil.WaitPipe()
	go func() {
		err := er.getObjectWithFastOpenInfo(ctx, bucket, object, off, length, pw, info)
		if err != nil {
			fastOpenRecordFinalError(ctx, err)
		}
		pw.CloseWithError(err)
	}()

	pipeCloser := func() {
		pr.CloseWithError(nil)
	}
	gr, err := fn(pr, h, pipeCloser, nsUnlocker)
	if err != nil {
		return fastOpenGETResult{reader: gr, outcome: fastOpenGETTerminalError, err: err}
	}
	return fastOpenGETResult{reader: gr, outcome: fastOpenGETSuccess}
}

// openFastOpenGETInfo consumes only the FastOpen frame from each opened stream.
// Body streams are left positioned immediately after their frame and are either
// selected for decode or closed. It opens the first wave and then any remaining
// online disks needed to recover from pre-commit FastOpen failures.
func (er erasureObjects) openFastOpenGETInfo(ctx context.Context, bucket, object string, opts ObjectOptions, metrics *fastOpenGETMetricsTracker) fastOpenGETInfoResult {
	disks := er.getDisks()
	openCount := er.fastOpenInitialOpenCount()
	selected := selectFastOpenGETDisks(disks, openCount, bucket, object)
	if len(selected) == 0 {
		return fastOpenGETInfoResult{outcome: fastOpenGETFallback}
	}

	reads := openFastOpenGETReads(ctx, disks, selected, bucket, object, opts.VersionID)

	info, ok, err := er.pickFastOpenGETInfo(ctx, bucket, object, disks, reads, opts)
	if ok {
		if err != nil {
			closeFastOpenGETReadsExcept(ctx, reads, nil)
			return fastOpenGETInfoResult{info: info, outcome: fastOpenGETTerminalError, err: err}
		}
		return fastOpenGETInfoResult{info: info, outcome: fastOpenGETSuccess}
	}

	if err != nil {
		remaining := selectRemainingFastOpenGETDisks(disks, selected)
		if len(remaining) > 0 {
			metrics.recordReplacement()
			metrics.recordFailure(err)
			reads = append(reads, openFastOpenGETReads(ctx, disks, remaining, bucket, object, opts.VersionID)...)
			info, ok, err = er.pickFastOpenGETInfo(ctx, bucket, object, disks, reads, opts)
			if ok {
				if err != nil {
					closeFastOpenGETReadsExcept(ctx, reads, nil)
					return fastOpenGETInfoResult{info: info, outcome: fastOpenGETTerminalError, err: err}
				}
				return fastOpenGETInfoResult{info: info, outcome: fastOpenGETSuccess}
			}
		}
	}

	closeFastOpenGETReadsExcept(ctx, reads, nil)
	if err != nil {
		metrics.recordFailure(err)
	}
	return fastOpenGETInfoResult{outcome: fastOpenGETFallback, err: err}
}

func openFastOpenGETReads(ctx context.Context, disks []StorageAPI, selected []int, bucket, object, versionID string) []fastOpenGETRead {
	reads := make([]fastOpenGETRead, len(selected))
	g := errgroup.WithNErrs(len(selected))
	for gi, di := range selected {
		gi, di := gi, di
		disk := disks[di]
		g.Go(func() error {
			reads[gi] = openFastOpenGETRead(ctx, disk, di, bucket, object, versionID)
			return reads[gi].err
		}, gi)
	}
	g.Wait()
	return reads
}

func selectRemainingFastOpenGETDisks(disks []StorageAPI, selected []int) []int {
	seen := make(map[int]bool, len(selected))
	for _, idx := range selected {
		seen[idx] = true
	}
	remaining := make([]int, 0, len(disks)-len(seen))
	for i, disk := range disks {
		if seen[i] || disk == nil || !disk.IsOnline() {
			continue
		}
		remaining = append(remaining, i)
	}
	return remaining
}

// openFastOpenGETRead owns a cancellable child context for exactly one disk
// stream. Closing the returned rc cancels the remote/local FastOpenPart work in
// addition to closing the body reader.
func openFastOpenGETRead(ctx context.Context, disk StorageAPI, diskIndex int, bucket, object, versionID string) fastOpenGETRead {
	return openFastOpenGETReadAt(ctx, disk, diskIndex, bucket, object, versionID, 0, -1)
}

func openFastOpenGETReadAt(ctx context.Context, disk StorageAPI, diskIndex int, bucket, object, versionID string, offset, length int64) fastOpenGETRead {
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
		Offset:     offset,
		Length:     length,
	})
	if err != nil {
		cancel()
		r.err = err
		return r
	}
	globalFastOpenMetrics.streamsOpened.Add(1)
	if offset > 0 {
		globalFastOpenMetrics.replacementOpen.Add(1)
	}
	_, frame, err := readFastOpenFrame(rc)
	if err != nil {
		cancel()
		globalFastOpenMetrics.streamCancels.Add(1)
		rc.Close()
		r.err = err
		return r
	}
	rc = &fastOpenCancelReadCloser{rc: rc, cancel: cancel}
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
		closeFastOpenGETReadsExcept(ctx, reads, nil)
		return info, true, nil
	}
	readers, prefer, used, ok, err := buildFastOpenGETReaders(ctx, bucket, object, opts.VersionID, fi, reads, onlineMeta, onlineDisks, disks)
	if !ok {
		return fastOpenGETInfo{}, false, err
	}
	closeFastOpenGETReadsExcept(ctx, reads, used)
	info.readers = readers
	info.prefer = prefer
	return info, true, nil
}

// buildFastOpenGETReaders transfers ownership of selected body streams to the
// returned ReaderAt slice. The used map tells the caller which opened streams
// must stay live; every other opened stream can be closed immediately.
func buildFastOpenGETReaders(ctx context.Context, bucket, object, versionID string, fi FileInfo, reads []fastOpenGETRead, metaArr []FileInfo, onlineDisks []StorageAPI, disks []StorageAPI) ([]io.ReaderAt, []bool, map[int]bool, bool, error) {
	readByDisk := make(map[int]int, len(reads))
	for i := range reads {
		readByDisk[reads[i].diskIndex] = i
	}

	checksumInfo := fi.Erasure.GetChecksumInfo(1)
	if checksumInfo.Algorithm != HighwayHash256S {
		// The stream reader below understands Buckit's streaming bitrot layout.
		// Other algorithms/modes need canonical GET unless a matching FastOpen
		// reader is added.
		return nil, nil, nil, false, nil
	}

	readers := make([]io.ReaderAt, len(fi.Erasure.Distribution))
	prefer := make([]bool, len(readers))
	used := make(map[int]bool)
	unavailableDisks := make(map[int]bool)
	hasInline := false
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
		// Bind the per-disk shard index to the winning distribution before the
		// stream can contribute data. Bitrot protects the shard bytes, but it does
		// not prove that those bytes belong in the claimed erasure slot.
		if diskIndex >= len(fi.Erasure.Distribution) {
			unavailableDisks[diskIndex] = true
			continue
		}
		expectedIndex := fi.Erasure.Distribution[diskIndex]
		pos := expectedIndex - 1
		if metaArr[diskIndex].Erasure.Index != expectedIndex || pos < 0 || pos >= len(readers) || readers[pos] != nil {
			// Exclude a known-invalid disk from the replacement capacity gate and
			// pool. Otherwise it can make FastOpen commit before the replacement
			// frame rejects the same mismatch during body decode.
			unavailableDisks[diskIndex] = true
			continue
		}
		if mode == FastOpenBodyInline {
			hasInline = true
		}
		readers[pos] = newFastOpenStreamingBitrotReader(ctx, reads[readIndex].rc, checksumInfo.Algorithm, fi.Erasure.ShardSize(), fi.Erasure.ShardFileSize(fi.Parts[0].Size))
		prefer[pos] = true
		used[readIndex] = true
		unavailableDisks[diskIndex] = true
	}

	candidates := fastOpenReplacementCandidates(disks, unavailableDisks)
	inlineReplacement := !hasInline || fastOpenInlineReplacementSafe(fi)
	if inlineReplacement && len(used)+len(candidates) >= fi.Erasure.DataBlocks {
		pool := newFastOpenReplacementPool(ctx, disks, unavailableDisks, bucket, object, versionID, fi, checksumInfo.Algorithm)
		for pos := range readers {
			if readers[pos] == nil {
				readers[pos] = &fastOpenLazyReplacementReader{pool: pool, slot: pos}
			}
		}
	}
	if fastOpenReaderCount(readers) < fi.Erasure.DataBlocks {
		return nil, nil, nil, false, errErasureReadQuorum
	}
	return readers, prefer, used, true, nil
}

func fastOpenInlineReplacementSafe(fi FileInfo) bool {
	if len(fi.Parts) != 1 {
		return false
	}
	return fi.Erasure.ShardFileSize(fi.Parts[0].Size) <= fi.Erasure.ShardSize()
}

func fastOpenReplacementCandidates(disks []StorageAPI, usedDisks map[int]bool) []int {
	candidates := make([]int, 0, len(disks)-len(usedDisks))
	for i, disk := range disks {
		if usedDisks[i] || disk == nil || !disk.IsOnline() {
			continue
		}
		candidates = append(candidates, i)
	}
	return candidates
}

func fastOpenReaderCount(readers []io.ReaderAt) int {
	var n int
	for _, reader := range readers {
		if reader != nil {
			n++
		}
	}
	return n
}

// getObjectWithFastOpenInfo decodes already-open encoded shard streams through
// the same erasure decoder used by canonical GET.
func (er erasureObjects) getObjectWithFastOpenInfo(ctx context.Context, bucket, object string, startOffset int64, length int64, writer io.Writer, info fastOpenGETInfo) error {
	defer func() {
		closeBitrotReaders(info.readers)
	}()
	decodeStart := time.Now()
	_, err := er.decodeFastOpenGETRange(ctx, bucket, object, startOffset, length, writer, info)
	fastOpenRecordDuration(&globalFastOpenMetrics.bodyDecodeNS, &globalFastOpenMetrics.bodyDecodeCount, decodeStart)
	return err
}

func (er erasureObjects) decodeFastOpenGETRange(ctx context.Context, bucket, object string, startOffset int64, length int64, writer io.Writer, info fastOpenGETInfo) (int64, error) {
	fi := info.fi
	if length < 0 {
		length = fi.Size - startOffset
	}
	if startOffset > fi.Size || startOffset+length > fi.Size {
		return -1, InvalidRange{startOffset, length, fi.Size}
	}
	if length == 0 {
		return 0, nil
	}

	partSize := fi.Parts[0].Size
	erasure, err := NewErasure(ctx, fi.Erasure.DataBlocks, fi.Erasure.ParityBlocks, fi.Erasure.BlockSize)
	if err != nil {
		return -1, toObjectErr(err, bucket, object)
	}

	written, err := erasure.Decode(ctx, writer, info.readers, startOffset, length, partSize, info.prefer)
	if err != nil {
		if written == length && (errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt)) {
			return written, nil
		}
		return written, toObjectErr(err, bucket, object)
	}
	return written, nil
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
func closeFastOpenGETReadsExcept(ctx context.Context, reads []fastOpenGETRead, used map[int]bool) {
	for i := range reads {
		if reads[i].rc == nil || used[i] {
			continue
		}
		if reads[i].frame.BodyMode == FastOpenBodyShard || reads[i].frame.BodyMode == FastOpenBodyInline {
			globalFastOpenMetrics.streamCancels.Add(1)
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

type fastOpenReplacementPool struct {
	ctx       context.Context
	disks     []StorageAPI
	slotDisk  []int
	bucket    string
	object    string
	versionID string
	fi        FileInfo
	algo      BitrotAlgorithm
	shardSize int64
	hashSize  int64
	bodyLen   int64

	mu      sync.Mutex
	engaged map[int]bool
}

func newFastOpenReplacementPool(ctx context.Context, disks []StorageAPI, engaged map[int]bool, bucket, object, versionID string, fi FileInfo, algo BitrotAlgorithm) *fastOpenReplacementPool {
	engagedCopy := make(map[int]bool, len(engaged))
	for idx := range engaged {
		engagedCopy[idx] = true
	}
	slotDisk := make([]int, len(fi.Erasure.Distribution))
	for i := range slotDisk {
		slotDisk[i] = -1
	}
	// Distribution maps disk-array index to erasure index. Use it to open the
	// one disk that can satisfy each lazy reader slot instead of probing
	// unrelated spares and discovering their Index from frames.
	for diskIndex, erasureIndex := range fi.Erasure.Distribution {
		slot := erasureIndex - 1
		if slot >= 0 && slot < len(slotDisk) && slotDisk[slot] == -1 {
			slotDisk[slot] = diskIndex
		}
	}
	shardSize := fi.Erasure.ShardSize()
	return &fastOpenReplacementPool{
		ctx:       ctx,
		disks:     disks,
		slotDisk:  slotDisk,
		bucket:    bucket,
		object:    object,
		versionID: versionID,
		fi:        fi,
		algo:      algo,
		shardSize: shardSize,
		hashSize:  int64(algo.New().Size()),
		bodyLen:   bitrotShardFileSize(fi.Erasure.ShardFileSize(fi.Parts[0].Size), shardSize, algo),
		engaged:   engagedCopy,
	}
}

func (p *fastOpenReplacementPool) open(slot int, shardOffset int64) (io.ReadCloser, error) {
	bodyOffset := (shardOffset/p.shardSize)*p.hashSize + shardOffset
	if bodyOffset < 0 || bodyOffset >= p.bodyLen {
		return nil, errFileCorrupt
	}
	diskIndex, disk, ok := p.claimSlotDisk(slot)
	if !ok {
		return nil, errFileNotFound
	}
	read := openFastOpenGETReadAt(p.ctx, disk, diskIndex, p.bucket, p.object, p.versionID, bodyOffset, -1)
	if read.err != nil {
		p.releaseSlotDisk(diskIndex)
		return nil, read.err
	}
	if err := p.validateFrame(slot, bodyOffset, read.frame); err != nil {
		globalFastOpenMetrics.streamCancels.Add(1)
		read.rc.Close()
		p.releaseSlotDisk(diskIndex)
		return nil, err
	}
	return read.rc, nil
}

func (p *fastOpenReplacementPool) claimSlotDisk(slot int) (int, StorageAPI, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if slot < 0 || slot >= len(p.slotDisk) {
		return 0, nil, false
	}
	diskIndex := p.slotDisk[slot]
	if diskIndex < 0 || diskIndex >= len(p.disks) || p.engaged[diskIndex] {
		return 0, nil, false
	}
	disk := p.disks[diskIndex]
	if disk == nil || !disk.IsOnline() {
		return 0, nil, false
	}
	p.engaged[diskIndex] = true
	return diskIndex, disk, true
}

func (p *fastOpenReplacementPool) releaseSlotDisk(diskIndex int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.engaged, diskIndex)
}

func (p *fastOpenReplacementPool) validateFrame(slot int, bodyOffset int64, frame CoalescedMetadataFrame) error {
	if frame.Status != FastOpenStatusOK {
		if frame.Status == FastOpenStatusNotFound || frame.Status == FastOpenStatusVersionNotFound {
			return errFileNotFound
		}
		return errFileCorrupt
	}
	switch frame.BodyMode {
	case FastOpenBodyShard:
		if frame.BodyLen != p.bodyLen-bodyOffset {
			return errFileCorrupt
		}
	case FastOpenBodyInline:
		if bodyOffset != 0 || !fastOpenInlineReplacementSafe(p.fi) || frame.BodyLen != p.bodyLen {
			return errFileCorrupt
		}
	default:
		return errFileCorrupt
	}
	fi, err := fastOpenGETMetaToFileInfo(p.bucket, p.object, frame.Status, frame.Meta)
	if err != nil {
		return err
	}
	if !fi.IsValid() || fi.Size != p.fi.Size || fi.VersionID != p.fi.VersionID {
		return errFileCorrupt
	}
	if len(fi.Parts) != 1 || len(p.fi.Parts) != 1 {
		return errFileCorrupt
	}
	if fi.Parts[0].Number != p.fi.Parts[0].Number || fi.Parts[0].Size != p.fi.Parts[0].Size || fi.Parts[0].ActualSize != p.fi.Parts[0].ActualSize {
		return errFileCorrupt
	}
	if !fi.Erasure.Equal(p.fi.Erasure) || fi.Erasure.Index != slot+1 {
		return errFileCorrupt
	}
	if fi.Erasure.GetChecksumInfo(1).Algorithm != p.algo {
		return errFileCorrupt
	}
	if p.fi.ModTime.IsZero() || p.fi.ModTime.Equal(timeSentinel) {
		if p.fi.Metadata["etag"] == "" || fi.Metadata["etag"] != p.fi.Metadata["etag"] {
			return errFileCorrupt
		}
	} else if !fi.ModTime.Equal(p.fi.ModTime) {
		return errFileCorrupt
	}
	return nil
}

type fastOpenLazyReplacementReader struct {
	pool   *fastOpenReplacementPool
	slot   int
	rc     io.ReadCloser
	reader *fastOpenStreamingBitrotReader
}

func (r *fastOpenLazyReplacementReader) Close() error {
	if r.reader != nil {
		return r.reader.Close()
	}
	if r.rc != nil {
		err := r.rc.Close()
		r.rc = nil
		return err
	}
	return nil
}

func (r *fastOpenLazyReplacementReader) ReadAt(buf []byte, offset int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if offset%r.pool.shardSize != 0 {
		return 0, errUnexpected
	}
	if r.reader == nil {
		rc, err := r.pool.open(r.slot, offset)
		if err != nil {
			return 0, err
		}
		r.rc = rc
		r.reader = newFastOpenStreamingBitrotReaderAt(r.pool.ctx, rc, r.pool.algo, r.pool.shardSize, r.pool.fi.Erasure.ShardFileSize(r.pool.fi.Parts[0].Size), offset)
	}
	return r.reader.ReadAt(buf, offset)
}

type fastOpenStreamingBitrotReader struct {
	ctx        context.Context
	rc         io.ReadCloser
	h          hash.Hash
	shardSize  int64
	shardFile  int64
	currOffset int64
	hashBytes  []byte
}

func newFastOpenStreamingBitrotReader(ctx context.Context, rc io.ReadCloser, algo BitrotAlgorithm, shardSize, shardFile int64) *fastOpenStreamingBitrotReader {
	return newFastOpenStreamingBitrotReaderAt(ctx, rc, algo, shardSize, shardFile, 0)
}

func newFastOpenStreamingBitrotReaderAt(ctx context.Context, rc io.ReadCloser, algo BitrotAlgorithm, shardSize, shardFile, currOffset int64) *fastOpenStreamingBitrotReader {
	h := algo.New()
	return &fastOpenStreamingBitrotReader{
		ctx:        ctx,
		rc:         rc,
		h:          h,
		shardSize:  shardSize,
		shardFile:  shardFile,
		currOffset: currOffset,
		hashBytes:  make([]byte, h.Size()),
	}
}

func (r *fastOpenStreamingBitrotReader) Close() error {
	if r.rc == nil {
		return nil
	}
	if r.currOffset < r.shardFile {
		globalFastOpenMetrics.streamCancels.Add(1)
	}
	err := r.rc.Close()
	r.rc = nil
	return err
}

func (r *fastOpenStreamingBitrotReader) ReadAt(buf []byte, offset int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	// FastOpen body streams are forward-only after open. Initial streams start at
	// shard offset 0; lazy replacement streams are reopened at the requested
	// shard offset before reaching this reader.
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

var (
	_ io.ReaderAt = (*fastOpenLazyReplacementReader)(nil)
	_ io.Closer   = (*fastOpenLazyReplacementReader)(nil)
	_ io.ReaderAt = (*fastOpenStreamingBitrotReader)(nil)
	_ io.Closer   = (*fastOpenStreamingBitrotReader)(nil)
)
