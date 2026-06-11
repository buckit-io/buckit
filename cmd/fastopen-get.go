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
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
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

type fastOpenProfileContextKey struct{}

type fastOpenProfileContext struct {
	id     uint64
	bucket string
	object string
	start  time.Time
}

var (
	globalFastOpenProfileID    atomic.Uint64
	globalFastOpenProfileLogMu sync.Mutex
)

func fastOpenProfileStart(ctx context.Context, bucket, object string) (context.Context, time.Time) {
	if !globalFastOpenProfile {
		return ctx, time.Time{}
	}
	now := fastOpenProfileTime()
	prof := &fastOpenProfileContext{
		id:     globalFastOpenProfileID.Add(1),
		bucket: bucket,
		object: object,
		start:  now,
	}
	ctx = context.WithValue(ctx, fastOpenProfileContextKey{}, prof)
	fastOpenProfileLog(ctx, "try_start")
	return ctx, now
}

func fastOpenProfileTime() time.Time {
	if !globalFastOpenProfile {
		return time.Time{}
	}
	return time.Now()
}

func fastOpenProfileLog(ctx context.Context, event string, fields ...any) {
	if !globalFastOpenProfile {
		return
	}
	prof, _ := ctx.Value(fastOpenProfileContextKey{}).(*fastOpenProfileContext)
	now := fastOpenProfileTime()

	globalFastOpenProfileLogMu.Lock()
	defer globalFastOpenProfileLogMu.Unlock()

	fmt.Fprintf(os.Stderr, "ts=%s component=fastopen_profile event=%s", now.Format(time.RFC3339Nano), event)
	if prof != nil {
		fmt.Fprintf(os.Stderr, " request_id=%d elapsed_ms=%.3f bucket=%q object=%q", prof.id, float64(now.Sub(prof.start).Nanoseconds())/1e6, prof.bucket, prof.object)
	}
	for i := 0; i+1 < len(fields); i += 2 {
		fmt.Fprintf(os.Stderr, " %v=%s", fields[i], fastOpenProfileValue(fields[i+1]))
	}
	fmt.Fprintln(os.Stderr)
}

func fastOpenProfileValue(v any) string {
	if v == nil {
		return "<nil>"
	}
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case error:
		return strconv.Quote(x.Error())
	case time.Time:
		return strconv.Quote(x.Format(time.RFC3339Nano))
	default:
		return fmt.Sprintf("%v", v)
	}
}

func fastOpenProfileDone(ctx context.Context, event string, start time.Time, fields ...any) {
	if !globalFastOpenProfile {
		return
	}
	fields = append(fields, "duration_ms", float64(time.Since(start).Nanoseconds())/1e6)
	fastOpenProfileLog(ctx, event, fields...)
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
	unsupported     atomic.Uint64
	replacementPath atomic.Uint64
	streamsOpened   atomic.Uint64
	replacementOpen atomic.Uint64
	streamCancels   atomic.Uint64
	connGot         atomic.Uint64
	connReused      atomic.Uint64
	connFresh       atomic.Uint64
	connWasIdle     atomic.Uint64
	failures        [fastOpenFailureCount]atomic.Uint64
	finalErrors     [fastOpenFinalErrorCount]atomic.Uint64
}

