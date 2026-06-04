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

	"github.com/minio/pkg/v3/sync/errgroup"
)

const singleTripCurrentDir = "current"

func (er erasureObjects) invalidateSingleTripShadow(ctx context.Context, bucket, object string, disks []StorageAPI, writeQuorum int) error {
	if !globalFastGetEnabled {
		return nil
	}
	deleteQuorum := singleTripShadowInvalidationQuorum(ctx, bucket, object, disks, writeQuorum)
	g := errgroup.WithNErrs(len(disks))
	for index := range disks {
		g.Go(func() error {
			if disks[index] == nil {
				return errDiskNotFound
			}
			return disks[index].Delete(ctx, bucket, pathJoin(object, singleTripCurrentDir), DeleteOptions{
				Recursive: true,
			})
		}, index)
	}
	return reduceSingleTripShadowDeleteErrs(ctx, g.Wait(), deleteQuorum)
}

func singleTripShadowInvalidationQuorum(ctx context.Context, bucket, object string, disks []StorageAPI, fallbackQuorum int) int {
	headers := make([]singleTripHeaderRead, len(disks))
	g := errgroup.WithNErrs(len(disks))
	for index := range disks {
		g.Go(func() error {
			headers[index] = readSingleTripHeader(ctx, disks[index], bucket, object, false)
			return headers[index].err
		}, index)
	}
	_ = g.Wait()

	bestCount, oldParity := 0, 0
	for i := range headers {
		if headers[i].err != nil {
			continue
		}
		count := 0
		for j := range headers {
			if headers[j].err == nil && headers[j].header.DirectSig == headers[i].header.DirectSig && headers[j].header.commonEqual(headers[i].header) {
				count++
			}
		}
		if count > bestCount {
			bestCount = count
			oldParity = int(headers[i].header.ErasureN)
		}
	}
	if oldParity > 0 && bestCount >= oldParity+1 {
		return min(len(disks), oldParity+1)
	}
	if fallbackQuorum > 0 {
		return min(len(disks), fallbackQuorum)
	}
	return len(disks)/2 + 1
}

func reduceSingleTripShadowDeleteErrs(ctx context.Context, errs []error, deleteQuorum int) error {
	if contextCanceled(ctx) {
		return context.Canceled
	}
	successes := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, errFileNotFound):
			successes++
		case errors.Is(err, errVolumeNotFound):
			successes++
		case IsErrIgnored(err, objectOpIgnoredErrs...):
			continue
		}
	}
	if successes >= deleteQuorum {
		return nil
	}
	return errErasureWriteQuorum
}

func (er erasureObjects) installSingleTripShadow(ctx context.Context, bucket, object string, disks []StorageAPI, metadata []FileInfo, writeQuorum int) error {
	if !globalFastGetEnabled {
		return nil
	}
	g := errgroup.WithNErrs(len(disks))
	for index := range disks {
		g.Go(func() error {
			if disks[index] == nil {
				return errDiskNotFound
			}
			fi := metadata[index]
			if fi.Erasure.Index == 0 {
				fi.Erasure.Index = index + 1
			}
			return writeSingleTripShadow(ctx, disks[index], bucket, object, fi)
		}, index)
	}
	return reduceWriteQuorumErrs(ctx, g.Wait(), objectOpIgnoredErrs, writeQuorum)
}

func writeSingleTripShadow(ctx context.Context, disk StorageAPI, bucket, object string, fi FileInfo) error {
	if len(fi.Parts) != 1 || fi.DataDir == "" || fi.InlineData() {
		return errFileNotFound
	}

	header, ok := newSingleTripHeaderFromFileInfo(fi, fi.Erasure.Index)
	if !ok {
		return errFileNotFound
	}
	encodedHeader, err := encodeSingleTripHeader(header)
	if err != nil {
		return err
	}

	// Part.Size is the encoded length erasure.Encode wrote. ActualSize is the
	// client-visible decrypted/decompressed length and must not drive shard I/O.
	shardSize := fi.Erasure.ShardFileSize(fi.Parts[0].Size)
	bitrotSize := bitrotShardFileSize(shardSize, fi.Erasure.ShardSize(), header.BitrotAlgo)
	srcPath := pathJoin(object, fi.DataDir, "part.1")
	src, err := disk.ReadFileStream(ctx, bucket, srcPath, 0, bitrotSize)
	if err != nil {
		return err
	}
	defer src.Close()

	tmpPath := pathJoin(object, singleTripCurrentDir+"."+mustGetUUID(), "part.1")
	dstPath := pathJoin(object, singleTripCurrentDir, "part.1")
	if err = disk.CreateFile(ctx, "", bucket, tmpPath, singleTripHeaderLen+bitrotSize, io.MultiReader(bytes.NewReader(encodedHeader), src)); err != nil {
		return err
	}
	return disk.RenameFile(ctx, bucket, tmpPath, bucket, dstPath)
}
