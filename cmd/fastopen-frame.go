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
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"time"

	"github.com/tinylib/msgp/msgp"
)

const (
	fastOpenFrameVersion      uint16 = 1
	fastOpenFramePreludeLen          = 15
	fastOpenFrameMaxHeaderLen        = 4 << 20
)

var (
	fastOpenFrameMagic = [4]byte{'B', 'F', 'G', '1'}

	errFastOpenFrameBadMagic       = errors.New("fastopen frame: bad magic")
	errFastOpenFrameBadVersion     = errors.New("fastopen frame: bad version")
	errFastOpenFrameBadCRC         = errors.New("fastopen frame: bad crc")
	errFastOpenFrameHeaderTooLarge = errors.New("fastopen frame: header too large")
	errFastOpenFrameBadPayload     = errors.New("fastopen frame: bad payload")
	errFastOpenFrameBadBodyMode    = errors.New("fastopen frame: bad body mode")
	errFastOpenFrameBadStatus      = errors.New("fastopen frame: bad status")
	errFastOpenFrameBadBitrot      = errors.New("fastopen frame: bad bitrot algorithm")
)

// FastOpenPartFlags carries protocol flags for a FastOpenPart request.
type FastOpenPartFlags uint32

// FastOpenPartRequest describes a storage-node FastOpen part request.
type FastOpenPartRequest struct {
	Version    uint16
	VersionID  string
	PartNumber int
	Offset     int64
	Length     int64
	Flags      FastOpenPartFlags
}

// FastOpenFramePrelude is the fixed-size header at byte zero of every
// successful FastOpenPart stream.
type FastOpenFramePrelude struct {
	Magic       [4]byte
	Version     uint16
	HeaderLen   uint32
	HeaderCRC32 uint32
	BodyMode    FastOpenBodyMode
}

type FastOpenBodyMode uint8

const (
	FastOpenBodyShard FastOpenBodyMode = iota + 1
	FastOpenBodyInline
	FastOpenBodyMetadataOnly
	FastOpenBodyTransitioned
)

type FastOpenFrameStatus uint8

const (
	FastOpenStatusOK FastOpenFrameStatus = iota
	FastOpenStatusDeleteMarker
	FastOpenStatusNotFound
	FastOpenStatusVersionNotFound
	FastOpenStatusUnsupported
)

type CoalescedMetadataFrame struct {
	Status   FastOpenFrameStatus
	Meta     FastOpenGETMeta
	BodyMode FastOpenBodyMode
	BodyLen  int64
}

type FastOpenGETMeta struct {
	VersionID             string
	IsLatest              bool
	Legacy                bool
	ModTimeUnixNano       int64
	Size                  int64
	Metadata              map[string]string
	Transition            FastOpenTransitionMeta
	Part                  FastOpenPartMeta
	Erasure               FastOpenErasureMeta
	Checksum              []byte
	NumVersions           int
	SuccessorModTimeNanos int64
}

type FastOpenTransitionMeta struct {
	Status    string
	Object    string
	Tier      string
	VersionID string
}

// FastOpenPartMeta intentionally carries only fields needed by Phase 1
// full-object GET. Range, partNumber, and multipart body paths must stay out of
// FastOpen while this compact shape omits per-part index/checksum details.
type FastOpenPartMeta struct {
	Number     int
	Size       int64
	ActualSize int64
}

type FastOpenErasureMeta struct {
	DataBlocks   int
	ParityBlocks int
	BlockSize    int64
	Index        int
	Distribution []int
	Bitrot       FastOpenBitrotMeta
}

type FastOpenBitrotMeta struct {
	PartNumber int
	Algorithm  uint8
	Hash       []byte
}