var globalFastOpenMetrics fastOpenMetrics

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
	ctx, start := fastOpenProfileStart(ctx, bucket, object)
	defer func() {
		fastOpenProfileDone(ctx, "try_done", start)
	}()
	var metrics fastOpenGETMetricsTracker
	openStart := fastOpenProfileTime()
	info, ok, err := er.openFastOpenGETInfo(ctx, bucket, object, opts, false, &metrics)
	fastOpenProfileDone(ctx, "try_open_info_first_done", openStart, "ok", ok, "err", err)
	if err != nil {
		if ok {
			fastOpenProfileLog(ctx, "try_return_object_error", "err", err)
			return nil, true, toObjectErr(err, bucket, object)
		}
		fastOpenProfileLog(ctx, "try_return_fallback_error", "err", err)
		return nil, false, err
	}
	if !ok {
		fastOpenProfileLog(ctx, "try_return_fallback_not_ok")
		return nil, false, nil
	}

	objInfo := info.fi.ToObjectInfo(bucket, object, opts.Versioned || opts.VersionSuspended)
	fastOpenProfileLog(ctx, "try_object_info", "size", objInfo.Size, "delete_marker", objInfo.DeleteMarker, "remote", objInfo.IsRemote(), "readers", fastOpenReaderCount(info.readers))
	// Metadata-only outcomes do not need shard readers. Close any streams opened
	// during selection before returning the canonical GET result for the object.
	if objInfo.DeleteMarker {
		closeStart := fastOpenProfileTime()
		closeBitrotReaders(info.readers)
		fastOpenProfileDone(ctx, "try_delete_marker_close_readers_done", closeStart)
		if opts.VersionID == "" {
			fastOpenProfileLog(ctx, "try_return_delete_marker_latest")
			return &GetObjectReader{
				ObjInfo: objInfo,
			}, true, toObjectErr(errFileNotFound, bucket, object)
		}
		fastOpenProfileLog(ctx, "try_return_delete_marker_versioned")
		return &GetObjectReader{
			ObjInfo: objInfo,
		}, true, toObjectErr(errMethodNotAllowed, bucket, object)
	}

	if crypto.SSEC.IsEncrypted(objInfo.UserDefined) && opts.ReplicationRequest {
		opts.NoDecryption = true
	}

	if objInfo.Size == 0 {
		closeStart := fastOpenProfileTime()
		closeBitrotReaders(info.readers)
		fastOpenProfileDone(ctx, "try_zero_size_close_readers_done", closeStart)
		readerStart := fastOpenProfileTime()
		gr, err := NewGetObjectReaderFromReader(bytes.NewReader(nil), objInfo, opts, nsUnlocker)
		fastOpenProfileDone(ctx, "try_zero_size_reader_done", readerStart, "err", err)
		return gr, true, err
	}

	if objInfo.IsRemote() {
		closeStart := fastOpenProfileTime()
		closeBitrotReaders(info.readers)
		fastOpenProfileDone(ctx, "try_remote_close_readers_done", closeStart)
		remoteStart := fastOpenProfileTime()
		gr, err := getTransitionedObjectReader(ctx, bucket, object, rs, h, objInfo, opts)
		fastOpenProfileDone(ctx, "try_remote_reader_done", remoteStart, "err", err)
		if err != nil {
			return nil, true, err
		}
		return gr.WithCleanupFuncs(nsUnlocker), true, nil
	}

	readerPlanStart := fastOpenProfileTime()
	fn, off, length, err := NewGetObjectReader(rs, objInfo, opts, h)
	fastOpenProfileDone(ctx, "try_new_get_object_reader_done", readerPlanStart, "off", off, "length", length, "err", err)
	if err != nil {
		closeBitrotReaders(info.readers)
		return nil, true, err
	}
	if off != 0 {
		closeBitrotReaders(info.readers)
		return nil, false, nil
	}

	pr, pw := xioutil.WaitPipe()
	go func() {
		bodyStart := fastOpenProfileTime()
		fastOpenProfileLog(ctx, "try_body_goroutine_start", "start_offset", off, "length", length)
		err := er.getObjectWithFastOpenInfo(ctx, bucket, object, off, length, pw, info)
		fastOpenProfileDone(ctx, "try_body_goroutine_decode_done", bodyStart, "err", err)
		if err != nil {
			fastOpenRecordFinalError(ctx, err)
		}
		pw.CloseWithError(err)
	}()

	pipeCloser := func() {
		pr.CloseWithError(nil)
	}
	fnStart := fastOpenProfileTime()
	gr, err := fn(pr, h, pipeCloser, nsUnlocker)
	fastOpenProfileDone(ctx, "try_final_reader_pipe_done", fnStart, "err", err)
	return gr, true, err
}

