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
	"time"

	xhttp "github.com/buckit-io/buckit/internal/http"
	xioutil "github.com/buckit-io/buckit/internal/ioutil"
	"github.com/cespare/xxhash/v2"
	"github.com/minio/pkg/v3/sync/errgroup"
)

type singleTripHeaderRead struct {
	disk      StorageAPI
	diskIndex int
	header    singleTripHeader
	rc        io.ReadCloser // live stream (original path); nil when body was buffered
	body      []byte        // eager path: buffered encoded shard for a data shard
	err       error
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
	// Initiate exactly the read quorum of shadow reads, preferring local disks. A
	// healthy GET then issues the *minimum* number of reads (no wasted reads of shards
	// it won't decode) — what matters under disk-bound concurrent load, where opening
	// all M+N made the fast path read ~M+N shards vs the canonical path's M. If the
	// chosen set doesn't reach quorum agreement (failure / version skew), we return
	// !ok and the caller falls back to the canonical path (which reads all disks).
	dataCount := er.setDriveCount - er.defaultParityCount
	readQuorum := dataCount
	if dataCount == er.defaultParityCount {
		readQuorum = dataCount + 1 // strict majority when data == parity
	}
	openCount := readQuorum
	hedged := globalFastGetHedgeShards && readQuorum < len(disks)
	if hedged {
		openCount++
	}
	sel := selectSingleTripFastOpenDisks(disks, openCount, bucket, object)
	if hedged && len(sel) > readQuorum {
		return er.openSingleTripFastInfoHedged(ctx, bucket, object, disks, sel, readQuorum)
	}

	reads := make([]singleTripHeaderRead, len(sel))
	g := errgroup.WithNErrs(len(sel))
	for gi := range sel {
		di := sel[gi]
		g.Go(func() error {
			reads[gi] = openSingleTripHeader(ctx, disks[di], bucket, object)
			reads[gi].diskIndex = di
			return reads[gi].err
		}, gi)
	}
	g.Wait()

	info, _, ok := pickSingleTripFastInfo(reads)
	if !ok {
		return singleTripFastInfo{}, false
	}
	return info, true
}

type singleTripHeaderResult struct {
	readIndex int
	read      singleTripHeaderRead
}

func (er erasureObjects) openSingleTripFastInfoHedged(ctx context.Context, bucket, object string, disks []StorageAPI, sel []int, readQuorum int) (singleTripFastInfo, bool) {
	reads := make([]singleTripHeaderRead, len(sel))
	completed := make([]bool, len(sel))
	cancels := make([]context.CancelFunc, len(sel))
	results := make(chan singleTripHeaderResult, len(sel))

	for gi, di := range sel {
		readCtx, cancel := context.WithCancel(ctx)
		cancels[gi] = cancel
		go func(readIndex, diskIndex int) {
			r := openSingleTripHeader(readCtx, disks[diskIndex], bucket, object)
			r.diskIndex = diskIndex
			results <- singleTripHeaderResult{readIndex: readIndex, read: r}
		}(gi, di)
	}

	received := 0
	for received < len(sel) {
		res := <-results
		reads[res.readIndex] = res.read
		completed[res.readIndex] = true
		received++

		info, used, ok := pickSingleTripFastInfoHedged(reads, completed, readQuorum)
		if !ok {
			continue
		}

		cleanupSingleTripHedgedReads(reads, completed, used, cancels, results, received, len(sel))
		return info, true
	}

	closeSingleTripHeaderReadsExcept(reads, nil)
	for _, cancel := range cancels {
		cancel()
	}
	return singleTripFastInfo{}, false
}

func selectSingleTripFastOpenDisks(disks []StorageAPI, readQuorum int, bucket, object string) []int {
	if readQuorum <= 0 {
		return nil
	}
	if globalFastGetSpreadSelection {
		return selectSingleTripSpreadDisks(len(disks), readQuorum, bucket, object)
	}

	locals := make([]int, 0, len(disks))
	remotes := make([]int, 0, len(disks))
	for i, d := range disks {
		if d != nil && d.IsLocal() {
			locals = append(locals, i)
			continue
		}
		remotes = append(remotes, i)
	}

	sel := make([]int, 0, readQuorum)
	for _, i := range locals {
		if len(sel) == readQuorum {
			return sel
		}
		sel = append(sel, i)
	}

	need := readQuorum - len(sel)
	if need <= 0 || len(remotes) == 0 {
		return sel
	}

	seed := xxhash.Sum64String(bucket + "\x00" + object)
	start := int(seed % uint64(len(remotes)))
	for n := 0; n < len(remotes) && need > 0; n++ {
		sel = append(sel, remotes[(start+n)%len(remotes)])
		need--
	}
	return sel
}

