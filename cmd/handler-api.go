// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/shirou/gopsutil/v3/mem"

	"github.com/buckit-io/buckit/internal/config/api"
	"github.com/buckit-io/buckit/internal/logger"
	"github.com/buckit-io/buckit/internal/mcontext"
)

type apiConfig struct {
	mu sync.RWMutex

	requestsPool           chan struct{}
	requestMemory          *requestMemoryGate
	requestsCapacity       int
	clusterDeadline        time.Duration
	listQuorum             string
	corsAllowOrigins       []string
	replicationPriority    string
	replicationMaxWorkers  int
	replicationMaxLWorkers int
	transitionWorkers      int

	staleUploadsExpiry          time.Duration
	staleUploadsCleanupInterval time.Duration
	deleteCleanupInterval       time.Duration
	enableODirect               bool
	gzipObjects                 bool
	rootAccess                  bool
	syncEvents                  bool
	objectMaxVersions           int64
}

const (
	cgroupV1MemLimitFile = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
	cgroupV2MemLimitFile = "/sys/fs/cgroup/memory.max"

	requestMemoryBudgetPercent = 75
)

var errRequestMemoryUnderflow = errors.New("request memory gate accounting underflow")

type requestMemoryGate struct {
	used atomic.Uint64

	budget       uint64
	readCost     uint64
	writeCost    uint64
	copyCost     uint64
	metadataCost uint64
}

func newRequestMemoryGate(maxSetDrives int, legacy bool) *requestMemoryGate {
	// Keep request buffers below the Go memory limit target. MemLimit is already
	// based on available memory, so this second fraction reserves headroom for
	// metadata, HTTP/TLS buffers, goroutine stacks, and other runtime allocations.
	budget := (globalServerCtxt.MemLimit * requestMemoryBudgetPercent) / 100
	if budget == 0 {
		budget = blockSizeV2
	}

	// These costs intentionally mirror the current erasure read/write buffer
	// reservations. GET normally holds one 2MiB pooled buffer. Non-inline PUTs
	// reserve one 2MiB streaming bitrot writer
	// buffer per erasure-set drive, plus encode and readahead buffers. Small or
	// inline writes are charged conservatively at the non-inline cost because
	// this middleware cannot reliably know the final object layout. Keep this
	// table in sync with erasure-decode.go, erasure-object.go, and
	// bitrot-streaming.go.
	readCost := uint64(blockSizeV2 * 2)
	writeCost := uint64(maxSetDrives+3) * blockSizeV2 * 2
	metadataCost := uint64(blockSizeV2)
	if legacy {
		readCost += blockSizeV1 * 2
		writeCost += blockSizeV1 * 2
		metadataCost += blockSizeV1
	}

	return &requestMemoryGate{
		budget:       budget,
		readCost:     readCost,
		writeCost:    writeCost,
		copyCost:     readCost + writeCost,
		metadataCost: metadataCost,
	}
}

func (g *requestMemoryGate) TryAcquire(cost uint64) bool {
	if g == nil {
		return true
	}
	if cost == 0 {
		cost = g.metadataCost
	}

	for {
		used := g.used.Load()
		// Always allow the first request through, even if its estimated cost is
		// larger than the budget. This preserves forward progress on small-memory
		// hosts or unusually wide erasure sets.
		if used != 0 && (cost > g.budget || used > g.budget-cost) {
			return false
		}
		if g.used.CompareAndSwap(used, used+cost) {
			return true
		}
	}
}

func (g *requestMemoryGate) Release(cost uint64) {
	if g == nil {
		return
	}
	if cost == 0 {
		cost = g.metadataCost
	}

	for {
		used := g.used.Load()
		if cost > used {
			bugLogIf(context.Background(), errRequestMemoryUnderflow)
			return
		}
		if g.used.CompareAndSwap(used, used-cost) {
			return
		}
	}
}

func (g *requestMemoryGate) capacity(cost uint64) uint64 {
	if g == nil {
		return 0
	}
	if cost == 0 {
		cost = g.metadataCost
	}
	if cost == 0 {
		return 1
	}
	capacity := g.budget / cost
	if capacity == 0 {
		return 1
	}
	return capacity
}

func (g *requestMemoryGate) remainingCapacity(cost uint64) uint64 {
	if g == nil {
		return 0
	}
	if cost == 0 {
		cost = g.metadataCost
	}
	if cost == 0 {
		return 1
	}
	used := g.used.Load()
	if used >= g.budget {
		return 0
	}
	return (g.budget - used) / cost
}