func encodeFastOpenFrame(frame CoalescedMetadataFrame) ([]byte, error) {
	payload, err := marshalCoalescedMetadataFrame(frame)
	if err != nil {
		return nil, err
	}
	if len(payload) > fastOpenFrameMaxHeaderLen {
		return nil, errFastOpenFrameHeaderTooLarge
	}

	prelude := FastOpenFramePrelude{
		Magic:       fastOpenFrameMagic,
		Version:     fastOpenFrameVersion,
		HeaderLen:   uint32(len(payload)),
		HeaderCRC32: crc32.ChecksumIEEE(payload),
		BodyMode:    frame.BodyMode,
	}

	out := make([]byte, fastOpenFramePreludeLen+len(payload))
	encodeFastOpenFramePrelude(out[:fastOpenFramePreludeLen], prelude)
	copy(out[fastOpenFramePreludeLen:], payload)
	return out, nil
}

func readFastOpenFrame(r io.Reader) (FastOpenFramePrelude, CoalescedMetadataFrame, error) {
	var prelude FastOpenFramePrelude
	var frame CoalescedMetadataFrame
	preludeBuf := make([]byte, fastOpenFramePreludeLen)
	if _, err := io.ReadFull(r, preludeBuf); err != nil {
		return prelude, frame, err
	}

	prelude, err := decodeFastOpenFramePrelude(preludeBuf)
	if err != nil {
		return prelude, frame, err
	}
	if prelude.HeaderLen > fastOpenFrameMaxHeaderLen {
		return prelude, frame, errFastOpenFrameHeaderTooLarge
	}

	payload := make([]byte, prelude.HeaderLen)
	if _, err = io.ReadFull(r, payload); err != nil {
		return prelude, frame, err
	}
	if got := crc32.ChecksumIEEE(payload); got != prelude.HeaderCRC32 {
		return prelude, frame, errFastOpenFrameBadCRC
	}

	frame, err = unmarshalCoalescedMetadataFrame(payload)
	if err != nil {
		return prelude, frame, err
	}
	if frame.BodyMode != prelude.BodyMode {
		return prelude, frame, errFastOpenFrameBadPayload
	}
	return prelude, frame, nil
}

func encodeFastOpenFramePrelude(dst []byte, p FastOpenFramePrelude) {
	copy(dst[:4], p.Magic[:])
	binary.BigEndian.PutUint16(dst[4:6], p.Version)
	binary.BigEndian.PutUint32(dst[6:10], p.HeaderLen)
	binary.BigEndian.PutUint32(dst[10:14], p.HeaderCRC32)
	dst[14] = byte(p.BodyMode)
}

func decodeFastOpenFramePrelude(src []byte) (FastOpenFramePrelude, error) {
	var p FastOpenFramePrelude
	if len(src) < fastOpenFramePreludeLen {
		return p, io.ErrUnexpectedEOF
	}
	copy(p.Magic[:], src[:4])
	if !bytes.Equal(p.Magic[:], fastOpenFrameMagic[:]) {
		return p, errFastOpenFrameBadMagic
	}
	p.Version = binary.BigEndian.Uint16(src[4:6])
	if p.Version != fastOpenFrameVersion {
		return p, errFastOpenFrameBadVersion
	}
	p.HeaderLen = binary.BigEndian.Uint32(src[6:10])
	p.HeaderCRC32 = binary.BigEndian.Uint32(src[10:14])
	p.BodyMode = FastOpenBodyMode(src[14])
	if !p.BodyMode.valid() {
		return p, errFastOpenFrameBadBodyMode
	}
	return p, nil
}

func (m FastOpenBodyMode) valid() bool {
	switch m {
	case FastOpenBodyShard, FastOpenBodyInline, FastOpenBodyMetadataOnly, FastOpenBodyTransitioned:
		return true
	default:
		return false
	}
}

func (s FastOpenFrameStatus) valid() bool {
	switch s {
	case FastOpenStatusOK, FastOpenStatusDeleteMarker, FastOpenStatusNotFound, FastOpenStatusVersionNotFound, FastOpenStatusUnsupported:
		return true
	default:
		return false
	}
}