func selectSingleTripSpreadDisks(diskCount, readQuorum int, bucket, object string) []int {
	if diskCount <= 0 || readQuorum <= 0 {
		return nil
	}
	if readQuorum > diskCount {
		readQuorum = diskCount
	}
	seed := xxhash.Sum64String(bucket + "\x00" + object)
	start := int(seed % uint64(diskCount))
	sel := make([]int, 0, readQuorum)
	for n := 0; n < diskCount && len(sel) < readQuorum; n++ {
		sel = append(sel, (start+n)%diskCount)
	}
	return sel
}

func openSingleTripHeader(ctx context.Context, disk StorageAPI, bucket, object string) singleTripHeaderRead {
	return readSingleTripHeader(ctx, disk, bucket, object, true)
}

func readSingleTripHeader(ctx context.Context, disk StorageAPI, bucket, object string, keepStream bool) (r singleTripHeaderRead) {
	r = singleTripHeaderRead{disk: disk, diskIndex: -1, err: errDiskNotFound}
	if disk == nil || !disk.IsOnline() {
		return r
	}

	shadowPath := pathJoin(object, singleTripCurrentDir, "part.1")
	if keepStream && globalFastGetEager && !globalFastGetEagerSelected && disk.IsLocal() {
		raw, err := disk.ReadFileStream(ctx, bucket, shadowPath, 0, singleTripHeaderLen)
		if err != nil {
			r.err = err
			return r
		}

		headerBytes := make([]byte, singleTripHeaderLen)
		if _, err = io.ReadFull(raw, headerBytes); err != nil {
			raw.Close()
			r.err = err
			return r
		}
		raw.Close()

		header, err := decodeSingleTripHeader(headerBytes)
		if err != nil {
			r.err = err
			return r
		}
		if !fastGetHeaderValid(header) {
			r.err = errFileCorrupt
			return r
		}

		r.header = header
		if singleTripHeaderSingleBlock(header) {
			if berr := prefetchSingleTripFirstBlockBounded(ctx, disk, bucket, shadowPath, &r, header); berr != nil {
				r.err = berr
				return r
			}
			r.err = nil
			return r
		}

		// Keep multi-block eager body streams bounded. The streaming bitrot
		// reader sees this body stream as encoded offset 0, so a to-EOF read
		// would overrun the shard body after the header has been skipped.
		streamCtx, cancel := context.WithCancel(ctx)
		bodyRaw, berr := disk.ReadFileStream(streamCtx, bucket, shadowPath, singleTripHeaderLen, singleTripBitrotBodyLen(header))
		if berr != nil {
			cancel()
			r.err = berr
			return r
		}
		r.rc = &cancelReadCloser{rc: bodyRaw, cancel: cancel}
		if berr = prefetchSingleTripFirstBlock(&r, header); berr != nil {
			r.rc.Close()
			r.err = berr
			return r
		}
		r.err = nil
		return r
	}

	// Per-stream cancelable context: the fast path opens all M+N shadows to read
	// headers but only decodes M. Closing an unused shadow's stream must CANCEL its
	// (possibly remote) to-EOF read so the serving disk aborts immediately — a plain
	// Close leaves the server parked on a blocked write (backpressure), and under
	// concurrency those parked goroutines/grid streams collapse GET throughput.
	streamCtx, cancel := context.WithCancel(ctx)
	raw, err := disk.ReadFileStream(streamCtx, bucket, shadowPath, 0, -1)
	if err != nil {
		cancel()
		r.err = err
		return r
	}
	rc := &cancelReadCloser{rc: raw, cancel: cancel}
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
		r.header = header
		r.err = nil
		return r
	}

	// Eager prefetch is locality-based: for a LOCAL shard (data or parity) read its
	// first encoded block now, in this same goroutine, off the already-open stream —
	// overlapping it with the open and removing the header->body barrier. Decode
	// serves block 0 from this buffer and streams remaining blocks from the kept
	// stream (the stream is at EOF for single-block objects). REMOTE shards keep a
	// header-positioned live stream. All agreeing shards are handed to the decoder,
	// which (prefer-local) reads the cheapest M and reconstructs from parity if a
	// data shard is remote/absent — so the local prefetched blocks decode instantly.
	if globalFastGetEager && !globalFastGetEagerSelected && disk.IsLocal() {
		r.header = header
		r.rc = rc
		if berr := prefetchSingleTripFirstBlock(&r, header); berr != nil {
			rc.Close()
			r.err = berr
			return r
		}
		r.err = nil
		return r
	}

	// Remote shard (or eager disabled): keep the live stream; decode reads lazily.
	r.header = header
	r.rc = rc
	r.err = nil
	return r
}

