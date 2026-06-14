// Copyright (c) 2015-2024 MinIO, Inc.
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
	"time"

	"github.com/buckit-io/minio-go/v7/pkg/set"
)

const (
	apiRejectedAuthTotal      MetricName = "rejected_auth_total"
	apiRejectedHeaderTotal    MetricName = "rejected_header_total"
	apiRejectedTimestampTotal MetricName = "rejected_timestamp_total"
	apiRejectedInvalidTotal   MetricName = "rejected_invalid_total"

	apiRequestsWaitingTotal  MetricName = "waiting_total"
	apiRequestsIncomingTotal MetricName = "incoming_total"

	apiRequestsInFlightTotal             MetricName = "inflight_total"
	apiRequestsTotal                     MetricName = "total"
	apiRequestsErrorsTotal               MetricName = "errors_total"
	apiRequests5xxErrorsTotal            MetricName = "5xx_errors_total"
	apiRequests4xxErrorsTotal            MetricName = "4xx_errors_total"
	apiRequestsCanceledTotal             MetricName = "canceled_total"
	apiRequestsFastGetHits               MetricName = "fast_get_hits_total"
	apiRequestsFastGetFallbacks          MetricName = "fast_get_fallbacks_total"
	apiRequestsFastOpenAttempted         MetricName = "fast_open_attempted_total"
	apiRequestsFastOpenHits              MetricName = "fast_open_hits_total"
	apiRequestsFastOpenUnsupported       MetricName = "fast_open_unsupported_total"
	apiRequestsFastOpenReplacementPath   MetricName = "fast_open_replacement_path_total"
	apiRequestsFastOpenStreamsOpened     MetricName = "fast_open_streams_opened_total"
	apiRequestsFastOpenReplacementOpen   MetricName = "fast_open_replacement_opens_total"
	apiRequestsFastOpenFailures          MetricName = "fast_open_selected_set_failures_total"
	apiRequestsFastOpenStreamCancels     MetricName = "fast_open_stream_cancellations_total"
	apiRequestsFastOpenFinalErrors       MetricName = "fast_open_final_errors_total"
	apiRequestsFastOpenConnGot           MetricName = "fast_open_httptrace_connections_total"
	apiRequestsFastOpenConnReused        MetricName = "fast_open_httptrace_reused_connections_total"
	apiRequestsFastOpenConnFresh         MetricName = "fast_open_httptrace_fresh_connections_total"
	apiRequestsFastOpenConnWasIdle       MetricName = "fast_open_httptrace_was_idle_connections_total"
	apiRequestsFastOpenTrySeconds        MetricName = "fast_open_try_seconds_total"
	apiRequestsFastOpenTryCount          MetricName = "fast_open_try_seconds_count"
	apiRequestsFastOpenOpenInfoSeconds   MetricName = "fast_open_open_info_seconds_total"
	apiRequestsFastOpenOpenInfoCount     MetricName = "fast_open_open_info_seconds_count"
	apiRequestsFastOpenBodyDecodeSeconds MetricName = "fast_open_body_decode_seconds_total"
	apiRequestsFastOpenBodyDecodeCount   MetricName = "fast_open_body_decode_seconds_count"

	apiRequestsTTFBSecondsDistribution MetricName = "ttfb_seconds_distribution"

	apiTrafficSentBytes MetricName = "traffic_sent_bytes"
	apiTrafficRecvBytes MetricName = "traffic_received_bytes"
)