func fileInfoToFastOpenGETMeta(fi FileInfo) (FastOpenGETMeta, error) {
	meta := FastOpenGETMeta{
		VersionID:       fi.VersionID,
		IsLatest:        fi.IsLatest,
		Legacy:          fi.XLV1,
		ModTimeUnixNano: fastOpenTimeUnixNano(fi.ModTime),
		Size:            fi.Size,
		Metadata:        cloneStringMap(fi.Metadata),
		Transition: FastOpenTransitionMeta{
			Status:    fi.TransitionStatus,
			Object:    fi.TransitionedObjName,
			Tier:      fi.TransitionTier,
			VersionID: fi.TransitionVersionID,
		},
		Erasure: FastOpenErasureMeta{
			DataBlocks:   fi.Erasure.DataBlocks,
			ParityBlocks: fi.Erasure.ParityBlocks,
			BlockSize:    fi.Erasure.BlockSize,
			Index:        fi.Erasure.Index,
			Distribution: append([]int(nil), fi.Erasure.Distribution...),
		},
		Checksum:              append([]byte(nil), fi.Checksum...),
		NumVersions:           fi.NumVersions,
		SuccessorModTimeNanos: fastOpenTimeUnixNano(fi.SuccessorModTime),
	}
	if len(fi.Parts) > 0 {
		meta.Part = FastOpenPartMeta{
			Number:     fi.Parts[0].Number,
			Size:       fi.Parts[0].Size,
			ActualSize: fi.Parts[0].ActualSize,
		}
	}

	if meta.Part.Number != 0 {
		ckSum := fi.Erasure.GetChecksumInfo(meta.Part.Number)
		algo, err := fastOpenBitrotCode(ckSum.Algorithm)
		if err != nil {
			return meta, err
		}
		// Modern xl.meta v2 leaves Erasure.Checksums empty; GetChecksumInfo
		// returns the canonical streaming default with a zero PartNumber and nil
		// hash. That intentionally reconstructs to the same verifier setup.
		meta.Erasure.Bitrot = FastOpenBitrotMeta{
			PartNumber: ckSum.PartNumber,
			Algorithm:  algo,
			Hash:       append([]byte(nil), ckSum.Hash...),
		}
	}
	return meta, nil
}

// fastOpenGETMetaToFileInfo converts OK and delete-marker frames only. Disk-local
// not-found, version-not-found, and unsupported statuses must be routed through
// FastOpen quorum/error handling before calling this helper.
func fastOpenGETMetaToFileInfo(volume, object string, status FastOpenFrameStatus, meta FastOpenGETMeta) (FileInfo, error) {
	if !status.valid() {
		return FileInfo{}, errFastOpenFrameBadStatus
	}
	if status != FastOpenStatusOK && status != FastOpenStatusDeleteMarker {
		return FileInfo{}, errFastOpenFrameBadStatus
	}

	fi := FileInfo{
		Volume:              volume,
		Name:                object,
		VersionID:           meta.VersionID,
		IsLatest:            meta.IsLatest,
		Deleted:             status == FastOpenStatusDeleteMarker,
		TransitionStatus:    meta.Transition.Status,
		TransitionedObjName: meta.Transition.Object,
		TransitionTier:      meta.Transition.Tier,
		TransitionVersionID: meta.Transition.VersionID,
		XLV1:                meta.Legacy,
		ModTime:             fastOpenUnixNanoTime(meta.ModTimeUnixNano),
		Size:                meta.Size,
		Metadata:            cloneStringMap(meta.Metadata),
		ReplicationState:    getInternalReplicationState(meta.Metadata),
		NumVersions:         meta.NumVersions,
		SuccessorModTime:    fastOpenUnixNanoTime(meta.SuccessorModTimeNanos),
		Checksum:            append([]byte(nil), meta.Checksum...),
	}
	if meta.Part.Number != 0 {
		fi.Parts = []ObjectPartInfo{{
			Number:     meta.Part.Number,
			Size:       meta.Part.Size,
			ActualSize: meta.Part.ActualSize,
		}}
	}

	if meta.Erasure.hasData() {
		fi.Erasure = ErasureInfo{
			Algorithm:    erasureAlgorithm,
			DataBlocks:   meta.Erasure.DataBlocks,
			ParityBlocks: meta.Erasure.ParityBlocks,
			BlockSize:    meta.Erasure.BlockSize,
			Index:        meta.Erasure.Index,
			Distribution: append([]int(nil), meta.Erasure.Distribution...),
		}
	}
	if meta.Erasure.Bitrot.Algorithm != 0 {
		algo, err := fastOpenBitrotAlgorithm(meta.Erasure.Bitrot.Algorithm)
		if err != nil {
			return FileInfo{}, err
		}
		fi.Erasure.Checksums = []ChecksumInfo{{
			PartNumber: meta.Erasure.Bitrot.PartNumber,
			Algorithm:  algo,
			Hash:       append([]byte(nil), meta.Erasure.Bitrot.Hash...),
		}}
	}
	if !fi.Deleted && meta.Part.Number != 0 && meta.Erasure.Bitrot.Algorithm == 0 {
		return FileInfo{}, errFastOpenFrameBadBitrot
	}
	return fi, nil
}

