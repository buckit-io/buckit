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
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/buckit-io/buckit/internal/config/storageclass"
)

func unsetFastGetEnvForTest(t *testing.T) {
	t.Helper()

	keys := []string{
		envBuckitFastGet,
		envBuckitFastGetSpread,
		envBuckitFastGetNoFallback,
		envBuckitFastOpenProfile,
	}
	old := make(map[string]string, len(keys))
	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			old[key] = value
			present[key] = true
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, key := range keys {
			if present[key] {
				if err := os.Setenv(key, old[key]); err != nil {
					t.Error(err)
				}
			} else if err := os.Unsetenv(key); err != nil {
				t.Error(err)
			}
		}
	})
}

func TestReadFastGetRuntimeConfigDefaultsToSpread(t *testing.T) {
	unsetFastGetEnvForTest(t)
	t.Setenv(envBuckitFastGet, "1")

	cfg := readFastGetRuntimeConfig()
	if !cfg.enabled || !cfg.spreadSelection {
		t.Fatalf("default fast-get config = %+v, want enabled spread", cfg)
	}
	if cfg.noFallback {
		t.Fatalf("default fast-get config = %+v, want no-fallback disabled", cfg)
	}
	if cfg.fastOpenProfile {
		t.Fatalf("default fast-get config = %+v, want FastOpen profile disabled", cfg)
	}
}

func TestReadFastGetRuntimeConfigHonorsExplicitOverrides(t *testing.T) {
	unsetFastGetEnvForTest(t)
	t.Setenv(envBuckitFastGet, "1")
	t.Setenv(envBuckitFastGetSpread, "0")
	t.Setenv(envBuckitFastGetNoFallback, "1")
	t.Setenv(envBuckitFastOpenProfile, "1")

	cfg := readFastGetRuntimeConfig()
	if !cfg.enabled {
		t.Fatalf("enabled = false, want true")
	}
	if cfg.spreadSelection {
		t.Fatalf("explicit disabled fast-get config = %+v, want spread disabled", cfg)
	}
	if !cfg.noFallback {
		t.Fatalf("noFallback = false, want true")
	}
	if !cfg.fastOpenProfile {
		t.Fatalf("fastOpenProfile = false, want true")
	}
}

type fastOpenGetObjectNInfo interface {
	GetObjectNInfo(ctx context.Context, bucket, object string, rs *HTTPRangeSpec, h http.Header, opts ObjectOptions) (*GetObjectReader, error)
}

func readFastOpenHelperObject(t *testing.T, obj fastOpenGetObjectNInfo, bucket, object string) ([]byte, ObjectInfo) {
	return readFastOpenHelperObjectRange(t, obj, bucket, object, nil)
}

func readFastOpenHelperObjectRange(t *testing.T, obj fastOpenGetObjectNInfo, bucket, object string, rs *HTTPRangeSpec) ([]byte, ObjectInfo) {
	t.Helper()

	out, info, err := readFastOpenHelperObjectRangeAllowError(t, obj, bucket, object, rs)
	if err != nil {
		t.Fatal(err)
	}
	return out, info
}

func readFastOpenHelperObjectRangeAllowError(t *testing.T, obj fastOpenGetObjectNInfo, bucket, object string, rs *HTTPRangeSpec) ([]byte, ObjectInfo, error) {
	t.Helper()

	gr, err := obj.GetObjectNInfo(t.Context(), bucket, object, rs, http.Header{}, ObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	defer gr.Close()

	var out bytes.Buffer
	if _, err = io.Copy(&out, gr); err != nil {
		return out.Bytes(), gr.ObjInfo, err
	}
	return out.Bytes(), gr.ObjInfo, nil
}

func makeFastOpenTestData(size int, seed byte) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*31 + int(seed)) % 251)
	}
	return data
}

func withFastOpenEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldEnabled := globalFastGetEnabled
	globalCompressConfigMu.Lock()
	oldCompressConfig := globalCompressConfig
	globalCompressConfig.Enabled = false
	globalCompressConfigMu.Unlock()
	oldAutoEncryption := globalAutoEncryption
	oldKMS := GlobalKMS
	oldStorageClass := globalStorageClass
	globalAutoEncryption = false
	GlobalKMS = nil
	defaultStorageClass, err := storageclass.LookupConfig(storageclass.DefaultKVS, 16)
	if err != nil {
		t.Fatal(err)
	}
	globalStorageClass.Update(defaultStorageClass)
	globalFastGetEnabled = enabled
	fastGetHits.Store(0)
	fastGetFallbacks.Store(0)
	resetFastOpenMetrics()
	t.Cleanup(func() {
		globalFastGetEnabled = oldEnabled
		globalAutoEncryption = oldAutoEncryption
		GlobalKMS = oldKMS
		globalStorageClass.Update(oldStorageClass)
		globalCompressConfigMu.Lock()
		globalCompressConfig = oldCompressConfig
		globalCompressConfigMu.Unlock()
		fastGetHits.Store(0)
		fastGetFallbacks.Store(0)
		resetFastOpenMetrics()
	})
}

func resetFastOpenMetrics() {
	globalFastOpenMetrics.attempted.Store(0)
	globalFastOpenMetrics.hits.Store(0)
	globalFastOpenMetrics.unsupported.Store(0)
	globalFastOpenMetrics.replacementPath.Store(0)
	globalFastOpenMetrics.streamsOpened.Store(0)
	globalFastOpenMetrics.replacementOpen.Store(0)
	globalFastOpenMetrics.streamCancels.Store(0)
	globalFastOpenMetrics.connGot.Store(0)
	globalFastOpenMetrics.connReused.Store(0)
	globalFastOpenMetrics.connFresh.Store(0)
	globalFastOpenMetrics.connWasIdle.Store(0)
	for i := range globalFastOpenMetrics.failures {
		globalFastOpenMetrics.failures[i].Store(0)
	}
	for i := range globalFastOpenMetrics.finalErrors {
		globalFastOpenMetrics.finalErrors[i].Store(0)
	}
}

func assertFastOpenCounterDelta(t *testing.T, hitsBefore, fallbacksBefore, wantHits, wantFallbacks uint64, label string) {
	t.Helper()

	gotHits := fastGetHits.Load() - hitsBefore
	gotFallbacks := fastGetFallbacks.Load() - fallbacksBefore
	if gotHits != wantHits || gotFallbacks != wantFallbacks {
		t.Fatalf("%s counters delta = hits:%d fallbacks:%d, want hits:%d fallbacks:%d", label, gotHits, gotFallbacks, wantHits, wantFallbacks)
	}
}

func assertFastOpenGETObjectInfoEqual(t *testing.T, got, want ObjectInfo) {
	t.Helper()

	if got.Size != want.Size {
		t.Fatalf("size = %d, want %d", got.Size, want.Size)
	}
	if got.ETag != want.ETag {
		t.Fatalf("etag = %q, want %q", got.ETag, want.ETag)
	}
	if got.ContentType != want.ContentType {
		t.Fatalf("content-type = %q, want %q", got.ContentType, want.ContentType)
	}
	if got.CacheControl != want.CacheControl {
		t.Fatalf("cache-control = %q, want %q", got.CacheControl, want.CacheControl)
	}
	if !got.ModTime.Equal(want.ModTime) {
		t.Fatalf("modtime = %s, want %s", got.ModTime, want.ModTime)
	}
}
