// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package masterclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEndpoints(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"single", "http://m:8089", []string{"http://m:8089"}},
		{"trim trailing slash", "http://m:8089/", []string{"http://m:8089"}},
		{"comma list", "a:1,b:2", []string{"a:1", "b:2"}},
		{"whitespace and empties", "  a:1 ,, b:2 ", []string{"a:1", "b:2"}},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEndpoints(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want %d (%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("idx %d: %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestUpdateNodeStatusCarriesAllocatedAndDisk(t *testing.T) {
	var received UpdateNodeStatusRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		fmt.Fprintln(w, `{"ret":{"ret_code":200,"ret_msg":"ok"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)
	want := &UpdateNodeStatusRequest{
		HeartbeatTime: time.Unix(1700000000, 0).UTC(),
		MetricTime:    time.Unix(1700000001, 0).UTC(),
		Allocated: &AllocatedResources{
			MilliCPU: 4500, MemoryMB: 8192,
			MvmNum: 3, MvmRunningNum: 2, NicQueues: 6,
			DataDiskMB: 100, StorageDiskMB: 200,
		},
		DiskUsage: &DiskUsage{
			DataDiskUsagePer: 11.5, StorageDiskUsagePer: 22.5, SysDiskUsagePer: 33.5,
		},
	}
	if err := c.UpdateNodeStatus(context.Background(), "node-a", want); err != nil {
		t.Fatalf("UpdateNodeStatus: %v", err)
	}
	if received.Allocated == nil || received.Allocated.MilliCPU != 4500 ||
		received.Allocated.MemoryMB != 8192 || received.Allocated.MvmNum != 3 ||
		received.Allocated.DataDiskMB != 100 || received.Allocated.StorageDiskMB != 200 {
		t.Fatalf("allocated round-trip mismatch: %+v", received.Allocated)
	}
	if received.DiskUsage == nil || received.DiskUsage.SysDiskUsagePer != 33.5 {
		t.Fatalf("disk usage round-trip mismatch: %+v", received.DiskUsage)
	}
	if !received.MetricTime.Equal(want.MetricTime) {
		t.Fatalf("metric_time mismatch: %v vs %v", received.MetricTime, want.MetricTime)
	}
}

func TestClientFailsOverOnTransportError(t *testing.T) {
	var goodHits int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodHits, 1)
		fmt.Fprintln(w, `{"ret":{"ret_code":200,"ret_msg":"ok"}}`)
	}))
	defer good.Close()

	// A closed listener URL makes Do fail with a connection refused so the
	// failover path is exercised deterministically without flakiness.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bad.Close()

	endpoints := strings.Join([]string{bad.URL, good.URL}, ",")
	c := New(endpoints, time.Second)
	for i := 0; i < 4; i++ {
		if err := c.Readyz(context.Background()); err != nil {
			t.Fatalf("Readyz attempt %d failed: %v", i, err)
		}
	}
	if atomic.LoadInt32(&goodHits) == 0 {
		t.Fatalf("no requests reached the healthy peer")
	}
}