func (m FastOpenErasureMeta) hasData() bool {
	return m.DataBlocks != 0 ||
		m.ParityBlocks != 0 ||
		m.BlockSize != 0 ||
		m.Index != 0 ||
		len(m.Distribution) != 0
}

func fastOpenTimeUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fastOpenUnixNanoTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func fastOpenBitrotCode(algo BitrotAlgorithm) (uint8, error) {
	switch algo {
	case SHA256:
		return 1, nil
	case HighwayHash256:
		return 2, nil
	case HighwayHash256S:
		return 3, nil
	case BLAKE2b512:
		return 4, nil
	default:
		return 0, errFastOpenFrameBadBitrot
	}
}

func fastOpenBitrotAlgorithm(code uint8) (BitrotAlgorithm, error) {
	switch code {
	case 1:
		return SHA256, nil
	case 2:
		return HighwayHash256, nil
	case 3:
		return HighwayHash256S, nil
	case 4:
		return BLAKE2b512, nil
	default:
		return 0, errFastOpenFrameBadBitrot
	}
}

func marshalCoalescedMetadataFrame(f CoalescedMetadataFrame) ([]byte, error) {
	if !f.Status.valid() {
		return nil, errFastOpenFrameBadStatus
	}
	if !f.BodyMode.valid() {
		return nil, errFastOpenFrameBadBodyMode
	}
	var b []byte
	b = msgp.AppendMapHeader(b, 4)
	b = msgp.AppendString(b, "s")
	b = msgp.AppendUint8(b, uint8(f.Status))
	b = msgp.AppendString(b, "m")
	b = appendFastOpenGETMeta(b, f.Meta)
	b = msgp.AppendString(b, "bm")
	b = msgp.AppendUint8(b, uint8(f.BodyMode))
	b = msgp.AppendString(b, "bl")
	b = msgp.AppendInt64(b, f.BodyLen)
	return b, nil
}

func unmarshalCoalescedMetadataFrame(b []byte) (CoalescedMetadataFrame, error) {
	var f CoalescedMetadataFrame
	n, b, err := msgp.ReadMapHeaderBytes(b)
	if err != nil {
		return f, err
	}
	for n > 0 {
		n--
		var field []byte
		field, b, err = msgp.ReadMapKeyZC(b)
		if err != nil {
			return f, err
		}
		switch string(field) {
		case "s":
			var v uint8
			v, b, err = msgp.ReadUint8Bytes(b)
			f.Status = FastOpenFrameStatus(v)
		case "m":
			f.Meta, b, err = readFastOpenGETMeta(b)
		case "bm":
			var v uint8
			v, b, err = msgp.ReadUint8Bytes(b)
			f.BodyMode = FastOpenBodyMode(v)
		case "bl":
			f.BodyLen, b, err = msgp.ReadInt64Bytes(b)
		default:
			b, err = msgp.Skip(b)
		}
		if err != nil {
			return f, err
		}
	}
	if len(b) != 0 {
		return f, errFastOpenFrameBadPayload
	}
	if !f.Status.valid() {
		return f, errFastOpenFrameBadStatus
	}
	if !f.BodyMode.valid() {
		return f, errFastOpenFrameBadBodyMode
	}
	return f, nil
}