// openFastOpenGETInfo consumes only the FastOpen frame from each opened stream.
// Body streams are left positioned immediately after their frame and are either
// selected for decode or closed. A normal call opens the first wave and then any
// remaining online disks needed to recover from pre-commit FastOpen failures;
// allOnline skips the first wave and opens every online disk from scratch.
func (er erasureObjects) openFastOpenGETInfo(ctx context.Context, bucket, object string, opts ObjectOptions, allOnline bool, metrics *fastOpenGETMetricsTracker) (fastOpenGETInfo, bool, error) {
	start := fastOpenProfileTime()
	disks := er.getDisks()
	openCount := er.fastOpenInitialOpenCount()
	selected := selectFastOpenGETDisks(disks, openCount, bucket, object)
	if allOnline {
		selected = selectRemainingFastOpenGETDisks(disks, nil)
	}
	fastOpenProfileLog(ctx, "open_info_selected", "all_online", allOnline, "set_drive_count", er.setDriveCount, "default_parity", er.defaultParityCount, "open_count", openCount, "selected", selected)
	if len(selected) == 0 {
		fastOpenProfileDone(ctx, "open_info_done", start, "ok", false, "err", nil, "reason", "no_selected_disks")
		return fastOpenGETInfo{}, false, nil
	}

	openReadsStart := fastOpenProfileTime()
	reads := openFastOpenGETReads(ctx, disks, selected, bucket, object, opts.VersionID)
	fastOpenProfileDone(ctx, "open_info_open_reads_done", openReadsStart, "read_count", len(reads))

	pickStart := fastOpenProfileTime()
	info, ok, err := er.pickFastOpenGETInfo(ctx, bucket, object, disks, reads, opts)
	fastOpenProfileDone(ctx, "open_info_pick_first_done", pickStart, "ok", ok, "err", err)
	if ok {
		if err != nil {
			closeStart := fastOpenProfileTime()
			closeFastOpenGETReadsExcept(ctx, reads, nil)
			fastOpenProfileDone(ctx, "open_info_close_after_pick_error_done", closeStart)
		}
		fastOpenProfileDone(ctx, "open_info_done", start, "ok", true, "err", err, "all_online", allOnline)
		return info, true, err
	}

	exhausted := allOnline
	if err != nil && !allOnline {
		remaining := selectRemainingFastOpenGETDisks(disks, selected)
		fastOpenProfileLog(ctx, "open_info_first_failed", "err", err, "remaining", remaining)
		if len(remaining) > 0 {
			metrics.recordReplacement()
			metrics.recordFailure(err)
			openReadsStart = fastOpenProfileTime()
			reads = append(reads, openFastOpenGETReads(ctx, disks, remaining, bucket, object, opts.VersionID)...)
			fastOpenProfileDone(ctx, "open_info_open_remaining_done", openReadsStart, "total_read_count", len(reads), "remaining_count", len(remaining))
			pickStart = fastOpenProfileTime()
			info, ok, err = er.pickFastOpenGETInfo(ctx, bucket, object, disks, reads, opts)
			fastOpenProfileDone(ctx, "open_info_pick_remaining_done", pickStart, "ok", ok, "err", err)
			if ok {
				if err != nil {
					closeStart := fastOpenProfileTime()
					closeFastOpenGETReadsExcept(ctx, reads, nil)
					fastOpenProfileDone(ctx, "open_info_close_after_remaining_error_done", closeStart)
				}
				fastOpenProfileDone(ctx, "open_info_done", start, "ok", true, "err", err, "all_online", allOnline)
				return info, true, err
			}
		}
		exhausted = true
	}

	closeStart := fastOpenProfileTime()
	closeFastOpenGETReadsExcept(ctx, reads, nil)
	fastOpenProfileDone(ctx, "open_info_close_unselected_done", closeStart)
	if err != nil && exhausted {
		metrics.recordFailure(err)
		fastOpenProfileDone(ctx, "open_info_done", start, "ok", true, "err", err, "exhausted", exhausted)
		return info, true, err
	}
	fastOpenProfileDone(ctx, "open_info_done", start, "ok", false, "err", err, "exhausted", exhausted)
	return info, false, nil
}

func openFastOpenGETReads(ctx context.Context, disks []StorageAPI, selected []int, bucket, object, versionID string) []fastOpenGETRead {
	start := fastOpenProfileTime()
	fastOpenProfileLog(ctx, "open_reads_start", "selected", selected)
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
	success, failed := 0, 0
	for i := range reads {
		if reads[i].err == nil {
			success++
		} else {
			failed++
		}
	}
	fastOpenProfileDone(ctx, "open_reads_done", start, "selected", selected, "success", success, "failed", failed)
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
	start := fastOpenProfileTime()
	fastOpenProfileLog(ctx, "open_read_start", "disk_index", diskIndex, "is_local", disk != nil && disk.IsLocal(), "offset", offset, "length", length)
	r := fastOpenGETRead{
		disk:      disk,
		diskIndex: diskIndex,
		err:       errDiskNotFound,
	}
	readCtx, cancel := context.WithCancel(ctx)
	partStart := fastOpenProfileTime()
	rc, err := disk.FastOpenPart(readCtx, bucket, object, FastOpenPartRequest{
		Version:    fastOpenFrameVersion,
		VersionID:  versionID,
		PartNumber: 1,
		Offset:     offset,
		Length:     length,
	})
	fastOpenProfileDone(ctx, "open_read_fast_open_part_done", partStart, "disk_index", diskIndex, "offset", offset, "length", length, "err", err)
	if err != nil {
		cancel()
		r.err = err
		fastOpenProfileDone(ctx, "open_read_done", start, "disk_index", diskIndex, "err", err)
		return r
	}
	globalFastOpenMetrics.streamsOpened.Add(1)
	if offset > 0 {
		globalFastOpenMetrics.replacementOpen.Add(1)
	}
	frameStart := fastOpenProfileTime()
	_, frame, err := readFastOpenFrame(rc)
	fastOpenProfileDone(ctx, "open_read_frame_done", frameStart, "disk_index", diskIndex, "status", frame.Status, "body_mode", frame.BodyMode, "body_len", frame.BodyLen, "err", err)
	if err != nil {
		cancel()
		globalFastOpenMetrics.streamCancels.Add(1)
		rc.Close()
		r.err = err
		fastOpenProfileDone(ctx, "open_read_done", start, "disk_index", diskIndex, "err", err)
		return r
	}
	rc = &fastOpenCancelReadCloser{rc: rc, cancel: cancel}
	r.rc = rc
	r.frame = frame
	r.err = nil
	fastOpenProfileDone(ctx, "open_read_done", start, "disk_index", diskIndex, "status", frame.Status, "body_mode", frame.BodyMode, "body_len", frame.BodyLen, "err", nil)
	return r
}

