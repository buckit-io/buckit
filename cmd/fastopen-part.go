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
	"fmt"
	"io"

	xioutil "github.com/buckit-io/buckit/internal/ioutil"
)

// FastOpenPart reads this disk's xl.meta, converts the selected object version
// into a compact FastOpen metadata frame, and returns a stream containing the
// frame followed by encoded body bytes when the selected version is locally
// streamable. Body bytes are the on-disk shard or inline bytes; callers remain
// responsible for bitrot verification, erasure decode, decryption, and response
// shaping.
func (s *xlStorage) FastOpenPart(ctx context.Context, volume, object string, req FastOpenPartRequest) (io.ReadCloser, error) {
	if req.Version != fastOpenFrameVersion {
		return nil, errFastOpenFrameBadVersion
	}
	if req.PartNumber != 1 || req.Offset != 0 || req.Length != -1 {
		return fastOpenFrameOnly(FastOpenStatusUnsupported)
	}

	raw, err := s.ReadXL(ctx, volume, object, true)
	if err != nil {
		switch err {
		case errFileNotFound:
			if req.VersionID != "" {
				return fastOpenFrameOnly(FastOpenStatusVersionNotFound)
			}
			return fastOpenFrameOnly(FastOpenStatusNotFound)
		default:
			return nil, err
		}
	}

	fi, err := getFileInfo(raw.Buf, volume, object, req.VersionID, fileInfoOpts{Data: true})
	if err != nil {
		metaDataPoolPut(raw.Buf)
		switch err {
		case errFileNotFound:
			return fastOpenFrameOnly(FastOpenStatusNotFound)
		case errFileVersionNotFound:
			return fastOpenFrameOnly(FastOpenStatusVersionNotFound)
		default:
			return nil, err
		}
	}

	inlineData := append([]byte(nil), fi.Data...)
	metaDataPoolPut(raw.Buf)
	fi.Data = nil

	meta, err := fileInfoToFastOpenGETMeta(fi)
	if err != nil {
		return nil, err
	}
	frame := CoalescedMetadataFrame{
		Status:   FastOpenStatusOK,
		Meta:     meta,
		BodyMode: FastOpenBodyMetadataOnly,
	}

	switch {
	case fi.Deleted:
		frame.Status = FastOpenStatusDeleteMarker
		return fastOpenFrameWithBody(frame, nil, nil)
	case fi.Size == 0:
		return fastOpenFrameWithBody(frame, nil, nil)
	case fi.IsRemote():
		frame.BodyMode = FastOpenBodyTransitioned
		return fastOpenFrameWithBody(frame, nil, nil)
	case len(fi.Parts) != 1 || fi.Parts[0].Number != req.PartNumber:
		return fastOpenFrameOnly(FastOpenStatusUnsupported)
	case fi.InlineData():
		if len(inlineData) == 0 {
			return nil, errFileCorrupt
		}
		frame.BodyMode = FastOpenBodyInline
		frame.BodyLen = int64(len(inlineData))
		return fastOpenFrameWithBody(frame, bytes.NewReader(inlineData), nil)
	default:
		partPath := pathJoin(object, fi.DataDir, fmt.Sprintf("part.%d", req.PartNumber))
		stat, err := s.StatInfoFile(ctx, volume, partPath, false)
		if err != nil {
			if len(inlineData) > 0 && IsErr(err, errPathNotFound, errFileNotFound) {
				frame.BodyMode = FastOpenBodyInline
				frame.BodyLen = int64(len(inlineData))
				return fastOpenFrameWithBody(frame, bytes.NewReader(inlineData), nil)
			}
			return nil, err
		}
		if len(stat) != 1 || stat[0].Dir {
			return nil, errFileNotFound
		}
		body, err := s.ReadFileStream(ctx, volume, partPath, 0, -1)
		if err != nil {
			return nil, err
		}
		frame.BodyMode = FastOpenBodyShard
		frame.BodyLen = stat[0].Size
		return fastOpenFrameWithBody(frame, body, body)
	}
}

func (p *xlStorageDiskIDCheck) FastOpenPart(ctx context.Context, volume, object string, req FastOpenPartRequest) (rc io.ReadCloser, err error) {
	ctx, done, err := p.TrackDiskHealth(ctx, storageMetricReadVersion, volume, object)
	if err != nil {
		return nil, err
	}
	defer done(0, &err)

	return xioutil.WithDeadline[io.ReadCloser](ctx, globalDriveConfig.GetMaxTimeout(), func(ctx context.Context) (io.ReadCloser, error) {
		return p.storage.FastOpenPart(ctx, volume, object, req)
	})
}

func fastOpenFrameOnly(status FastOpenFrameStatus) (io.ReadCloser, error) {
	return fastOpenFrameWithBody(CoalescedMetadataFrame{
		Status:   status,
		BodyMode: FastOpenBodyMetadataOnly,
	}, nil, nil)
}

func fastOpenFrameWithBody(frame CoalescedMetadataFrame, body io.Reader, closer io.Closer) (io.ReadCloser, error) {
	encoded, err := encodeFastOpenFrame(frame)
	if err != nil {
		if closer != nil {
			closer.Close()
		}
		return nil, err
	}
	if body == nil {
		return io.NopCloser(bytes.NewReader(encoded)), nil
	}
	return &fastOpenMultiReadCloser{
		r: io.MultiReader(bytes.NewReader(encoded), body),
		c: closer,
	}, nil
}

type fastOpenMultiReadCloser struct {
	r io.Reader
	c io.Closer
}

func (m *fastOpenMultiReadCloser) Read(p []byte) (int, error) { return m.r.Read(p) }

func (m *fastOpenMultiReadCloser) Close() error {
	if m.c != nil {
		return m.c.Close()
	}
	return nil
}