func appendFastOpenGETMeta(b []byte, m FastOpenGETMeta) []byte {
	b = msgp.AppendMapHeader(b, 12)
	b = msgp.AppendString(b, "vid")
	b = msgp.AppendString(b, m.VersionID)
	b = msgp.AppendString(b, "il")
	b = msgp.AppendBool(b, m.IsLatest)
	b = msgp.AppendString(b, "v1")
	b = msgp.AppendBool(b, m.Legacy)
	b = msgp.AppendString(b, "mt")
	b = msgp.AppendInt64(b, m.ModTimeUnixNano)
	b = msgp.AppendString(b, "sz")
	b = msgp.AppendInt64(b, m.Size)
	b = msgp.AppendString(b, "meta")
	b = appendStringMap(b, m.Metadata)
	b = msgp.AppendString(b, "tr")
	b = appendFastOpenTransitionMeta(b, m.Transition)
	b = msgp.AppendString(b, "part")
	b = appendFastOpenPartMeta(b, m.Part)
	b = msgp.AppendString(b, "ei")
	b = appendFastOpenErasureMeta(b, m.Erasure)
	b = msgp.AppendString(b, "cs")
	b = appendBytesOrNil(b, m.Checksum)
	b = msgp.AppendString(b, "nv")
	b = msgp.AppendInt(b, m.NumVersions)
	b = msgp.AppendString(b, "smt")
	b = msgp.AppendInt64(b, m.SuccessorModTimeNanos)
	return b
}

func readFastOpenGETMeta(b []byte) (FastOpenGETMeta, []byte, error) {
	var m FastOpenGETMeta
	n, b, err := msgp.ReadMapHeaderBytes(b)
	if err != nil {
		return m, b, err
	}
	for n > 0 {
		n--
		var field []byte
		field, b, err = msgp.ReadMapKeyZC(b)
		if err != nil {
			return m, b, err
		}
		switch string(field) {
		case "vid":
			m.VersionID, b, err = msgp.ReadStringBytes(b)
		case "il":
			m.IsLatest, b, err = msgp.ReadBoolBytes(b)
		case "v1":
			m.Legacy, b, err = msgp.ReadBoolBytes(b)
		case "mt":
			m.ModTimeUnixNano, b, err = msgp.ReadInt64Bytes(b)
		case "sz":
			m.Size, b, err = msgp.ReadInt64Bytes(b)
		case "meta":
			m.Metadata, b, err = readStringMap(b)
		case "tr":
			m.Transition, b, err = readFastOpenTransitionMeta(b)
		case "part":
			m.Part, b, err = readFastOpenPartMeta(b)
		case "ei":
			m.Erasure, b, err = readFastOpenErasureMeta(b)
		case "cs":
			m.Checksum, b, err = readBytesOrNil(b)
		case "nv":
			m.NumVersions, b, err = msgp.ReadIntBytes(b)
		case "smt":
			m.SuccessorModTimeNanos, b, err = msgp.ReadInt64Bytes(b)
		default:
			b, err = msgp.Skip(b)
		}
		if err != nil {
			return m, b, err
		}
	}
	return m, b, nil
}

func appendFastOpenTransitionMeta(b []byte, m FastOpenTransitionMeta) []byte {
	b = msgp.AppendMapHeader(b, 4)
	b = msgp.AppendString(b, "s")
	b = msgp.AppendString(b, m.Status)
	b = msgp.AppendString(b, "o")
	b = msgp.AppendString(b, m.Object)
	b = msgp.AppendString(b, "t")
	b = msgp.AppendString(b, m.Tier)
	b = msgp.AppendString(b, "v")
	b = msgp.AppendString(b, m.VersionID)
	return b
}