var (
	apiRejectedAuthTotalMD = NewCounterMD(apiRejectedAuthTotal,
		"Total number of requests rejected for auth failure", "type")
	apiRejectedHeaderTotalMD = NewCounterMD(apiRejectedHeaderTotal,
		"Total number of requests rejected for invalid header", "type")
	apiRejectedTimestampTotalMD = NewCounterMD(apiRejectedTimestampTotal,
		"Total number of requests rejected for invalid timestamp", "type")
	apiRejectedInvalidTotalMD = NewCounterMD(apiRejectedInvalidTotal,
		"Total number of invalid requests", "type")

	apiRequestsWaitingTotalMD = NewGaugeMD(apiRequestsWaitingTotal,
		"Total number of requests in the waiting queue", "type")
	apiRequestsIncomingTotalMD = NewGaugeMD(apiRequestsIncomingTotal,
		"Total number of incoming requests", "type")

	apiRequestsInFlightTotalMD = NewGaugeMD(apiRequestsInFlightTotal,
		"Total number of requests currently in flight", "name", "type")
	apiRequestsTotalMD = NewCounterMD(apiRequestsTotal,
		"Total number of requests", "name", "type")
	apiRequestsErrorsTotalMD = NewCounterMD(apiRequestsErrorsTotal,
		"Total number of requests with (4xx and 5xx) errors", "name", "type")
	apiRequests5xxErrorsTotalMD = NewCounterMD(apiRequests5xxErrorsTotal,
		"Total number of requests with 5xx errors", "name", "type")
	apiRequests4xxErrorsTotalMD = NewCounterMD(apiRequests4xxErrorsTotal,
		"Total number of requests with 4xx errors", "name", "type")
	apiRequestsCanceledTotalMD = NewCounterMD(apiRequestsCanceledTotal,
		"Total number of requests canceled by the client", "name", "type")
	apiRequestsFastGetHitsMD = NewCounterMD(apiRequestsFastGetHits,
		"Total number of GET requests served by the FastOpen path", "type")
	apiRequestsFastGetFallbacksMD = NewCounterMD(apiRequestsFastGetFallbacks,
		"Total number of FastOpen-eligible GET requests that fell back to the normal path", "type")
	apiRequestsFastOpenAttemptedMD = NewCounterMD(apiRequestsFastOpenAttempted,
		"Total number of GET requests attempted on the FastOpen path", "type")
	apiRequestsFastOpenHitsMD = NewCounterMD(apiRequestsFastOpenHits,
		"Total number of GET requests served by the FastOpen path", "type")
	apiRequestsFastOpenUnsupportedMD = NewCounterMD(apiRequestsFastOpenUnsupported,
		"Total number of FastOpen-eligible requests that fell back before response commit", "type")
	apiRequestsFastOpenReplacementPathMD = NewCounterMD(apiRequestsFastOpenReplacementPath,
		"Total number of FastOpen GET requests that used the replacement path", "type")
	apiRequestsFastOpenStreamsOpenedMD = NewCounterMD(apiRequestsFastOpenStreamsOpened,
		"Total number of FastOpenPart streams opened by GET requests", "type")
	apiRequestsFastOpenReplacementOpenMD = NewCounterMD(apiRequestsFastOpenReplacementOpen,
		"Total number of non-zero-offset FastOpenPart replacement streams opened by GET requests", "type")
	apiRequestsFastOpenFailuresMD = NewCounterMD(apiRequestsFastOpenFailures,
		"Total number of FastOpen selected-set failures by reason", "reason", "type")
	apiRequestsFastOpenStreamCancelsMD = NewCounterMD(apiRequestsFastOpenStreamCancels,
		"Total number of FastOpenPart streams canceled by GET request cleanup", "type")
	apiRequestsFastOpenFinalErrorsMD = NewCounterMD(apiRequestsFastOpenFinalErrors,
		"Total number of FastOpen GET requests that returned an object-level error by category", "category", "type")
	apiRequestsFastOpenConnGotMD = NewCounterMD(apiRequestsFastOpenConnGot,
		"Total number of FastOpenPart HTTP connections observed by httptrace", "type")
	apiRequestsFastOpenConnReusedMD = NewCounterMD(apiRequestsFastOpenConnReused,
		"Total number of FastOpenPart HTTP connections reported as reused by httptrace", "type")
	apiRequestsFastOpenConnFreshMD = NewCounterMD(apiRequestsFastOpenConnFresh,
		"Total number of FastOpenPart HTTP connections reported as fresh by httptrace", "type")
	apiRequestsFastOpenConnWasIdleMD = NewCounterMD(apiRequestsFastOpenConnWasIdle,
		"Total number of FastOpenPart HTTP connections reported as previously idle by httptrace", "type")
	apiRequestsFastOpenTrySecondsMD = NewCounterMD(apiRequestsFastOpenTrySeconds,
		"Total wall time spent in tryFastOpenGET", "type")
	apiRequestsFastOpenTryCountMD = NewCounterMD(apiRequestsFastOpenTryCount,
		"Total number of timed tryFastOpenGET calls", "type")
	apiRequestsFastOpenOpenInfoSecondsMD = NewCounterMD(apiRequestsFastOpenOpenInfoSeconds,
		"Total wall time spent opening FastOpen GET info", "type")
	apiRequestsFastOpenOpenInfoCountMD = NewCounterMD(apiRequestsFastOpenOpenInfoCount,
		"Total number of timed FastOpen GET info opens", "type")
	apiRequestsFastOpenBodyDecodeSecondsMD = NewCounterMD(apiRequestsFastOpenBodyDecodeSeconds,
		"Total wall time spent decoding FastOpen GET bodies", "type")
	apiRequestsFastOpenBodyDecodeCountMD = NewCounterMD(apiRequestsFastOpenBodyDecodeCount,
		"Total number of timed FastOpen GET body decodes", "type")

	apiRequestsTTFBSecondsDistributionMD = NewCounterMD(apiRequestsTTFBSecondsDistribution,
		"Distribution of time to first byte across API calls", "name", "type", "le")

	apiTrafficSentBytesMD = NewCounterMD(apiTrafficSentBytes,
		"Total number of bytes sent", "type")
	apiTrafficRecvBytesMD = NewCounterMD(apiTrafficRecvBytes,
		"Total number of bytes received", "type")
)