func (g *requestMemoryGate) requestCost(api string) uint64 {
	if g == nil {
		return 0
	}

	switch api {
	case "GetObject", "GetObjectLambda", "SelectObjectContent":
		return g.readCost
	case "PutObject", "PutObjectPart", "PutObjectExtract", "PostPolicyBucket":
		return g.writeCost
	case "CopyObject", "CopyObjectPart":
		return g.copyCost
	case "CompleteMultipartUpload", "NewMultipartUpload", "AbortMultipartUpload",
		"DeleteObject", "DeleteMultipleObjects",
		"GetObjectACL", "PutObjectACL",
		"GetObjectTagging", "PutObjectTagging", "DeleteObjectTagging",
		"GetObjectRetention", "PutObjectRetention",
		"GetObjectLegalHold", "PutObjectLegalHold",
		"GetObjectAttributes", "HeadObject",
		"GetBucketLocation", "GetBucketPolicy", "GetBucketLifecycle",
		"GetBucketEncryption", "GetBucketObjectLockConfig",
		"GetBucketReplicationConfig", "GetBucketVersioning",
		"GetBucketNotification", "ResetBucketReplicationStatus",
		"GetBucketACL", "PutBucketACL", "GetBucketCors", "PutBucketCors",
		"DeleteBucketCors", "GetBucketWebsite", "GetBucketAccelerate",
		"GetBucketRequestPayment", "GetBucketLogging", "GetBucketTagging",
		"DeleteBucketWebsite", "DeleteBucketTagging", "GetBucketPolicyStatus",
		"PutBucketLifecycle", "PutBucketReplicationConfig",
		"PutBucketEncryption", "PutBucketPolicy", "PutBucketObjectLockConfig",
		"PutBucketTagging", "PutBucketVersioning", "PutBucketNotification",
		"ResetBucketReplicationStart", "PutBucket", "HeadBucket",
		"DeleteBucketPolicy", "DeleteBucketReplicationConfig",
		"DeleteBucketLifecycle", "DeleteBucketEncryption", "DeleteBucket",
		"GetBucketReplicationMetricsV2", "GetBucketReplicationMetrics",
		"ValidateBucketReplicationCreds",
		"ListObjectParts", "ListMultipartUploads", "ListObjectsV1",
		"ListObjectsV2", "ListObjectsV2M", "ListObjectVersions",
		"ListObjectVersionsM", "ListBuckets":
		return g.metadataCost
	default:
		// Unknown S3 APIs are charged conservatively so new write-heavy handlers
		// do not bypass memory admission until this table is updated.
		return g.writeCost
	}
}

func cgroupMemLimit() (limit uint64) {
	buf, err := os.ReadFile(cgroupV2MemLimitFile)
	if err != nil {
		buf, err = os.ReadFile(cgroupV1MemLimitFile)
	}
	if err != nil {
		return 0
	}
	limit, err = strconv.ParseUint(strings.TrimSpace(string(buf)), 10, 64)
	if err != nil {
		// The kernel can return valid but non integer values
		// but still, no need to interpret more
		return 0
	}
	if limit >= 100*humanize.TiByte {
		// No limit set, or unreasonably high. Ignore
		return 0
	}
	return limit
}

func availableMemory() (available uint64) {
	available = 2048 * blockSizeV2 * 2 // Default to 4 GiB when we can't find the limits.

	if runtime.GOOS == "linux" {
		// Honor cgroup limits if set.
		limit := cgroupMemLimit()
		if limit > 0 {
			// A valid value is found, return its 90%
			available = (limit * 9) / 10
			return available
		}
	} // for all other platforms limits are based on virtual memory.

	memStats, err := mem.VirtualMemory()
	if err != nil {
		return available
	}

	// A valid value is available return its 90%
	available = (memStats.Available * 9) / 10
	return available
}