// pickFastOpenGETInfo maps per-disk FastOpen frames into the same FileInfo,
// error, and online-disk arrays that canonical GET uses for quorum selection.
// It returns ok=false only for pre-commit cases where canonical GET should be
// tried instead.
func (er erasureObjects) pickFastOpenGETInfo(ctx context.Context, bucket, object string, disks []StorageAPI, reads []fastOpenGETRead, opts ObjectOptions) (fastOpenGETInfo, bool, error) {
	start := fastOpenProfileTime()
	fastOpenProfileLog(ctx, "pick_start", "read_count", len(reads))
	metaArr := make([]FileInfo, er.setDriveCount)
	errs := make([]error, er.setDriveCount)
	for i := range errs {
		errs[i] = errDiskOngoingReq
	}
	statusOK, statusDelete, statusNotFound, statusVersionNotFound, statusUnsupported, statusOther, readErrors := 0, 0, 0, 0, 0, 0, 0
	for i := range reads {
		read := &reads[i]
		if read.diskIndex < 0 || read.diskIndex >= len(errs) {
			continue
		}
		if read.err != nil {
			readErrors++
			errs[read.diskIndex] = read.err
			fastOpenProfileLog(ctx, "pick_read_error", "disk_index", read.diskIndex, "err", read.err)
			continue
		}
		switch read.frame.Status {
		case FastOpenStatusOK, FastOpenStatusDeleteMarker:
			if read.frame.Status == FastOpenStatusOK {
				statusOK++
			} else {
				statusDelete++
			}
			fi, err := fastOpenGETMetaToFileInfo(bucket, object, read.frame.Status, read.frame.Meta)
			if err != nil {
				errs[read.diskIndex] = err
				fastOpenProfileLog(ctx, "pick_meta_convert_error", "disk_index", read.diskIndex, "status", read.frame.Status, "err", err)
				continue
			}
			metaArr[read.diskIndex] = fi
			errs[read.diskIndex] = nil
		case FastOpenStatusNotFound:
			statusNotFound++
			errs[read.diskIndex] = errFileNotFound
		case FastOpenStatusVersionNotFound:
			statusVersionNotFound++
			errs[read.diskIndex] = errFileVersionNotFound
		case FastOpenStatusUnsupported:
			statusUnsupported++
			fastOpenProfileDone(ctx, "pick_done", start, "ok", false, "err", nil, "reason", "unsupported_status", "status_ok", statusOK, "status_unsupported", statusUnsupported, "read_errors", readErrors)
			return fastOpenGETInfo{}, false, nil
		default:
			statusOther++
			errs[read.diskIndex] = errFastOpenFrameBadStatus
		}
	}
	fastOpenProfileLog(ctx, "pick_status_counts", "ok", statusOK, "delete", statusDelete, "not_found", statusNotFound, "version_not_found", statusVersionNotFound, "unsupported", statusUnsupported, "other", statusOther, "read_errors", readErrors)

	// NotFound and VersionNotFound are disk-local frame statuses. They become an
	// object-level error only after the same quorum rules canonical GET applies.
	quorumStart := fastOpenProfileTime()
	readQuorum, _, err := objectQuorumFromMeta(ctx, metaArr, errs, er.defaultParityCount)
	fastOpenProfileDone(ctx, "pick_object_quorum_done", quorumStart, "read_quorum", readQuorum, "err", err)
	if err != nil {
		if errors.Is(err, errFileNotFound) || errors.Is(err, errFileVersionNotFound) {
			fastOpenProfileDone(ctx, "pick_done", start, "ok", true, "err", err, "reason", "object_not_found")
			return fastOpenGETInfo{}, true, err
		}
		fastOpenProfileDone(ctx, "pick_done", start, "ok", false, "err", err, "reason", "object_quorum")
		return fastOpenGETInfo{}, false, err
	}
	reduceStart := fastOpenProfileTime()
	if err = reduceReadQuorumErrs(ctx, errs, objectOpIgnoredErrs, readQuorum); err != nil {
		fastOpenProfileDone(ctx, "pick_reduce_quorum_done", reduceStart, "read_quorum", readQuorum, "err", err)
		if errors.Is(err, errFileNotFound) || errors.Is(err, errFileVersionNotFound) {
			fastOpenProfileDone(ctx, "pick_done", start, "ok", true, "err", err, "reason", "reduce_not_found")
			return fastOpenGETInfo{}, true, err
		}
		fastOpenProfileDone(ctx, "pick_done", start, "ok", false, "err", err, "reason", "reduce_quorum")
		return fastOpenGETInfo{}, false, err
	}
	fastOpenProfileDone(ctx, "pick_reduce_quorum_done", reduceStart, "read_quorum", readQuorum, "err", nil)
	onlineDisks, modTime, etag := listOnlineDisks(disks, metaArr, errs, readQuorum)
	onlineCount := 0
	for _, disk := range onlineDisks {
		if disk != nil {
			onlineCount++
		}
	}
	fastOpenProfileLog(ctx, "pick_online_disks", "online_count", onlineCount, "read_quorum", readQuorum, "etag", etag, "mod_time", modTime)
	pickFIStart := fastOpenProfileTime()
	fi, err := pickValidFileInfo(ctx, metaArr, modTime, etag, readQuorum)
	fastOpenProfileDone(ctx, "pick_valid_file_info_done", pickFIStart, "err", err)
	if err != nil {
		fastOpenProfileDone(ctx, "pick_done", start, "ok", false, "err", err, "reason", "pick_valid_file_info")
		return fastOpenGETInfo{}, false, err
	}
	fastOpenProfileLog(ctx, "pick_file_info", "size", fi.Size, "data_blocks", fi.Erasure.DataBlocks, "parity_blocks", fi.Erasure.ParityBlocks, "block_size", fi.Erasure.BlockSize, "distribution", fi.Erasure.Distribution)

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
	filteredOnlineCount := 0
	for _, disk := range onlineDisks {
		if disk != nil {
			filteredOnlineCount++
		}
	}
	fastOpenProfileLog(ctx, "pick_filtered_online", "online_count", filteredOnlineCount)

	info := fastOpenGETInfo{fi: fi}
	if fi.Deleted || fi.Size == 0 || fi.IsRemote() {
		closeStart := fastOpenProfileTime()
		closeFastOpenGETReadsExcept(ctx, reads, nil)
		fastOpenProfileDone(ctx, "pick_metadata_only_close_done", closeStart)
		fastOpenProfileDone(ctx, "pick_done", start, "ok", true, "err", nil, "metadata_only", true)
		return info, true, nil
	}
	buildStart := fastOpenProfileTime()
	readers, prefer, used, ok, err := buildFastOpenGETReaders(ctx, bucket, object, opts.VersionID, fi, reads, onlineMeta, onlineDisks, disks)
	fastOpenProfileDone(ctx, "pick_build_readers_done", buildStart, "ok", ok, "err", err, "used_count", len(used), "reader_count", fastOpenReaderCount(readers))
	if !ok {
		fastOpenProfileDone(ctx, "pick_done", start, "ok", false, "err", err, "reason", "build_readers")
		return fastOpenGETInfo{}, false, err
	}
	closeStart := fastOpenProfileTime()
	closeFastOpenGETReadsExcept(ctx, reads, used)
	fastOpenProfileDone(ctx, "pick_close_unused_done", closeStart, "used_count", len(used))
	info.readers = readers
	info.prefer = prefer
	fastOpenProfileDone(ctx, "pick_done", start, "ok", true, "err", nil, "reader_count", fastOpenReaderCount(readers), "used_count", len(used))
	return info, true, nil
}