// cancelReadCloser wraps a (possibly remote) shadow stream so Close cancels the
// read's context — aborting the serving disk's to-EOF handler instead of leaving it
// parked on a blocked write. Cancel BEFORE Close so a remote to-EOF body that would
// otherwise drain on Close returns immediately and frees the peer's request worker.
type cancelReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c *cancelReadCloser) Close() error {
	c.cancel()
	return c.rc.Close()
}

// multiReadCloser adapts a concatenated reader (first-block buffer + remaining
// live stream) into an io.ReadCloser whose Close releases the live stream.
type multiReadCloser struct {
	r io.Reader
	c io.Closer
}

func (m *multiReadCloser) Read(p []byte) (int, error) { return m.r.Read(p) }

func (m *multiReadCloser) Close() error {
	if m.c != nil {
		return m.c.Close()
	}
	return nil
}

func pickSingleTripFastInfo(reads []singleTripHeaderRead) (singleTripFastInfo, map[int]bool, bool) {
	info, used, ok := pickSingleTripFastInfoNoClose(reads)
	if !ok {
		closeSingleTripHeaderReadsExcept(reads, nil)
		return singleTripFastInfo{}, nil, false
	}
	// Cancel-close every stream NOT among the selected M (the unused N shards + any
	// disagreeing reads) immediately — before decode — so their parked server-side
	// to-EOF reads abort now rather than after the response. This is the throughput
	// fix under concurrency: it stops ~N held/blocked streams per GET. The selected
	// M's streams are owned by info.readers and closed after the response.
	closeSingleTripHeaderReadsExcept(reads, used)
	return info, used, true
}

func pickSingleTripFastInfoNoClose(reads []singleTripHeaderRead) (singleTripFastInfo, map[int]bool, bool) {
	var (
		bestHeader singleTripHeader
		bestGroup  map[uint16]int
		found      bool
	)
	for i := range reads {
		if reads[i].err != nil {
			continue
		}
		hi := reads[i].header
		total := uint16(hi.ErasureM) + uint16(hi.ErasureN)
		group := make(map[uint16]int)
		for j := range reads {
			if reads[j].err != nil {
				continue
			}
			hj := reads[j].header
			if hj.DirectSig != hi.DirectSig || !hj.commonEqual(hi) {
				continue
			}
			idx := hj.ErasureIndex
			if idx < 1 || idx > total {
				continue
			}
			if _, ok := group[idx]; ok {
				continue
			}
			group[idx] = j
		}
		// Any M distinct agreeing shards (data and/or parity) decode the object, so
		// require >= M — not specifically all M data indices. This lets the fast path
		// reconstruct from parity and choose the cheapest M shards.
		if len(group) < int(hi.ErasureM) {
			continue
		}
		if !found || hi.ModTimeNanos > bestHeader.ModTimeNanos {
			bestHeader = hi
			bestGroup = group
			found = true
		}
	}
	if !found {
		return singleTripFastInfo{}, nil, false
	}
	info, used, ok := buildSingleTripFastInfo(bestHeader, reads, bestGroup)
	if !ok {
		return singleTripFastInfo{}, nil, false
	}
	return info, used, true
}

type singleTripIndexedRead struct {
	readIndex int
	read      singleTripHeaderRead
}

