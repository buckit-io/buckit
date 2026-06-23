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

import "testing"

func TestRequestMemoryGateAdmission(t *testing.T) {
	g := &requestMemoryGate{
		budget:       10,
		metadataCost: 1,
	}

	if !g.TryAcquire(7) {
		t.Fatal("expected first request to be admitted")
	}
	if g.TryAcquire(4) {
		t.Fatal("expected request over budget to be rejected")
	}
	g.Release(7)
	if got := g.used.Load(); got != 0 {
		t.Fatalf("expected used memory to return to 0, got %d", got)
	}
}

func TestRequestMemoryGateAlwaysAdmitsFirstRequest(t *testing.T) {
	g := &requestMemoryGate{
		budget:       5,
		metadataCost: 1,
	}

	if !g.TryAcquire(10) {
		t.Fatal("expected first request to be admitted even when cost exceeds budget")
	}
	if g.TryAcquire(1) {
		t.Fatal("expected second request to be rejected while over budget")
	}
	g.Release(10)
}

func TestRequestMemoryGateCostClassification(t *testing.T) {
	g := &requestMemoryGate{
		readCost:     2,
		writeCost:    20,
		copyCost:     22,
		metadataCost: 1,
	}

	tests := []struct {
		api  string
		cost uint64
	}{
		{api: "GetObject", cost: 2},
		{api: "PutObject", cost: 20},
		{api: "CopyObject", cost: 22},
		{api: "CompleteMultipartUpload", cost: 1},
		{api: "ListObjectsV2", cost: 1},
		{api: "UnknownFutureHandler", cost: 20},
	}

	for _, tt := range tests {
		t.Run(tt.api, func(t *testing.T) {
			cost := g.requestCost(tt.api)
			if cost != tt.cost {
				t.Fatalf("expected cost %d, got %d", tt.cost, cost)
			}
		})
	}
}