// buildFastOpenGETReaders transfers ownership of selected body streams to the
// returned ReaderAt slice. The used map tells the caller which opened streams
// must stay live; every other opened stream can be closed immediately.
func buildFastOpenGETReaders(ctx context.Context, bucket, object, versionID string, fi FileInfo, reads []fastOpenGETRead, metaArr []FileInfo, onlineDisks []StorageAPI, disks []StorageAPI) ([]io.ReaderAt, []bool, map[int]bool, bool, error) {
	start := fastOpenProfileTime()
	fastOpenProfileLog(ctx, "build_readers_start", "read_count", len(reads), "data_blocks", fi.Erasure.DataBlocks, "parity_blocks", fi.Erasure.ParityBlocks)
	readByDisk := make(map[int]int, len(reads))
	for i := range reads {
		readByDisk[reads[i].diskIndex] = i
	}

	checksumInfo := fi.Erasure.GetChecksumInfo(1)
	if checksumInfo.Algorithm != HighwayHash256S {
		// The stream reader below understands Buckit's streaming bitrot layout.
		// Other algorithms/modes need canonical GET unless a matching FastOpen
		// reader is added.
		fastOpenProfileDone(ctx, "build_readers_done", start, "ok", false, "reason", "unsupported_checksum", "checksum", checksumInfo.Algorithm)
		return nil, nil, nil, false, nil
	}

	readers := make([]io.ReaderAt, len(fi.Erasure.Distribution))
	prefer := make([]bool, len(readers))
	used := make(map[int]bool)
	usedDisks := make(map[int]bool)
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
		if mode == FastOpenBodyInline {
			hasInline = true
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
		readers[pos] = newFastOpenStreamingBitrotReader(ctx, reads[readIndex].rc, checksumInfo.Algorithm, fi.Erasure.ShardSize(), fi.Erasure.ShardFileSize(fi.Parts[0].Size))
		prefer[pos] = true
		used[readIndex] = true
		usedDisks[diskIndex] = true
		fastOpenProfileLog(ctx, "build_reader_selected", "disk_index", diskIndex, "read_index", readIndex, "slot", pos, "body_mode", mode, "inline", mode == FastOpenBodyInline)
	}

	candidates := fastOpenReplacementCandidates(disks, usedDisks)
	inlineReplacement := !hasInline || fastOpenInlineReplacementSafe(fi)
	fastOpenProfileLog(ctx, "build_replacement_candidates", "used_count", len(used), "candidate_count", len(candidates), "has_inline", hasInline, "inline_replacement", inlineReplacement, "candidates", candidates)
	if inlineReplacement && len(used)+len(candidates) >= fi.Erasure.DataBlocks {
		pool := newFastOpenReplacementPool(ctx, disks, usedDisks, bucket, object, versionID, fi, checksumInfo.Algorithm)
		for pos := range readers {
			if readers[pos] == nil {
				readers[pos] = &fastOpenLazyReplacementReader{pool: pool, slot: pos}
				fastOpenProfileLog(ctx, "build_lazy_replacement_reader", "slot", pos)
			}
		}
	}
	if fastOpenReaderCount(readers) < fi.Erasure.DataBlocks {
		fastOpenProfileDone(ctx, "build_readers_done", start, "ok", false, "err", errErasureReadQuorum, "reader_count", fastOpenReaderCount(readers))
		return nil, nil, nil, false, errErasureReadQuorum
	}
	fastOpenProfileDone(ctx, "build_readers_done", start, "ok", true, "reader_count", fastOpenReaderCount(readers), "used_count", len(used), "candidate_count", len(candidates), "has_inline", hasInline)
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
	start := fastOpenProfileTime()
	defer func() {
		closeStart := fastOpenProfileTime()
		closeBitrotReaders(info.readers)
		fastOpenProfileDone(ctx, "body_close_readers_done", closeStart)
		fastOpenProfileDone(ctx, "body_done", start)
	}()
	fastOpenProfileLog(ctx, "body_start", "start_offset", startOffset, "length", length)
	decodeStart := fastOpenProfileTime()
	_, err := er.decodeFastOpenGETRange(ctx, bucket, object, startOffset, length, writer, info)
	fastOpenProfileDone(ctx, "body_decode_done", decodeStart, "err", err)
	return err
}