func pickSingleTripFastInfoHedged(reads []singleTripHeaderRead, completed []bool, readQuorum int) (singleTripFastInfo, map[int]bool, bool) {
	if readQuorum <= 0 {
		return singleTripFastInfo{}, nil, false
	}
	primaryComplete := readQuorum <= len(reads)
	if primaryComplete {
		for i := 0; i < readQuorum; i++ {
			if !completed[i] {
				primaryComplete = false
				break
			}
		}
	}
	if primaryComplete {
		primary := make([]singleTripIndexedRead, 0, readQuorum)
		for i := 0; i < readQuorum; i++ {
			primary = append(primary, singleTripIndexedRead{readIndex: i, read: reads[i]})
		}
		if info, used, ok := pickSingleTripFastInfoIndexed(primary); ok {
			return info, used, true
		}
	}

	if countCompletedReads(completed) < readQuorum {
		return singleTripFastInfo{}, nil, false
	}
	items := make([]singleTripIndexedRead, 0, len(reads))
	for i := range reads {
		if completed[i] {
			items = append(items, singleTripIndexedRead{readIndex: i, read: reads[i]})
		}
	}
	return pickSingleTripFastInfoIndexed(items)
}

func pickSingleTripFastInfoIndexed(items []singleTripIndexedRead) (singleTripFastInfo, map[int]bool, bool) {
	compact := make([]singleTripHeaderRead, len(items))
	for i := range items {
		compact[i] = items[i].read
	}
	info, usedLocal, ok := pickSingleTripFastInfoNoClose(compact)
	if !ok {
		return singleTripFastInfo{}, nil, false
	}
	used := make(map[int]bool, len(usedLocal))
	for local := range usedLocal {
		used[items[local].readIndex] = true
	}
	return info, used, true
}

func countCompletedReads(completed []bool) int {
	count := 0
	for _, ok := range completed {
		if ok {
			count++
		}
	}
	return count
}

func cleanupSingleTripHedgedReads(reads []singleTripHeaderRead, completed []bool, used map[int]bool, cancels []context.CancelFunc, results <-chan singleTripHeaderResult, received, total int) {
	for i, cancel := range cancels {
		if !used[i] {
			cancel()
		}
	}
	for i := range reads {
		if completed[i] && !used[i] && reads[i].rc != nil {
			reads[i].rc.Close()
		}
	}

	if received >= total {
		return
	}
	go func() {
		for received < total {
			res := <-results
			if !used[res.readIndex] && res.read.rc != nil {
				res.read.rc.Close()
			}
			received++
		}
	}()
}

// buildSingleTripFastInfo selects the cheapest M shards from the agreeing group
// (prefer-local, then by erasure index for determinism), wiring only those M into an
// index-ordered readers slice and returning the read indices used (so the rest can
// be cancel-closed early). Selected shards may be data or parity; parallelReader
// reconstructs any missing data positions from the parity that ends up among the M.
// Handing the decoder exactly M readers requires no prefer reorder, and the unused
// streams are released up front instead of being held through decode.
func buildSingleTripFastInfo(header singleTripHeader, reads []singleTripHeaderRead, group map[uint16]int) (singleTripFastInfo, map[int]bool, bool) {
	m := int(header.ErasureM)
	totalShards := int(header.ErasureM) + int(header.ErasureN)
	fi := header.toFileInfo()

	type candidate struct {
		idx       uint16
		readIndex int
		local     bool
	}
	cands := make([]candidate, 0, len(group))
	for idx, readIndex := range group {
		d := reads[readIndex].disk
		cands = append(cands, candidate{idx: idx, readIndex: readIndex, local: d != nil && d.IsLocal()})
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].local != cands[b].local {
			return cands[a].local // local shards first (cheapest reads)
		}
		return cands[a].idx < cands[b].idx
	})

	onlineDisks := make([]StorageAPI, totalShards)
	readers := make([]io.ReaderAt, totalShards)
	used := make(map[int]bool, m)
	selected := make([]int, 0, m)
	for _, c := range cands {
		if len(used) >= m {
			break
		}
		pos := int(c.idx) - 1
		onlineDisks[pos] = reads[c.readIndex].disk
		used[c.readIndex] = true
		selected = append(selected, c.readIndex)
	}
	if globalFastGetEager && globalFastGetEagerSelected && selectedReadsHaveDisks(reads, selected) {
		if err := prefetchSingleTripSelectedReads(reads, selected, header); err != nil {
			return singleTripFastInfo{}, nil, false
		}
	}
	for _, c := range cands {
		if !used[c.readIndex] {
			continue
		}
		pos := int(c.idx) - 1
		readers[pos] = newSingleTripShardReaderAt(reads[c.readIndex], header.BitrotAlgo, fi.Erasure.ShardSize())
	}
	return singleTripFastInfo{fi: fi, onlineDisks: onlineDisks, readers: readers}, used, true
}