func readFastOpenTransitionMeta(b []byte) (FastOpenTransitionMeta, []byte, error) {
	var m FastOpenTransitionMeta
	n, b, err := msgp.ReadMapHeaderBytes(b)
	if err != nil {
		return m, b, err
	}
	for n > 0 {
		n--
		var field []byte
		field, b, err = msgp.ReadMapKeyZC(b)
		if err != nil {
			return m, b, err
		}
		switch string(field) {
		case "s":
			m.Status, b, err = msgp.ReadStringBytes(b)
		case "o":
			m.Object, b, err = msgp.ReadStringBytes(b)
		case "t":
			m.Tier, b, err = msgp.ReadStringBytes(b)
		case "v":
			m.VersionID, b, err = msgp.ReadStringBytes(b)
		default:
			b, err = msgp.Skip(b)
		}
		if err != nil {
			return m, b, err
		}
	}
	return m, b, nil
}

func appendFastOpenPartMeta(b []byte, m FastOpenPartMeta) []byte {
	b = msgp.AppendMapHeader(b, 3)
	b = msgp.AppendString(b, "n")
	b = msgp.AppendInt(b, m.Number)
	b = msgp.AppendString(b, "s")
	b = msgp.AppendInt64(b, m.Size)
	b = msgp.AppendString(b, "as")
	b = msgp.AppendInt64(b, m.ActualSize)
	return b
}

func readFastOpenPartMeta(b []byte) (FastOpenPartMeta, []byte, error) {
	var m FastOpenPartMeta
	n, b, err := msgp.ReadMapHeaderBytes(b)
	if err != nil {
		return m, b, err
	}
	for n > 0 {
		n--
		var field []byte
		field, b, err = msgp.ReadMapKeyZC(b)
		if err != nil {
			return m, b, err
		}
		switch string(field) {
		case "n":
			m.Number, b, err = msgp.ReadIntBytes(b)
		case "s":
			m.Size, b, err = msgp.ReadInt64Bytes(b)
		case "as":
			m.ActualSize, b, err = msgp.ReadInt64Bytes(b)
		default:
			b, err = msgp.Skip(b)
		}
		if err != nil {
			return m, b, err
		}
	}
	return m, b, nil
}

func appendFastOpenErasureMeta(b []byte, m FastOpenErasureMeta) []byte {
	b = msgp.AppendMapHeader(b, 6)
	b = msgp.AppendString(b, "d")
	b = msgp.AppendInt(b, m.DataBlocks)
	b = msgp.AppendString(b, "p")
	b = msgp.AppendInt(b, m.ParityBlocks)
	b = msgp.AppendString(b, "bs")
	b = msgp.AppendInt64(b, m.BlockSize)
	b = msgp.AppendString(b, "i")
	b = msgp.AppendInt(b, m.Index)
	b = msgp.AppendString(b, "dist")
	b = appendIntSlice(b, m.Distribution)
	b = msgp.AppendString(b, "br")
	b = appendFastOpenBitrotMeta(b, m.Bitrot)
	return b
}

func readFastOpenErasureMeta(b []byte) (FastOpenErasureMeta, []byte, error) {
	var m FastOpenErasureMeta
	n, b, err := msgp.ReadMapHeaderBytes(b)
	if err != nil {
		return m, b, err
	}
	for n > 0 {
		n--
		var field []byte
		field, b, err = msgp.ReadMapKeyZC(b)
		if err != nil {
			return m, b, err
		}
		switch string(field) {
		case "d":
			m.DataBlocks, b, err = msgp.ReadIntBytes(b)
		case "p":
			m.ParityBlocks, b, err = msgp.ReadIntBytes(b)
		case "bs":
			m.BlockSize, b, err = msgp.ReadInt64Bytes(b)
		case "i":
			m.Index, b, err = msgp.ReadIntBytes(b)
		case "dist":
			m.Distribution, b, err = readIntSlice(b)
		case "br":
			m.Bitrot, b, err = readFastOpenBitrotMeta(b)
		default:
			b, err = msgp.Skip(b)
		}
		if err != nil {
			return m, b, err
		}
	}
	return m, b, nil
}