func (t *apiConfig) init(cfg api.Config, setDriveCounts []int, legacy bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	clusterDeadline := cfg.ClusterDeadline
	if clusterDeadline == 0 {
		clusterDeadline = 10 * time.Second
	}
	t.clusterDeadline = clusterDeadline
	corsAllowOrigin := cfg.CorsAllowOrigin
	if len(corsAllowOrigin) == 0 {
		corsAllowOrigin = []string{"*"}
	}
	t.corsAllowOrigins = corsAllowOrigin

	if cfg.RequestsMax <= 0 {
		maxSetDrives := slices.Max(setDriveCounts)
		t.requestMemory = newRequestMemoryGate(maxSetDrives, legacy)
		t.requestsPool = nil
		t.requestsCapacity = int(t.requestMemory.capacity(t.requestMemory.readCost))

		if globalIsDistErasure {
			logger.Info("Configured API request memory budget per node: %d bytes", t.requestMemory.budget)
		}
	} else {
		apiRequestsMaxPerNode := cfg.RequestsMax
		if n := totalNodeCount(); n > 0 {
			apiRequestsMaxPerNode /= n
		}

		if globalIsDistErasure {
			logger.Info("Configured max API requests per node: %d", apiRequestsMaxPerNode)
		}

		if cap(t.requestsPool) != apiRequestsMaxPerNode {
			// Only replace if needed.
			// Existing requests will use the previous limit,
			// but new requests will use the new limit.
			// There will be a short overlap window,
			// but this shouldn't last long.
			t.requestsPool = make(chan struct{}, apiRequestsMaxPerNode)
		}
		t.requestMemory = nil
		t.requestsCapacity = apiRequestsMaxPerNode
	}
	listQuorum := cfg.ListQuorum
	if listQuorum == "" {
		listQuorum = "strict"
	}
	t.listQuorum = listQuorum
	if r := globalReplicationPool.GetNonBlocking(); r != nil &&
		(cfg.ReplicationPriority != t.replicationPriority || cfg.ReplicationMaxWorkers != t.replicationMaxWorkers || cfg.ReplicationMaxLWorkers != t.replicationMaxLWorkers) {
		r.ResizeWorkerPriority(cfg.ReplicationPriority, cfg.ReplicationMaxWorkers, cfg.ReplicationMaxLWorkers)
	}
	t.replicationPriority = cfg.ReplicationPriority
	t.replicationMaxWorkers = cfg.ReplicationMaxWorkers
	t.replicationMaxLWorkers = cfg.ReplicationMaxLWorkers

	// N B api.transition_workers will be deprecated
	if globalTransitionState != nil {
		globalTransitionState.UpdateWorkers(cfg.TransitionWorkers)
	}
	t.transitionWorkers = cfg.TransitionWorkers

	t.staleUploadsExpiry = cfg.StaleUploadsExpiry
	t.deleteCleanupInterval = cfg.DeleteCleanupInterval
	t.enableODirect = cfg.EnableODirect
	t.gzipObjects = cfg.GzipObjects
	t.rootAccess = cfg.RootAccess
	t.syncEvents = cfg.SyncEvents
	t.objectMaxVersions = cfg.ObjectMaxVersions

	if t.staleUploadsCleanupInterval != cfg.StaleUploadsCleanupInterval {
		t.staleUploadsCleanupInterval = cfg.StaleUploadsCleanupInterval

		// signal that cleanup interval has changed
		select {
		case staleUploadsCleanupIntervalChangedCh <- struct{}{}:
		default: // in case the channel is blocked...
		}
	}
}

func (t *apiConfig) odirectEnabled() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.enableODirect
}

func (t *apiConfig) shouldGzipObjects() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.gzipObjects
}

func (t *apiConfig) permitRootAccess() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.rootAccess
}

func (t *apiConfig) getListQuorum() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.listQuorum == "" {
		return "strict"
	}

	return t.listQuorum
}

func (t *apiConfig) getCorsAllowOrigins() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if len(t.corsAllowOrigins) == 0 {
		return []string{"*"}
	}

	corsAllowOrigins := make([]string, len(t.corsAllowOrigins))
	copy(corsAllowOrigins, t.corsAllowOrigins)
	return corsAllowOrigins
}

func (t *apiConfig) getStaleUploadsCleanupInterval() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.staleUploadsCleanupInterval == 0 {
		return 6 * time.Hour // default 6 hours
	}

	return t.staleUploadsCleanupInterval
}

func (t *apiConfig) getStaleUploadsExpiry() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.staleUploadsExpiry == 0 {
		return 24 * time.Hour // default 24 hours
	}

	return t.staleUploadsExpiry
}

func (t *apiConfig) getDeleteCleanupInterval() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.deleteCleanupInterval == 0 {
		return 5 * time.Minute // every 5 minutes
	}

	return t.deleteCleanupInterval
}