func (er erasureObjects) decodeFastOpenGETRange(ctx context.Context, bucket, object string, startOffset int64, length int64, writer io.Writer, info fastOpenGETInfo) (int64, error) {
	start := fastOpenProfileTime()
	fi := info.fi
	fastOpenProfileLog(ctx, "decode_start", "start_offset", startOffset, "length", length, "size", fi.Size, "reader_count", fastOpenReaderCount(info.readers), "data_blocks", fi.Erasure.DataBlocks, "parity_blocks", fi.Erasure.ParityBlocks)
	if length < 0 {
		length = fi.Size - startOffset
	}
	if startOffset > fi.Size || startOffset+length > fi.Size {
		fastOpenProfileDone(ctx, "decode_done", start, "written", -1, "err", InvalidRange{startOffset, length, fi.Size})
		return -1, InvalidRange{startOffset, length, fi.Size}
	}
	if length == 0 {
		fastOpenProfileDone(ctx, "decode_done", start, "written", 0, "err", nil, "reason", "zero_length")
		return 0, nil
	}

	partSize := fi.Parts[0].Size
	erasureStart := fastOpenProfileTime()
	erasure, err := NewErasure(ctx, fi.Erasure.DataBlocks, fi.Erasure.ParityBlocks, fi.Erasure.BlockSize)
	fastOpenProfileDone(ctx, "decode_new_erasure_done", erasureStart, "err", err)
	if err != nil {
		fastOpenProfileDone(ctx, "decode_done", start, "written", -1, "err", err)
		return -1, toObjectErr(err, bucket, object)
	}

	decodeStart := fastOpenProfileTime()
	written, err := erasure.Decode(ctx, writer, info.readers, startOffset, length, partSize, info.prefer)
	fastOpenProfileDone(ctx, "decode_erasure_decode_done", decodeStart, "written", written, "length", length, "err", err)
	if err != nil {
		if written == length && (errors.Is(err, errFileNotFound) || errors.Is(err, errFileCorrupt)) {
			fastOpenProfileDone(ctx, "decode_done", start, "written", written, "err", nil, "ignored_err", err)
			return written, nil
		}
		fastOpenProfileDone(ctx, "decode_done", start, "written", written, "err", err)
		return written, toObjectErr(err, bucket, object)
	}
	fastOpenProfileDone(ctx, "decode_done", start, "written", written, "err", nil)
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
	if globalFastOpenProfile {
		closed, kept := 0, 0
		for i := range reads {
			if reads[i].rc == nil {
				continue
			}
			if used[i] {
				kept++
			} else {
				closed++
			}
		}
		fastOpenProfileLog(ctx, "close_reads_except", "closed", closed, "kept", kept, "total", len(reads))
	}
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
	start := fastOpenProfileTime()
	fastOpenProfileLog(p.ctx, "replacement_open_start", "slot", slot, "shard_offset", shardOffset)
	bodyOffset := (shardOffset/p.shardSize)*p.hashSize + shardOffset
	if bodyOffset < 0 || bodyOffset >= p.bodyLen {
		fastOpenProfileDone(p.ctx, "replacement_open_done", start, "slot", slot, "body_offset", bodyOffset, "err", errFileCorrupt)
		return nil, errFileCorrupt
	}
	diskIndex, disk, ok := p.claimSlotDisk(slot)
	if !ok {
		fastOpenProfileDone(p.ctx, "replacement_open_done", start, "slot", slot, "body_offset", bodyOffset, "err", errFileNotFound, "reason", "claim_failed")
		return nil, errFileNotFound
	}
	fastOpenProfileLog(p.ctx, "replacement_claimed_disk", "slot", slot, "disk_index", diskIndex, "body_offset", bodyOffset)
	read := openFastOpenGETReadAt(p.ctx, disk, diskIndex, p.bucket, p.object, p.versionID, bodyOffset, -1)
	if read.err != nil {
		p.releaseSlotDisk(diskIndex)
		fastOpenProfileDone(p.ctx, "replacement_open_done", start, "slot", slot, "disk_index", diskIndex, "body_offset", bodyOffset, "err", read.err)
		return nil, read.err
	}
	if err := p.validateFrame(slot, bodyOffset, read.frame); err != nil {
		globalFastOpenMetrics.streamCancels.Add(1)
		read.rc.Close()
		p.releaseSlotDisk(diskIndex)
		fastOpenProfileDone(p.ctx, "replacement_open_done", start, "slot", slot, "disk_index", diskIndex, "body_offset", bodyOffset, "err", err, "reason", "validate_failed")
		return nil, err
	}
	fastOpenProfileDone(p.ctx, "replacement_open_done", start, "slot", slot, "disk_index", diskIndex, "body_offset", bodyOffset, "err", nil)
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
	start := fastOpenProfileTime()
	fastOpenProfileLog(p.ctx, "replacement_validate_start", "slot", slot, "body_offset", bodyOffset, "status", frame.Status, "body_mode", frame.BodyMode, "body_len", frame.BodyLen)
	if frame.Status != FastOpenStatusOK {
		if frame.Status == FastOpenStatusNotFound || frame.Status == FastOpenStatusVersionNotFound {
			fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileNotFound)
			return errFileNotFound
		}
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "bad_status")
		return errFileCorrupt
	}
	switch frame.BodyMode {
	case FastOpenBodyShard:
		if frame.BodyLen != p.bodyLen-bodyOffset {
			fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "bad_body_len")
			return errFileCorrupt
		}
	case FastOpenBodyInline:
		if bodyOffset != 0 || !fastOpenInlineReplacementSafe(p.fi) || frame.BodyLen != p.bodyLen {
			fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "bad_inline_body")
			return errFileCorrupt
		}
	default:
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "bad_body_mode")
		return errFileCorrupt
	}
	fi, err := fastOpenGETMetaToFileInfo(p.bucket, p.object, frame.Status, frame.Meta)
	if err != nil {
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", err)
		return err
	}
	if !fi.IsValid() || fi.Size != p.fi.Size || fi.VersionID != p.fi.VersionID {
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "identity_mismatch")
		return errFileCorrupt
	}
	if len(fi.Parts) != 1 || len(p.fi.Parts) != 1 {
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "part_count")
		return errFileCorrupt
	}
	if fi.Parts[0].Number != p.fi.Parts[0].Number || fi.Parts[0].Size != p.fi.Parts[0].Size || fi.Parts[0].ActualSize != p.fi.Parts[0].ActualSize {
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "part_mismatch")
		return errFileCorrupt
	}
	if !fi.Erasure.Equal(p.fi.Erasure) || fi.Erasure.Index != slot+1 {
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "erasure_mismatch")
		return errFileCorrupt
	}
	if fi.Erasure.GetChecksumInfo(1).Algorithm != p.algo {
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "checksum_mismatch")
		return errFileCorrupt
	}
	if p.fi.ModTime.IsZero() || p.fi.ModTime.Equal(timeSentinel) {
		if p.fi.Metadata["etag"] == "" || fi.Metadata["etag"] != p.fi.Metadata["etag"] {
			fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "etag_mismatch")
			return errFileCorrupt
		}
	} else if !fi.ModTime.Equal(p.fi.ModTime) {
		fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", errFileCorrupt, "reason", "modtime_mismatch")
		return errFileCorrupt
	}
	fastOpenProfileDone(p.ctx, "replacement_validate_done", start, "slot", slot, "err", nil)
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
	start := fastOpenProfileTime()
	if len(buf) == 0 {
		fastOpenProfileDone(r.pool.ctx, "lazy_read_at_done", start, "slot", r.slot, "offset", offset, "len", len(buf), "n", 0, "err", nil, "reason", "empty")
		return 0, nil
	}
	fastOpenProfileLog(r.pool.ctx, "lazy_read_at_start", "slot", r.slot, "offset", offset, "len", len(buf), "has_reader", r.reader != nil)
	if offset%r.pool.shardSize != 0 {
		fastOpenProfileDone(r.pool.ctx, "lazy_read_at_done", start, "slot", r.slot, "offset", offset, "len", len(buf), "n", 0, "err", errUnexpected)
		return 0, errUnexpected
	}
	if r.reader == nil {
		openStart := fastOpenProfileTime()
		rc, err := r.pool.open(r.slot, offset)
		fastOpenProfileDone(r.pool.ctx, "lazy_read_at_open_done", openStart, "slot", r.slot, "offset", offset, "err", err)
		if err != nil {
			fastOpenProfileDone(r.pool.ctx, "lazy_read_at_done", start, "slot", r.slot, "offset", offset, "len", len(buf), "n", 0, "err", err)
			return 0, err
		}
		r.rc = rc
		r.reader = newFastOpenStreamingBitrotReaderAt(r.pool.ctx, rc, r.pool.algo, r.pool.shardSize, r.pool.fi.Erasure.ShardFileSize(r.pool.fi.Parts[0].Size), offset)
	}
	n, err := r.reader.ReadAt(buf, offset)
	fastOpenProfileDone(r.pool.ctx, "lazy_read_at_done", start, "slot", r.slot, "offset", offset, "len", len(buf), "n", n, "err", err)
	return n, err
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
	start := fastOpenProfileTime()
	if len(buf) == 0 {
		fastOpenProfileDone(r.ctx, "stream_read_at_done", start, "offset", offset, "len", len(buf), "n", 0, "err", nil, "reason", "empty")
		return 0, nil
	}
	fastOpenProfileLog(r.ctx, "stream_read_at_start", "offset", offset, "len", len(buf), "curr_offset", r.currOffset, "shard_size", r.shardSize, "shard_file", r.shardFile)
	// FastOpen body streams are forward-only after open. Initial streams start at
	// shard offset 0; lazy replacement streams are reopened at the requested
	// shard offset before reaching this reader.
	if offset%r.shardSize != 0 || offset != r.currOffset {
		fastOpenProfileDone(r.ctx, "stream_read_at_done", start, "offset", offset, "len", len(buf), "n", 0, "err", errUnexpected)
		return 0, errUnexpected
	}
	hashReadStart := fastOpenProfileTime()
	if _, err := io.ReadFull(r.rc, r.hashBytes); err != nil {
		fastOpenProfileDone(r.ctx, "stream_read_hash_done", hashReadStart, "offset", offset, "err", err)
		fastOpenProfileDone(r.ctx, "stream_read_at_done", start, "offset", offset, "len", len(buf), "n", 0, "err", err)
		return 0, err
	}
	fastOpenProfileDone(r.ctx, "stream_read_hash_done", hashReadStart, "offset", offset, "err", nil)
	bodyStart := fastOpenProfileTime()
	if _, err := io.ReadFull(r.rc, buf); err != nil {
		fastOpenProfileDone(r.ctx, "stream_read_body_done", bodyStart, "offset", offset, "len", len(buf), "err", err)
		fastOpenProfileDone(r.ctx, "stream_read_at_done", start, "offset", offset, "len", len(buf), "n", 0, "err", err)
		return 0, err
	}
	fastOpenProfileDone(r.ctx, "stream_read_body_done", bodyStart, "offset", offset, "len", len(buf), "err", nil)
	hashStart := fastOpenProfileTime()
	r.h.Reset()
	r.h.Write(buf)
	if !bytes.Equal(r.h.Sum(nil), r.hashBytes) {
		fastOpenProfileDone(r.ctx, "stream_hash_verify_done", hashStart, "offset", offset, "len", len(buf), "err", errFileCorrupt)
		fastOpenProfileDone(r.ctx, "stream_read_at_done", start, "offset", offset, "len", len(buf), "n", 0, "err", errFileCorrupt)
		return 0, errFileCorrupt
	}
	fastOpenProfileDone(r.ctx, "stream_hash_verify_done", hashStart, "offset", offset, "len", len(buf), "err", nil)
	r.currOffset += int64(len(buf))
	fastOpenProfileDone(r.ctx, "stream_read_at_done", start, "offset", offset, "len", len(buf), "n", len(buf), "err", nil)
	return len(buf), nil
}

var _ io.ReaderAt = (*fastOpenLazyReplacementReader)(nil)
var _ io.Closer = (*fastOpenLazyReplacementReader)(nil)
var _ io.ReaderAt = (*fastOpenStreamingBitrotReader)(nil)
var _ io.Closer = (*fastOpenStreamingBitrotReader)(nil)
