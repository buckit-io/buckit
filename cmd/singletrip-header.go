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
	"fmt"
	"io"
	"strings"

	"github.com/buckit-io/buckit/internal/crypto"
	xhttp "github.com/buckit-io/buckit/internal/http"
	"github.com/cespare/xxhash/v2"
)

const (
	singleTripHeaderVersion = 1
	singleTripHeaderLen     = 1024

	singleTripContentTypeMax     = 128
	singleTripContentEncodingMax = 64
	singleTripCacheControlMax    = 128
	singleTripExpiresMax         = 40
	singleTripStorageClassMax    = 32
	singleTripETagMax            = 128
	singleTripVersionIDMax       = 128
	singleTripErasureDistMax     = 64
)

var (
	singleTripHeaderMagic = [8]byte{'B', 'K', 'T', 'S', 'G', 'E', 'T', '1'}

	errSingleTripHeaderBadMagic   = errors.New("single-trip header: bad magic")
	errSingleTripHeaderBadVersion = errors.New("single-trip header: bad version")
	errSingleTripHeaderBadCRC     = errors.New("single-trip header: bad crc")
	errSingleTripHeaderTooLarge   = errors.New("single-trip header: encoded payload too large")
	errSingleTripHeaderBadPayload = errors.New("single-trip header: bad payload")
)

type singleTripHeaderFlags uint32

const (
	singleTripFlagDeleteMarker singleTripHeaderFlags = 1 << iota
	singleTripFlagInline
	singleTripFlagTransitioned
	singleTripFlagEncrypted
	singleTripFlagHasUserMeta
	singleTripFlagHasObjectTags
	singleTripFlagMetadataOverCap
	singleTripFlagCompressed
	singleTripFlagObjectLocked
)

type singleTripHeader struct {
	DirectSig        uint32
	VersionID        string
	ModTimeNanos     int64
	Size             int64
	PartSize         int64
	ActualPartSize   int64
	ETag             string
	ErasureM         uint16
	ErasureN         uint16
	ErasureIndex     uint16
	ErasureBlockSize int64
	ErasureDist      []uint8
	BitrotAlgo       BitrotAlgorithm
	PartCount        uint16
	Flags            singleTripHeaderFlags
	ContentType      string
	ContentEncoding  string
	CacheControl     string
	Expires          string
	StorageClass     string
}

func (h singleTripHeader) metadataWithinCaps() bool {
	return len(h.ContentType) <= singleTripContentTypeMax &&
		len(h.ContentEncoding) <= singleTripContentEncodingMax &&
		len(h.CacheControl) <= singleTripCacheControlMax &&
		len(h.Expires) <= singleTripExpiresMax &&
		len(h.StorageClass) <= singleTripStorageClassMax &&
		len(h.ETag) <= singleTripETagMax &&
		len(h.VersionID) <= singleTripVersionIDMax &&
		len(h.ErasureDist) <= singleTripErasureDistMax
}

func (h singleTripHeader) validForFastGet() bool {
	if h.PartCount != 1 {
		return false
	}
	if !h.metadataWithinCaps() {
		return false
	}
	if h.Flags&(singleTripFlagDeleteMarker|singleTripFlagInline|singleTripFlagTransitioned|singleTripFlagEncrypted|singleTripFlagHasUserMeta|singleTripFlagHasObjectTags|singleTripFlagMetadataOverCap|singleTripFlagCompressed|singleTripFlagObjectLocked) != 0 {
		return false
	}
	if h.ErasureM == 0 || h.ErasureN == 0 || h.ErasureIndex == 0 || h.ErasureIndex > h.ErasureM+h.ErasureN {
		return false
	}
	return h.BitrotAlgo.Available()
}

