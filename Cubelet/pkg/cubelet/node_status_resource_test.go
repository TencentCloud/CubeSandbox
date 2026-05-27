// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"

	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/resourcesource"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
)

type stubCollector struct {
	alloc *resourcesource.AllocatedResources
	disk  *resourcesource.DiskUsage
}

func (s *stubCollector) CollectAllocated() *resourcesource.AllocatedResources { return s.alloc }
func (s *stubCollector) CollectDiskUsage() *resourcesource.DiskUsage          { return s.disk }

func TestAttachResourceReportPopulatesFieldsAndMetricTime(t *testing.T) {
	t.Cleanup(func() { resourcesource.Set(nil) })

	now := time.Unix(1700000000, 0).UTC()
	resourcesource.Set(&stubCollector{
		alloc: &resourcesource.AllocatedResources{
			MilliCPU: 1000, MemoryMB: 2048, MvmNum: 3, MvmRunningNum: 2,
			NicQueues: 4, DataDiskMB: 10, StorageDiskMB: 20,
		},
		disk: &resourcesource.DiskUsage{DataDiskUsagePer: 25.0, StorageDiskUsagePer: 50.0, SysDiskUsagePer: 75.0},
	})
	req := &masterclient.UpdateNodeStatusRequest{}
	attachResourceReport(req, now)

	if req.Allocated == nil || req.Allocated.MilliCPU != 1000 || req.Allocated.MvmNum != 3 ||
		req.Allocated.StorageDiskMB != 20 {
		t.Fatalf("Allocated not propagated: %+v", req.Allocated)
	}
	if req.DiskUsage == nil || req.DiskUsage.StorageDiskUsagePer != 50.0 {
		t.Fatalf("DiskUsage not propagated: %+v", req.DiskUsage)
	}
	if !req.MetricTime.Equal(now) {
		t.Fatalf("MetricTime %v want %v", req.MetricTime, now)
	}
}

func TestAttachResourceReportSkipsWhenCollectorAbsent(t *testing.T) {
	t.Cleanup(func() { resourcesource.Set(nil) })

	resourcesource.Set(nil)
	req := &masterclient.UpdateNodeStatusRequest{}
	attachResourceReport(req, time.Now())
	if req.Allocated != nil || req.DiskUsage != nil {
		t.Fatalf("expected zero report when collector absent: %+v", req)
	}
	if !req.MetricTime.IsZero() {
		t.Fatalf("expected zero MetricTime, got %v", req.MetricTime)
	}
}

func TestAttachResourceReportRespectsNilFromCollector(t *testing.T) {
	t.Cleanup(func() { resourcesource.Set(nil) })

	resourcesource.Set(&stubCollector{alloc: nil, disk: nil})
	req := &masterclient.UpdateNodeStatusRequest{}
	attachResourceReport(req, time.Now())
	if req.Allocated != nil || req.DiskUsage != nil || !req.MetricTime.IsZero() {
		t.Fatalf("expected unchanged request: %+v", req)
	}
}

func TestTryUpdateNodeStatusReportsPeriodicallyWithoutNodeChanges(t *testing.T) {
	t.Cleanup(func() { resourcesource.Set(nil) })
	resourcesource.Set(&stubCollector{
		alloc: &resourcesource.AllocatedResources{
			MilliCPU: 1500, MemoryMB: 2048, MvmNum: 2,
		},
	})

	var (
		reqCount int32
		received masterclient.UpdateNodeStatusRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		if r.URL.Path != "/internal/meta/nodes/node-a/status" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"Success"}}`))
	}))
	defer srv.Close()

	snapshot := &cubeletnodemeta.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Labels: map[string]string{
				corev1.LabelOSStable:   goruntime.GOOS,
				corev1.LabelArchStable: goruntime.GOARCH,
			},
		},
	}
	kl := &Cubelet{
		nodeName:                  "node-a",
		masterClient:              masterclient.New(srv.URL, time.Second),
		lastNodeSnapshot:          snapshot,
		lastStatusReportTime:      time.Now(),
		nodeStatusReportFrequency: time.Hour,
		clock:                     clock.RealClock{},
	}

	if err := kl.tryUpdateNodeStatus(context.Background(), 0); err != nil {
		t.Fatalf("recent unchanged update should not fail: %v", err)
	}
	if got := atomic.LoadInt32(&reqCount); got != 0 {
		t.Fatalf("recent unchanged update should not call master, got %d requests", got)
	}

	kl.lastStatusReportTime = time.Now().Add(-2 * time.Hour)
	if err := kl.tryUpdateNodeStatus(context.Background(), 0); err != nil {
		t.Fatalf("stale unchanged update should succeed: %v", err)
	}
	if got := atomic.LoadInt32(&reqCount); got != 1 {
		t.Fatalf("expected exactly one periodic report, got %d", got)
	}
	if received.Allocated == nil || received.Allocated.MilliCPU != 1500 || received.Allocated.MemoryMB != 2048 {
		t.Fatalf("periodic report lost allocated payload: %+v", received.Allocated)
	}
	if received.MetricTime.IsZero() {
		t.Fatalf("periodic report should carry MetricTime: %+v", received)
	}
}