func appendFastOpenBitrotMeta(b []byte, m FastOpenBitrotMeta) []byte {
	b = msgp.AppendMapHeader(b, 3)
	b = msgp.AppendString(b, "pn")
	b = msgp.AppendInt(b, m.PartNumber)
	b = msgp.AppendString(b, "a")
	b = msgp.AppendUint8(b, m.Algorithm)
	b = msgp.AppendString(b, "h")
	b = appendBytesOrNil(b, m.Hash)
	return b
}

func readFastOpenBitrotMeta(b []byte) (FastOpenBitrotMeta, []byte, error) {
	var m FastOpenBitrotMeta
	n, b, err := msgp.ReadMapHeaderBytes(b)
	if err != nil {
		return m, b, err
	}
	for n > 0 {
		n--
		var field []byte
		field, b, err = msgp.ReadMapKeyZC(b)
		if err != nil {
			return m, b, err
		}
		switch string(field) {
		case "pn":
			m.PartNumber, b, err = msgp.ReadIntBytes(b)
		case "a":
			m.Algorithm, b, err = msgp.ReadUint8Bytes(b)
		case "h":
			m.Hash, b, err = readBytesOrNil(b)
		default:
			b, err = msgp.Skip(b)
		}
		if err != nil {
			return m, b, err
		}
	}
	return m, b, nil
}

func appendStringMap(b []byte, m map[string]string) []byte {
	if m == nil {
		return msgp.AppendNil(b)
	}
	b = msgp.AppendMapHeader(b, uint32(len(m)))
	for k, v := range m {
		b = msgp.AppendString(b, k)
		b = msgp.AppendString(b, v)
	}
	return b
}

func readStringMap(b []byte) (map[string]string, []byte, error) {
	if msgp.IsNil(b) {
		var err error
		b, err = msgp.ReadNilBytes(b)
		return nil, b, err
	}
	n, b, err := msgp.ReadMapHeaderBytes(b)
	if err != nil {
		return nil, b, err
	}
	m := make(map[string]string, n)
	for n > 0 {
		n--
		var k, v string
		k, b, err = msgp.ReadStringBytes(b)
		if err != nil {
			return nil, b, err
		}
		v, b, err = msgp.ReadStringBytes(b)
		if err != nil {
			return nil, b, err
		}
		m[k] = v
	}
	return m, b, nil
}

func appendIntSlice(b []byte, ints []int) []byte {
	if ints == nil {
		return msgp.AppendNil(b)
	}
	b = msgp.AppendArrayHeader(b, uint32(len(ints)))
	for _, v := range ints {
		b = msgp.AppendInt(b, v)
	}
	return b
}

func readIntSlice(b []byte) ([]int, []byte, error) {
	if msgp.IsNil(b) {
		var err error
		b, err = msgp.ReadNilBytes(b)
		return nil, b, err
	}
	n, b, err := msgp.ReadArrayHeaderBytes(b)
	if err != nil {
		return nil, b, err
	}
	ints := make([]int, n)
	for i := range ints {
		ints[i], b, err = msgp.ReadIntBytes(b)
		if err != nil {
			return nil, b, err
		}
	}
	return ints, b, nil
}

func appendBytesOrNil(b, v []byte) []byte {
	if v == nil {
		return msgp.AppendNil(b)
	}
	return msgp.AppendBytes(b, v)
}

func readBytesOrNil(b []byte) ([]byte, []byte, error) {
	if msgp.IsNil(b) {
		var err error
		b, err = msgp.ReadNilBytes(b)
		return nil, b, err
	}
	v, b, err := msgp.ReadBytesZC(b)
	if err != nil {
		return nil, b, err
	}
	return append([]byte(nil), v...), b, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