func (h singleTripHeader) commonEqual(o singleTripHeader) bool {
	return h.DirectSig == o.DirectSig &&
		h.VersionID == o.VersionID &&
		h.ModTimeNanos == o.ModTimeNanos &&
		h.Size == o.Size &&
		h.PartSize == o.PartSize &&
		h.ActualPartSize == o.ActualPartSize &&
		h.ETag == o.ETag &&
		h.ErasureM == o.ErasureM &&
		h.ErasureN == o.ErasureN &&
		h.ErasureBlockSize == o.ErasureBlockSize &&
		bytes.Equal(h.ErasureDist, o.ErasureDist) &&
		h.BitrotAlgo == o.BitrotAlgo &&
		h.PartCount == o.PartCount &&
		h.Flags == o.Flags &&
		h.ContentType == o.ContentType &&
		h.ContentEncoding == o.ContentEncoding &&
		h.CacheControl == o.CacheControl &&
		h.Expires == o.Expires &&
		h.StorageClass == o.StorageClass
}

func encodeSingleTripHeader(h singleTripHeader) ([]byte, error) {
	if !h.metadataWithinCaps() {
		h.Flags |= singleTripFlagMetadataOverCap
	}
	h.DirectSig = singleTripDirectSig(h)

	payload, err := marshalSingleTripHeaderPayload(h)
	if err != nil {
		return nil, err
	}
	if len(payload) > singleTripHeaderLen-18 {
		return nil, errSingleTripHeaderTooLarge
	}

	out := make([]byte, singleTripHeaderLen)
	copy(out[:8], singleTripHeaderMagic[:])
	binary.LittleEndian.PutUint16(out[8:10], singleTripHeaderVersion)
	binary.LittleEndian.PutUint16(out[10:12], singleTripHeaderLen)
	binary.LittleEndian.PutUint16(out[16:18], uint16(len(payload)))
	copy(out[18:], payload)
	binary.LittleEndian.PutUint32(out[12:16], xxhash32(out[16:]))
	return out, nil
}

func decodeSingleTripHeader(buf []byte) (singleTripHeader, error) {
	var h singleTripHeader
	if len(buf) < singleTripHeaderLen {
		return h, io.ErrUnexpectedEOF
	}
	if !bytes.Equal(buf[:8], singleTripHeaderMagic[:]) {
		return h, errSingleTripHeaderBadMagic
	}
	if binary.LittleEndian.Uint16(buf[8:10]) != singleTripHeaderVersion {
		return h, errSingleTripHeaderBadVersion
	}
	if binary.LittleEndian.Uint16(buf[10:12]) != singleTripHeaderLen {
		return h, errSingleTripHeaderBadPayload
	}
	wantCRC := binary.LittleEndian.Uint32(buf[12:16])
	if gotCRC := xxhash32(buf[16:singleTripHeaderLen]); gotCRC != wantCRC {
		return h, errSingleTripHeaderBadCRC
	}
	payloadLen := int(binary.LittleEndian.Uint16(buf[16:18]))
	if payloadLen < 0 || payloadLen > singleTripHeaderLen-18 {
		return h, errSingleTripHeaderBadPayload
	}
	h, err := unmarshalSingleTripHeaderPayload(buf[18 : 18+payloadLen])
	if err != nil {
		return h, err
	}
	if h.DirectSig != singleTripDirectSig(h) {
		return h, errSingleTripHeaderBadPayload
	}
	return h, nil
}

func marshalSingleTripHeaderPayload(h singleTripHeader) ([]byte, error) {
	var b bytes.Buffer
	writeUint32(&b, h.DirectSig)
	writeString(&b, h.VersionID)
	writeInt64(&b, h.ModTimeNanos)
	writeInt64(&b, h.Size)
	writeInt64(&b, h.PartSize)
	writeInt64(&b, h.ActualPartSize)
	writeString(&b, h.ETag)
	writeUint16(&b, h.ErasureM)
	writeUint16(&b, h.ErasureN)
	writeUint16(&b, h.ErasureIndex)
	writeInt64(&b, h.ErasureBlockSize)
	if len(h.ErasureDist) > singleTripErasureDistMax {
		return nil, fmt.Errorf("%w: erasure distribution", errSingleTripHeaderTooLarge)
	}
	writeSingleTripBytes(&b, h.ErasureDist)
	writeUint16(&b, uint16(h.BitrotAlgo))
	writeUint16(&b, h.PartCount)
	writeUint32(&b, uint32(h.Flags))
	writeString(&b, h.ContentType)
	writeString(&b, h.ContentEncoding)
	writeString(&b, h.CacheControl)
	writeString(&b, h.Expires)
	writeString(&b, h.StorageClass)
	return b.Bytes(), nil
}

