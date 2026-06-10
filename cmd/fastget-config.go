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
	"os"
	"sync/atomic"
)

const (
	envBuckitFastGet           = "BUCKIT_FAST_GET"
	envBuckitFastGetSpread     = "BUCKIT_FASTGET_SPREAD"
	envBuckitFastGetNoFallback = "BUCKIT_FASTGET_NO_FALLBACK"
)

type fastGetRuntimeConfig struct {
	enabled         bool
	spreadSelection bool
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
		enabled:         enabled,
		spreadSelection: fastGetEnvBool(envBuckitFastGetSpread, enabled),
		noFallback:      fastGetEnvBool(envBuckitFastGetNoFallback, false),
	}
}

var globalFastGetRuntimeConfig = readFastGetRuntimeConfig()

var (
	globalFastGetEnabled         = globalFastGetRuntimeConfig.enabled
	globalFastGetSpreadSelection = globalFastGetRuntimeConfig.spreadSelection
	globalFastGetNoFallback      = globalFastGetRuntimeConfig.noFallback
	fastGetHits                  atomic.Uint64
	fastGetFallbacks             atomic.Uint64
)

var errFastGetNoFallback = errors.New("FastOpen unavailable and fallback disabled")