func (t *apiConfig) getClusterDeadline() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.clusterDeadline == 0 {
		return 10 * time.Second
	}

	return t.clusterDeadline
}

func (t *apiConfig) getRequestsPoolCapacity() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.requestsCapacity
}

func (t *apiConfig) getRequestsPool() chan struct{} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.requestsPool == nil {
		return nil
	}

	return t.requestsPool
}

func (t *apiConfig) getRequestMemoryGate() *requestMemoryGate {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.requestMemory
}

// maxClients throttles the S3 API calls
func maxClients(api string, f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		globalHTTPStats.incS3RequestsIncoming()

		if r.Header.Get(globalObjectPerfUserMetadata) == "" {
			if val := globalServiceFreeze.Load(); val != nil {
				if unlock, ok := val.(chan struct{}); ok && unlock != nil {
					// Wait until unfrozen.
					select {
					case <-unlock:
					case <-r.Context().Done():
						// if client canceled we don't need to wait here forever.
						return
					}
				}
			}
		}

		globalHTTPStats.addRequestsInQueue(1)
		gate := globalAPIConfig.getRequestMemoryGate()
		if gate != nil {
			if tc, ok := r.Context().Value(mcontext.ContextTraceKey).(*mcontext.TraceCtxt); ok {
				tc.FuncName = "s3.MaxClients"
			}

			cost := gate.requestCost(api)
			w.Header().Set("X-RateLimit-Limit", strconv.FormatUint(gate.capacity(cost), 10))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatUint(gate.remainingCapacity(cost), 10))

			ctx := r.Context()
			if !gate.TryAcquire(cost) {
				globalHTTPStats.addRequestsInQueue(-1)
				if contextCanceled(ctx) {
					w.WriteHeader(499)
					return
				}
				writeErrorResponse(ctx, w,
					errorCodes.ToAPIErr(ErrTooManyRequests),
					r.URL)
				return
			}
			defer gate.Release(cost)

			globalHTTPStats.addRequestsInQueue(-1)
			if contextCanceled(ctx) {
				w.WriteHeader(499)
				return
			}
			f.ServeHTTP(w, r)
			return
		}

		pool := globalAPIConfig.getRequestsPool()
		if pool == nil {
			globalHTTPStats.addRequestsInQueue(-1)
			f.ServeHTTP(w, r)
			return
		}

		if tc, ok := r.Context().Value(mcontext.ContextTraceKey).(*mcontext.TraceCtxt); ok {
			tc.FuncName = "s3.MaxClients"
		}

		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(cap(pool)))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(cap(pool)-len(pool)))

		ctx := r.Context()
		select {
		case pool <- struct{}{}:
			defer func() { <-pool }()
			globalHTTPStats.addRequestsInQueue(-1)
			if contextCanceled(ctx) {
				w.WriteHeader(499)
				return
			}
			f.ServeHTTP(w, r)
		case <-r.Context().Done():
			globalHTTPStats.addRequestsInQueue(-1)
			// When the client disconnects before getting the S3 handler
			// status code response, set the status code to 499 so this request
			// will be properly audited and traced.
			w.WriteHeader(499)
		default:
			globalHTTPStats.addRequestsInQueue(-1)
			if contextCanceled(ctx) {
				w.WriteHeader(499)
				return
			}
			// Send a http timeout message
			writeErrorResponse(ctx, w,
				errorCodes.ToAPIErr(ErrTooManyRequests),
				r.URL)
		}
	}
}

func (t *apiConfig) getReplicationOpts() replicationPoolOpts {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.replicationPriority == "" {
		return replicationPoolOpts{
			Priority:    "auto",
			MaxWorkers:  WorkerMaxLimit,
			MaxLWorkers: LargeWorkerCount,
		}
	}

	return replicationPoolOpts{
		Priority:    t.replicationPriority,
		MaxWorkers:  t.replicationMaxWorkers,
		MaxLWorkers: t.replicationMaxLWorkers,
	}
}

func (t *apiConfig) getTransitionWorkers() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.transitionWorkers <= 0 {
		return runtime.GOMAXPROCS(0) / 2
	}

	return t.transitionWorkers
}

func (t *apiConfig) isSyncEventsEnabled() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.syncEvents
}

func (t *apiConfig) getObjectMaxVersions() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.objectMaxVersions <= 0 {
		// defaults to 'IntMax' when unset.
		return math.MaxInt64
	}

	return t.objectMaxVersions
}
