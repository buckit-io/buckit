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
	"errors"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/buckit-io/buckit/internal/crypto"
)

const (
	envBuckitFastGet            = "BUCKIT_FAST_GET"
	envBuckitFastGetEager       = "BUCKIT_FASTGET_EAGER"
	envBuckitFastGetEagerSelect = "BUCKIT_FASTGET_EAGER_SELECTED"
	envBuckitFastGetSpread      = "BUCKIT_FASTGET_SPREAD"
	envBuckitFastGetHedge       = "BUCKIT_FASTGET_HEDGE"
	envBuckitFastGetNoFallback  = "BUCKIT_FASTGET_NO_FALLBACK"
)

type fastGetRuntimeConfig struct {
	enabled         bool
	eager           bool
	eagerSelected   bool
	spreadSelection bool
	hedgeShards     bool
	noFallback      bool
}

func fastGetEnvBool(name string, defaultValue bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return defaultValue
	}
	return v == "1"
}

func readFastGetRuntimeConfig() fastGetRuntimeConfig {
	enabled := fastGetEnvBool(envBuckitFastGet, false)
	return fastGetRuntimeConfig{
		enabled: enabled,
		// Default candidate when fast GET is enabled: EAGERSTABLE.
		eager:           fastGetEnvBool(envBuckitFastGetEager, enabled),
		spreadSelection: fastGetEnvBool(envBuckitFastGetSpread, enabled),
		eagerSelected:   fastGetEnvBool(envBuckitFastGetEagerSelect, false),
		hedgeShards:     fastGetEnvBool(envBuckitFastGetHedge, false),
		noFallback:      fastGetEnvBool(envBuckitFastGetNoFallback, false),
	}
}

var globalFastGetRuntimeConfig = readFastGetRuntimeConfig()

var (
	globalFastGetEnabled = globalFastGetRuntimeConfig.enabled
	// globalFastGetEager: read each data shard's body inside the per-disk open
	// goroutine (overlapping body read with the open, skipping parity bodies and
	// the header->body barrier) instead of lazily during decode. Defaults on when
	// BUCKIT_FAST_GET=1; set BUCKIT_FASTGET_EAGER=0 to disable.
	globalFastGetEager = globalFastGetRuntimeConfig.eager
	// globalFastGetEagerSelected is a diagnostic mode: read headers first,
	// select the decode quorum, then prefetch the first block for all selected
	// shards concurrently. This avoids the local-only eager barrier.
	globalFastGetEagerSelected = globalFastGetRuntimeConfig.eagerSelected
	// globalFastGetSpreadSelection selects exactly M shards by rotating across the
	// full erasure-set disk order instead of local-first. Defaults on when
	// BUCKIT_FAST_GET=1; set BUCKIT_FASTGET_SPREAD=0 to disable.
	globalFastGetSpreadSelection = globalFastGetRuntimeConfig.spreadSelection
	// globalFastGetHedgeShards opens one deterministic extra shadow header beyond
	// the stable primary M and allows it to replace one slow/bad primary shard.
	globalFastGetHedgeShards = globalFastGetRuntimeConfig.hedgeShards
	globalFastGetNoFallback  = globalFastGetRuntimeConfig.noFallback
	fastGetHits              atomic.Uint64
	fastGetFallbacks         atomic.Uint64
)

var errFastGetNoFallback = errors.New("single-trip fast get unavailable and fallback disabled")

func fastGetRequestEligible(bucket string, h http.Header, rs *HTTPRangeSpec, opts ObjectOptions) bool {
	if !globalFastGetEnabled {
		return false
	}
	if bucket == minioMetaBucket {
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