// loadAPIRequestsHTTPMetrics - reads S3 HTTP metrics.
//
// This is a `MetricsLoaderFn`.
//
// This includes node level S3 HTTP metrics.
//
// This function currently ignores `opts`.
func loadAPIRequestsHTTPMetrics(ctx context.Context, m MetricValues, _ *metricsCache) error {
	// Collect node level S3 HTTP metrics.
	httpStats := globalHTTPStats.toServerHTTPStats(false)

	// Currently we only collect S3 API related stats, so we set the "type"
	// label to "s3".

	m.Set(apiRejectedAuthTotal, float64(httpStats.TotalS3RejectedAuth), "type", "s3")
	m.Set(apiRejectedTimestampTotal, float64(httpStats.TotalS3RejectedTime), "type", "s3")
	m.Set(apiRejectedHeaderTotal, float64(httpStats.TotalS3RejectedHeader), "type", "s3")
	m.Set(apiRejectedInvalidTotal, float64(httpStats.TotalS3RejectedInvalid), "type", "s3")
	m.Set(apiRequestsWaitingTotal, float64(httpStats.S3RequestsInQueue), "type", "s3")
	m.Set(apiRequestsIncomingTotal, float64(httpStats.S3RequestsIncoming), "type", "s3")

	for name, value := range httpStats.CurrentS3Requests.APIStats {
		m.Set(apiRequestsInFlightTotal, float64(value), "name", name, "type", "s3")
	}
	for name, value := range httpStats.TotalS3Requests.APIStats {
		m.Set(apiRequestsTotal, float64(value), "name", name, "type", "s3")
	}
	for name, value := range httpStats.TotalS3Errors.APIStats {
		m.Set(apiRequestsErrorsTotal, float64(value), "name", name, "type", "s3")
	}
	for name, value := range httpStats.TotalS35xxErrors.APIStats {
		m.Set(apiRequests5xxErrorsTotal, float64(value), "name", name, "type", "s3")
	}
	for name, value := range httpStats.TotalS34xxErrors.APIStats {
		m.Set(apiRequests4xxErrorsTotal, float64(value), "name", name, "type", "s3")
	}
	for name, value := range httpStats.TotalS3Canceled.APIStats {
		m.Set(apiRequestsCanceledTotal, float64(value), "name", name, "type", "s3")
	}
	m.Set(apiRequestsFastGetHits, float64(fastGetHits.Load()), "type", "s3")
	m.Set(apiRequestsFastGetFallbacks, float64(fastGetFallbacks.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenAttempted, float64(globalFastOpenMetrics.attempted.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenHits, float64(globalFastOpenMetrics.hits.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenUnsupported, float64(globalFastOpenMetrics.unsupported.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenReplacementPath, float64(globalFastOpenMetrics.replacementPath.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenStreamsOpened, float64(globalFastOpenMetrics.streamsOpened.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenReplacementOpen, float64(globalFastOpenMetrics.replacementOpen.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenStreamCancels, float64(globalFastOpenMetrics.streamCancels.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenConnGot, float64(globalFastOpenMetrics.connGot.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenConnReused, float64(globalFastOpenMetrics.connReused.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenConnFresh, float64(globalFastOpenMetrics.connFresh.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenConnWasIdle, float64(globalFastOpenMetrics.connWasIdle.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenTrySeconds, float64(globalFastOpenMetrics.tryNS.Load())/float64(time.Second), "type", "s3")
	m.Set(apiRequestsFastOpenTryCount, float64(globalFastOpenMetrics.tryCount.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenOpenInfoSeconds, float64(globalFastOpenMetrics.openInfoNS.Load())/float64(time.Second), "type", "s3")
	m.Set(apiRequestsFastOpenOpenInfoCount, float64(globalFastOpenMetrics.openInfoCount.Load()), "type", "s3")
	m.Set(apiRequestsFastOpenBodyDecodeSeconds, float64(globalFastOpenMetrics.bodyDecodeNS.Load())/float64(time.Second), "type", "s3")
	m.Set(apiRequestsFastOpenBodyDecodeCount, float64(globalFastOpenMetrics.bodyDecodeCount.Load()), "type", "s3")
	for reason := fastOpenFailureReason(0); reason < fastOpenFailureCount; reason++ {
		m.Set(apiRequestsFastOpenFailures, float64(globalFastOpenMetrics.failures[reason].Load()), "reason", reason.String(), "type", "s3")
	}
	for category := fastOpenFinalErrorCategory(0); category < fastOpenFinalErrorCount; category++ {
		m.Set(apiRequestsFastOpenFinalErrors, float64(globalFastOpenMetrics.finalErrors[category].Load()), "category", category.String(), "type", "s3")
	}
	return nil
}

// loadAPIRequestsTTFBMetrics - loads S3 TTFB metrics.
//
// This is a `MetricsLoaderFn`.
func loadAPIRequestsTTFBMetrics(ctx context.Context, m MetricValues, _ *metricsCache) error {
	renameLabels := map[string]string{"api": "name"}
	labelsFilter := map[string]set.StringSet{}
	m.SetHistogram(apiRequestsTTFBSecondsDistribution, httpRequestsDuration, labelsFilter, renameLabels, nil,
		"type", "s3")
	return nil
}

// loadAPIRequestsNetworkMetrics - loads S3 network metrics.
//
// This is a `MetricsLoaderFn`.
func loadAPIRequestsNetworkMetrics(ctx context.Context, m MetricValues, _ *metricsCache) error {
	connStats := globalConnStats.toServerConnStats()
	m.Set(apiTrafficSentBytes, float64(connStats.s3OutputBytes), "type", "s3")
	m.Set(apiTrafficRecvBytes, float64(connStats.s3InputBytes), "type", "s3")
	return nil
}

// Metric Descriptions for bucket level S3 metrics.
var (
	bucketAPITrafficSentBytesMD = NewCounterMD(apiTrafficSentBytes,
		"Total number of bytes received for a bucket", "bucket", "type")
	bucketAPITrafficRecvBytesMD = NewCounterMD(apiTrafficRecvBytes,
		"Total number of bytes sent for a bucket", "bucket", "type")

	bucketAPIRequestsInFlightMD = NewGaugeMD(apiRequestsInFlightTotal,
		"Total number of requests currently in flight for a bucket", "bucket", "name", "type")
	bucketAPIRequestsTotalMD = NewCounterMD(apiRequestsTotal,
		"Total number of requests for a bucket", "bucket", "name", "type")
	bucketAPIRequestsCanceledMD = NewCounterMD(apiRequestsCanceledTotal,
		"Total number of requests canceled by the client for a bucket", "bucket", "name", "type")
	bucketAPIRequests4xxErrorsMD = NewCounterMD(apiRequests4xxErrorsTotal,
		"Total number of requests with 4xx errors for a bucket", "bucket", "name", "type")
	bucketAPIRequests5xxErrorsMD = NewCounterMD(apiRequests5xxErrorsTotal,
		"Total number of requests with 5xx errors for a bucket", "bucket", "name", "type")

	bucketAPIRequestsTTFBSecondsDistributionMD = NewCounterMD(apiRequestsTTFBSecondsDistribution,
		"Distribution of time to first byte across API calls for a bucket",
		"bucket", "name", "le", "type")
)

// loadBucketAPIHTTPMetrics - loads bucket level S3 HTTP metrics.
//
// This is a `MetricsLoaderFn`.
//
// This includes bucket level S3 HTTP metrics and S3 network in/out metrics.
func loadBucketAPIHTTPMetrics(ctx context.Context, m MetricValues, _ *metricsCache, buckets []string) error {
	if len(buckets) == 0 {
		return nil
	}
	for bucket, inOut := range globalBucketConnStats.getBucketS3InOutBytes(buckets) {
		recvBytes := inOut.In
		if recvBytes > 0 {
			m.Set(apiTrafficSentBytes, float64(recvBytes), "bucket", bucket, "type", "s3")
		}
		sentBytes := inOut.Out
		if sentBytes > 0 {
			m.Set(apiTrafficRecvBytes, float64(sentBytes), "bucket", bucket, "type", "s3")
		}

		httpStats := globalBucketHTTPStats.load(bucket)
		for k, v := range httpStats.currentS3Requests.Load(false) {
			m.Set(apiRequestsInFlightTotal, float64(v), "bucket", bucket, "name", k, "type", "s3")
		}

		for k, v := range httpStats.totalS3Requests.Load(false) {
			m.Set(apiRequestsTotal, float64(v), "bucket", bucket, "name", k, "type", "s3")
		}

		for k, v := range httpStats.totalS3Canceled.Load(false) {
			m.Set(apiRequestsCanceledTotal, float64(v), "bucket", bucket, "name", k, "type", "s3")
		}

		for k, v := range httpStats.totalS34xxErrors.Load(false) {
			m.Set(apiRequests4xxErrorsTotal, float64(v), "bucket", bucket, "name", k, "type", "s3")
		}

		for k, v := range httpStats.totalS35xxErrors.Load(false) {
			m.Set(apiRequests5xxErrorsTotal, float64(v), "bucket", bucket, "name", k, "type", "s3")
		}
	}

	return nil
}

// loadBucketAPITTFBMetrics - loads bucket S3 TTFB metrics.
//
// This is a `MetricsLoaderFn`.
func loadBucketAPITTFBMetrics(ctx context.Context, m MetricValues, _ *metricsCache, buckets []string) error {
	renameLabels := map[string]string{"api": "name"}
	labelsFilter := map[string]set.StringSet{}
	m.SetHistogram(apiRequestsTTFBSecondsDistribution, bucketHTTPRequestsDuration, labelsFilter, renameLabels,
		buckets, "type", "s3")
	return nil
}