func unmarshalSingleTripHeaderPayload(payload []byte) (singleTripHeader, error) {
	r := bytes.NewReader(payload)
	h := singleTripHeader{
		DirectSig:        readUint32(r),
		VersionID:        readString(r),
		ModTimeNanos:     readInt64(r),
		Size:             readInt64(r),
		PartSize:         readInt64(r),
		ActualPartSize:   readInt64(r),
		ETag:             readString(r),
		ErasureM:         readUint16(r),
		ErasureN:         readUint16(r),
		ErasureIndex:     readUint16(r),
		ErasureBlockSize: readInt64(r),
		ErasureDist:      readSingleTripBytes(r),
		BitrotAlgo:       BitrotAlgorithm(readUint16(r)),
		PartCount:        readUint16(r),
		Flags:            singleTripHeaderFlags(readUint32(r)),
		ContentType:      readString(r),
		ContentEncoding:  readString(r),
		CacheControl:     readString(r),
		Expires:          readString(r),
		StorageClass:     readString(r),
	}
	if r.Len() != 0 || h.Flags&singleTripFlagMetadataOverCap != 0 || !h.metadataWithinCaps() {
		return h, errSingleTripHeaderBadPayload
	}
	return h, nil
}

func singleTripDirectSig(h singleTripHeader) uint32 {
	var b bytes.Buffer
	writeString(&b, h.VersionID)
	writeInt64(&b, h.ModTimeNanos)
	writeUint16(&b, h.ErasureM)
	writeUint16(&b, h.ErasureN)
	writeInt64(&b, h.ErasureBlockSize)
	writeSingleTripBytes(&b, h.ErasureDist)
	writeUint16(&b, uint16(h.BitrotAlgo))
	writeUint16(&b, h.PartCount)
	writeInt64(&b, h.PartSize)
	writeInt64(&b, h.ActualPartSize)
	writeInt64(&b, h.Size)
	writeString(&b, h.ETag)
	writeUint32(&b, uint32(h.Flags))
	writeString(&b, h.ContentType)
	writeString(&b, h.ContentEncoding)
	writeString(&b, h.CacheControl)
	writeString(&b, h.Expires)
	writeString(&b, h.StorageClass)
	return xxhash32(b.Bytes())
}

func newSingleTripHeaderFromFileInfo(fi FileInfo, erasureIndex int) (singleTripHeader, bool) {
	h := singleTripHeader{
		VersionID:        fi.VersionID,
		ModTimeNanos:     fi.ModTime.UnixNano(),
		Size:             fi.Size,
		ETag:             extractETag(fi.Metadata),
		ErasureM:         uint16(fi.Erasure.DataBlocks),
		ErasureN:         uint16(fi.Erasure.ParityBlocks),
		ErasureIndex:     uint16(erasureIndex),
		ErasureBlockSize: fi.Erasure.BlockSize,
		BitrotAlgo:       fi.Erasure.GetChecksumInfo(1).Algorithm,
		ContentType:      fi.Metadata["content-type"],
		ContentEncoding:  fi.Metadata["content-encoding"],
		CacheControl:     fi.Metadata["cache-control"],
		Expires:          fi.Metadata["expires"],
		StorageClass:     fi.Metadata[xhttp.AmzStorageClass],
	}
	if len(fi.Parts) == 1 {
		h.PartCount = 1
		h.PartSize = fi.Parts[0].Size
		h.ActualPartSize = fi.Parts[0].ActualSize
	}
	h.ErasureDist = make([]uint8, 0, len(fi.Erasure.Distribution))
	for _, idx := range fi.Erasure.Distribution {
		if idx < 0 || idx > 255 {
			h.Flags |= singleTripFlagMetadataOverCap
			continue
		}
		h.ErasureDist = append(h.ErasureDist, uint8(idx))
	}
	if fi.Deleted {
		h.Flags |= singleTripFlagDeleteMarker
	}
	if fi.InlineData() {
		h.Flags |= singleTripFlagInline
	}
	if fi.TransitionStatus != "" || fi.TransitionedObjName != "" || fi.TransitionTier != "" || fi.TransitionVersionID != "" {
		h.Flags |= singleTripFlagTransitioned
	}
	if _, encrypted := crypto.IsEncrypted(fi.Metadata); encrypted {
		h.Flags |= singleTripFlagEncrypted
	}
	if fi.Metadata[xhttp.AmzObjectTagging] != "" {
		h.Flags |= singleTripFlagHasObjectTags
	}
	if singleTripHasUserMeta(fi.Metadata) {
		h.Flags |= singleTripFlagHasUserMeta
	}
	if singleTripHasCompressionMeta(fi.Metadata) {
		h.Flags |= singleTripFlagCompressed
	}
	if singleTripHasObjectLockMeta(fi.Metadata) {
		h.Flags |= singleTripFlagObjectLocked
	}
	if !h.metadataWithinCaps() {
		h.Flags |= singleTripFlagMetadataOverCap
	}
	h.DirectSig = singleTripDirectSig(h)
	return h, h.validForFastGet()
}

