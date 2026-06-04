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
	"net/http"
	"os"
	"sync/atomic"

	"github.com/buckit-io/buckit/internal/crypto"
)

var (
	globalFastGetEnabled = os.Getenv("BUCKIT_FAST_GET") == "1"
	fastGetHits          atomic.Uint64
	fastGetFallbacks     atomic.Uint64
)

func fastGetRequestEligible(h http.Header, rs *HTTPRangeSpec, opts ObjectOptions) bool {
	if !globalFastGetEnabled {
		return false
	}
	if opts.VersionID != "" || opts.Versioned || opts.VersionSuspended {
		return false
	}
	if opts.ReplicationRequest || opts.ProxyRequest || opts.ProxyHeaderSet {
		return false
	}
	if crypto.SSEC.IsRequested(h) {
		return false
	}
	if rs != nil && (rs.IsSuffixLength || rs.Start != 0) {
		return false
	}
	return true
}

func fastGetHeaderValid(h singleTripHeader) bool {
	return h.validForFastGet()
}