func selectedReadsHaveDisks(reads []singleTripHeaderRead, selected []int) bool {
	for _, readIndex := range selected {
		if reads[readIndex].disk == nil {
			return false
		}
	}
	return true
}

func prefetchSingleTripSelectedReads(reads []singleTripHeaderRead, selected []int, header singleTripHeader) error {
	g := errgroup.WithNErrs(len(selected))
	for gi, readIndex := range selected {
		g.Go(func() error {
			return prefetchSingleTripFirstBlock(&reads[readIndex], header)
		}, gi)
	}
	return errors.Join(g.Wait()...)
}

func prefetchSingleTripFirstBlock(r *singleTripHeaderRead, header singleTripHeader) error {
	if r.body != nil {
		return nil
	}
	if r.rc == nil {
		return errDiskNotFound
	}
	firstBuf := make([]byte, singleTripFirstEncodedBlockLen(header))
	if _, err := io.ReadFull(r.rc, firstBuf); err != nil {
		return err
	}
	r.body = firstBuf
	return nil
}

func prefetchSingleTripFirstBlockBounded(ctx context.Context, disk StorageAPI, bucket, shadowPath string, r *singleTripHeaderRead, header singleTripHeader) error {
	if r.body != nil {
		return nil
	}
	firstBuf := make([]byte, singleTripFirstEncodedBlockLen(header))
	rc, err := disk.ReadFileStream(ctx, bucket, shadowPath, singleTripHeaderLen, int64(len(firstBuf)))
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err = io.ReadFull(rc, firstBuf); err != nil {
		return err
	}
	r.body = firstBuf
	return nil
}

func singleTripHeaderSingleBlock(header singleTripHeader) bool {
	return header.Size <= header.ErasureBlockSize
}

func singleTripFirstEncodedBlockLen(header singleTripHeader) int64 {
	firstBlockOrig := header.ErasureBlockSize
	if header.Size < firstBlockOrig {
		firstBlockOrig = header.Size
	}
	hashSize := int64(header.BitrotAlgo.New().Size())
	return hashSize + ceilFrac(firstBlockOrig, int64(header.ErasureM))
}

func singleTripBitrotBodyLen(header singleTripHeader) int64 {
	if header.PartSize == 0 {
		return 0
	}
	shardFileSize := singleTripShardFileSize(header)
	shardSize := header.ErasureBlockSize / int64(header.ErasureM)
	return bitrotShardFileSize(shardFileSize, shardSize, header.BitrotAlgo)
}

func singleTripShardFileSize(header singleTripHeader) int64 {
	numShards := header.PartSize / header.ErasureBlockSize
	lastBlockSize := header.PartSize % header.ErasureBlockSize
	lastShardSize := ceilFrac(lastBlockSize, int64(header.ErasureM))
	return numShards*(header.ErasureBlockSize/int64(header.ErasureM)) + lastShardSize
}

// newSingleTripShardReaderAt builds a bitrot ReaderAt for a selected shard: from a
// prefetched first block (+ remaining live stream) for local shards, or from a live
// stream for remote shards.
func newSingleTripShardReaderAt(rd singleTripHeaderRead, algo BitrotAlgorithm, shardSize int64) io.ReaderAt {
	switch {
	case rd.body != nil:
		var rc io.ReadCloser
		if rd.rc != nil {
			rc = &multiReadCloser{r: io.MultiReader(bytes.NewReader(rd.body), rd.rc), c: rd.rc}
		} else {
			rc = io.NopCloser(bytes.NewReader(rd.body))
		}
		return newSingleTripStreamingBitrotReader(rc, algo, shardSize)
	case rd.rc != nil:
		return newSingleTripStreamingBitrotReader(rd.rc, algo, shardSize)
	default:
		return nil
	}
}

// closeSingleTripHeaderReadsExcept cancel-closes the streams of all reads NOT in
// used (pass used=nil to close all). The selected M's streams are owned by
// info.readers and closed after the response by closeBitrotReaders.
func closeSingleTripHeaderReadsExcept(reads []singleTripHeaderRead, used map[int]bool) {
	for i := range reads {
		if reads[i].rc == nil || used[i] {
			continue
		}
		reads[i].rc.Close() // cancelReadCloser.Close cancels the server-side to-EOF read
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

	// Exactly M readers are wired (prefer-local selection happened in
	// buildSingleTripFastInfo; the unused N were already cancel-closed), so no prefer
	// reorder is needed — parallelReader reads all M and reconstructs from any parity
	// among them.
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