func singleTripHasUserMeta(metadata map[string]string) bool {
	for k := range metadata {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			return true
		}
	}
	return false
}

func singleTripHasCompressionMeta(metadata map[string]string) bool {
	for k := range metadata {
		if strings.EqualFold(k, ReservedMetadataPrefix+"compression") {
			return true
		}
	}
	return false
}

func singleTripHasObjectLockMeta(metadata map[string]string) bool {
	for k := range metadata {
		if equals(k, xhttp.AmzObjectLockMode, xhttp.AmzObjectLockRetainUntilDate, xhttp.AmzObjectLockLegalHold) {
			return true
		}
		if strings.EqualFold(k, ReservedMetadataPrefixLower+ObjectLockRetentionTimestamp) || strings.EqualFold(k, ReservedMetadataPrefixLower+ObjectLockLegalHoldTimestamp) {
			return true
		}
	}
	return false
}

func xxhash32(b []byte) uint32 {
	return uint32(xxhash.Sum64(b))
}

func writeUint16(b *bytes.Buffer, v uint16) {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], v)
	b.Write(tmp[:])
}

func writeUint32(b *bytes.Buffer, v uint32) {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	b.Write(tmp[:])
}

func writeInt64(b *bytes.Buffer, v int64) {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], uint64(v))
	b.Write(tmp[:])
}

func writeString(b *bytes.Buffer, v string) {
	writeSingleTripBytes(b, []byte(v))
}

func writeSingleTripBytes(b *bytes.Buffer, v []byte) {
	if len(v) > 65535 {
		panic("single-trip header: byte field too large")
	}
	writeUint16(b, uint16(len(v)))
	b.Write(v)
}

func readUint16(r *bytes.Reader) uint16 {
	var tmp [2]byte
	if _, err := io.ReadFull(r, tmp[:]); err != nil {
		return 0
	}
	return binary.LittleEndian.Uint16(tmp[:])
}

func readUint32(r *bytes.Reader) uint32 {
	var tmp [4]byte
	if _, err := io.ReadFull(r, tmp[:]); err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(tmp[:])
}

func readInt64(r *bytes.Reader) int64 {
	var tmp [8]byte
	if _, err := io.ReadFull(r, tmp[:]); err != nil {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(tmp[:]))
}

func readString(r *bytes.Reader) string {
	return string(readSingleTripBytes(r))
}

func readSingleTripBytes(r *bytes.Reader) []byte {
	n := int(readUint16(r))
	if n > r.Len() {
		r.Reset(nil)
		return nil
	}
	out := make([]byte, n)
	_, _ = io.ReadFull(r, out)
	return out
}
