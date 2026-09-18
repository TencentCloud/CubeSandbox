// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

// fakePusher stubs OpsAgentPusher.
type fakePusher struct {
	mu      sync.Mutex
	calls   int
	hostIPs []string
	applied bool
	err     error
}

func (f *fakePusher) PushQuota(ctx context.Context, hostIP string, req model.OpsAgentPushRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.hostIPs = append(f.hostIPs, hostIP)
	return f.applied, f.err
}

func newQuotaTestService(t *testing.T) (*service.NodeService, *fakeNodeStore, *fakePusher) {
	t.Helper()
	svc, fs := newTestService(t)
	fp := &fakePusher{applied: true}
	svc.SetOpsAgentPusher(fp)
	return svc, fs, fp
}

func registerQuotaNode(t *testing.T, svc *service.NodeService) {
	t.Helper()
	_, err := svc.RegisterNode(context.Background(), &model.RegisterNodeRequest{
		NodeID:   "node-q",
		HostIP:   "10.0.0.1",
		Capacity: model.ResourceSnapshot{MilliCPU: 16000, MemoryMB: 39799},
		QuotaCPU: 32000, QuotaMemMB: 49748, MaxMvmNum: 97,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestSetNodeQuotaPersistsAuditsPushes(t *testing.T) {
	svc, fs, fp := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	view, push, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MCpuLimit: 64000, MemLimit: "64Gi", PausedReleaseRatio: floatPtr(0.5)}, "alice")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !push.Applied {
		t.Fatalf("push result: %+v", push)
	}
	if view.Spec == nil || view.Spec.MCpuLimit != 64000 || view.Spec.Revision != 1 {
		t.Fatalf("view spec: %+v", view.Spec)
	}
	if fp.calls != 1 || fp.hostIPs[0] != "10.0.0.1" {
		t.Fatalf("pusher calls=%d hosts=%v", fp.calls, fp.hostIPs)
	}
	spec, err := fs.GetQuotaSpec(ctx, "node-q")
	if err != nil || spec.MCpuLimit != 64000 || spec.Revision != 1 || spec.UpdatedBy != "alice" {
		t.Fatalf("stored spec: %+v err=%v", spec, err)
	}
	ops, _ := fs.ListOperations(ctx, "node-q", 10)
	if len(ops) != 1 || ops[0].Type != model.OpSetQuota || ops[0].Operator != "alice" {
		t.Fatalf("audit rows: %+v", ops)
	}
	if !strings.Contains(ops[0].Detail, `"paused_release_ratio":0.5`) || !strings.Contains(ops[0].Detail, `"node_managed":true`) {
		t.Fatalf("audit detail must carry ratio and managed flag: %s", ops[0].Detail)
	}
}

func TestSetNodeQuotaRevisionBumpsAndCAS(t *testing.T) {
	svc, _, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	if _, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MvmLimit: 10}, "a"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if _, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MvmLimit: 20}, "b"); err != nil {
		t.Fatalf("second set: %v", err)
	}
	view, _ := svc.GetNodeQuotaView(ctx, "node-q")
	if view.Spec.Revision != 2 {
		t.Fatalf("revision = %d, want 2", view.Spec.Revision)
	}

	// CAS with a stale revision must fail without touching the row.
	_, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MvmLimit: 30, ExpectedRevision: 1}, "c")
	if !errors.Is(err, service.ErrQuotaRevisionConflict) {
		t.Fatalf("want revision conflict, got %v", err)
	}
	view, _ = svc.GetNodeQuotaView(ctx, "node-q")
	if view.Spec.MvmLimit != 20 {
		t.Fatalf("conflicting write must not change the spec: %+v", view.Spec)
	}
}

func TestSetNodeQuotaRejectsOvercommit(t *testing.T) {
	svc, _, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	cases := []struct {
		name string
		spec *model.QuotaSpec
	}{
		{"cpu over guard", &model.QuotaSpec{MCpuLimit: 16000 * 1000 * 21}}, // 16 cores, guard 20
		{"mem just over guard", &model.QuotaSpec{MemLimit: "119398Mi"}},    // 39799MB * 3 + 1
		{"mem over guard", &model.QuotaSpec{MemLimit: "512Gi"}},            // 39799MB * 3 < 524288
		{"bad mem", &model.QuotaSpec{MemLimit: "lots"}},
		{"ratio out of range", &model.QuotaSpec{PausedReleaseRatio: floatPtr(2)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := svc.SetNodeQuota(ctx, "node-q", tc.spec, "a"); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestSetNodeQuotaPushFailureStillPersists(t *testing.T) {
	svc, fs, fp := newQuotaTestService(t)
	fp.err = errors.New("agent down")
	ctx := context.Background()
	registerQuotaNode(t, svc)

	_, push, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MCpuLimit: 32000}, "a")
	if err != nil {
		t.Fatalf("set must survive push failure: %v", err)
	}
	if push.Applied || push.SkipReason == "" {
		t.Fatalf("push result: %+v", push)
	}
	if _, err := fs.GetQuotaSpec(ctx, "node-q"); err != nil {
		t.Fatalf("spec must be persisted for pull reconcile: %v", err)
	}
}

func TestGetNodeQuotaViewDrift(t *testing.T) {
	svc, _, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	// No spec yet.
	view, err := svc.GetNodeQuotaView(ctx, "node-q")
	if err != nil || view.Drift != model.DriftNoSpec {
		t.Fatalf("initial view: %+v err=%v", view, err)
	}

	// Spec matching the heartbeat actual: no drift.
	if _, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MCpuLimit: 32000, MemLimit: "49748Mi"}, "a"); err != nil {
		t.Fatalf("set: %v", err)
	}
	view, _ = svc.GetNodeQuotaView(ctx, "node-q")
	if view.Drift != model.DriftNone {
		t.Fatalf("matching spec should not drift: %+v", view)
	}

	// Heartbeat reports a different cpu quota: drift detected.
	if _, err := svc.UpdateNodeStatus(ctx, "node-q", &model.UpdateNodeStatusRequest{
		Quota: &model.QuotaReport{MilliCPU: 16000, MemMB: 49748, MaxMvmNum: 97},
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	view, _ = svc.GetNodeQuotaView(ctx, "node-q")
	if view.Drift != model.DriftDetected || view.Message == "" {
		t.Fatalf("expected drift with message: %+v", view)
	}
}

func TestGetNodeQuotaViewDriftMemFloorsLikeCubelet(t *testing.T) {
	svc, _, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	// 1536Ki floors to 1MiB in cubelet's reporting; the drift check must
	// truncate the same way instead of using the guard's round-up (2).
	if _, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MemLimit: "1536Ki"}, "a"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := svc.UpdateNodeStatus(ctx, "node-q", &model.UpdateNodeStatusRequest{
		Quota: &model.QuotaReport{MilliCPU: 32000, MemMB: 1, MaxMvmNum: 97},
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	view, _ := svc.GetNodeQuotaView(ctx, "node-q")
	if view.Drift != model.DriftNone {
		t.Fatalf("sub-MiB mem must drift-check with cubelet's truncation: %+v", view)
	}
}

func TestGetOpsAgentSpec(t *testing.T) {
	svc, _, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	resp, err := svc.GetOpsAgentSpec(ctx, "node-q")
	if err != nil || resp.Managed || resp.Spec != nil {
		t.Fatalf("unmanaged response: %+v err=%v", resp, err)
	}
	// Physical is no longer served: the agent measures its own host capacity,
	// so the field stays zero-valued.
	if resp.Physical.CpuTotal != 0 || resp.Physical.MemMBTotal != 0 {
		t.Fatalf("physical should be zero-valued, got %+v", resp.Physical)
	}

	if _, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MCpuLimit: 64000}, "a"); err != nil {
		t.Fatalf("set: %v", err)
	}
	resp, _ = svc.GetOpsAgentSpec(ctx, "node-q")
	if !resp.Managed || resp.Spec == nil || resp.Spec.MCpuLimit != 64000 || resp.Revision != 1 {
		t.Fatalf("managed response: %+v", resp)
	}
}

func TestSetNodeQuotaUnknownNode(t *testing.T) {
	svc, _, _ := newQuotaTestService(t)
	if _, _, err := svc.SetNodeQuota(context.Background(), "ghost", &model.QuotaSpec{}, "a"); err == nil {
		t.Fatal("expected not-found error")
	}
}
func floatPtr(v float64) *float64 { return &v }

func TestResolvePausedRatio(t *testing.T) {
	cases := []struct {
		name      string
		node      *float64
		cluster   *float64
		want      float64
		inherited bool
	}{
		{"explicit override", floatPtr(0.6), floatPtr(0.5), 0.6, false},
		{"explicit zero", floatPtr(0), floatPtr(0.5), 0, false},
		{"inherit cluster default", nil, floatPtr(0.5), 0.5, true},
		{"inherit unset cluster default", nil, nil, 0, true},
		{"explicit wins over nil cluster", floatPtr(1), nil, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, inherited := service.ResolvePausedRatioForTest(tc.node, tc.cluster)
			if got != tc.want || inherited != tc.inherited {
				t.Fatalf("resolvePausedRatio(%v, %v) = (%v, %v), want (%v, %v)",
					tc.node, tc.cluster, got, inherited, tc.want, tc.inherited)
			}
		})
	}
}

func setClusterDefault(t *testing.T, svc *service.NodeService, fs *fakeNodeStore, v *float64) {
	t.Helper()
	if _, _, err := svc.SetClusterQuotaDefaults(context.Background(), v, "test"); err != nil {
		t.Fatalf("set cluster default: %v", err)
	}
	_ = fs
}

func TestSetClusterQuotaDefaults_MaterializesAndPropagates(t *testing.T) {
	svc, fs := newTestService(t)
	fp := &fakePusher{applied: true}
	svc.SetOpsAgentPusher(fp)
	ctx := context.Background()

	// node-a: no row yet. node-b: node-managed with explicit ratio.
	fs.regs["node-a"] = &store.NodeRegistration{NodeID: "node-a", HostIP: "10.0.0.1"}
	fs.regs["node-b"] = &store.NodeRegistration{NodeID: "node-b", HostIP: "10.0.0.2"}
	fs.specs["node-b"] = &store.NodeQuotaSpec{NodeID: "node-b", Revision: 1, NodeManaged: true, PausedReleaseRatio: floatPtr(0.8)}

	defaults, prop, err := svc.SetClusterQuotaDefaults(ctx, floatPtr(0.5), "alice")
	if err != nil {
		t.Fatalf("set defaults: %v", err)
	}
	if defaults.PausedReleaseRatio == nil || *defaults.PausedReleaseRatio != 0.5 {
		t.Fatalf("defaults = %+v", defaults)
	}

	// Sentinel row stores the default.
	sentinel := fs.specs["*"]
	if sentinel == nil || sentinel.PausedReleaseRatio == nil || *sentinel.PausedReleaseRatio != 0.5 {
		t.Fatalf("sentinel = %+v", sentinel)
	}
	// node-a got a cluster-managed row; node-b keeps its explicit ratio.
	rowA := fs.specs["node-a"]
	if rowA == nil || rowA.NodeManaged || rowA.Revision != 2 {
		t.Fatalf("node-a row = %+v", rowA)
	}
	if fs.specs["node-b"].PausedReleaseRatio == nil || *fs.specs["node-b"].PausedReleaseRatio != 0.8 {
		t.Fatalf("node-b ratio = %+v", fs.specs["node-b"].PausedReleaseRatio)
	}
	// Both followers were bumped; only node-a is pushed (node-b has an
	// explicit ratio and is not a follower).
	if prop.Bumped != 1 || prop.Pushed != 1 || prop.Failed != 0 {
		t.Fatalf("propagation = %+v", prop)
	}
	if fp.calls != 1 || fp.hostIPs[0] != "10.0.0.1" {
		t.Fatalf("pusher calls=%d hosts=%v", fp.calls, fp.hostIPs)
	}
	// The cluster-level change is audited on the sentinel row.
	ops, err := fs.ListOperations(ctx, "*", 10)
	if err != nil || len(ops) != 1 || ops[0].Type != model.OpSetClusterQuota || ops[0].Operator != "alice" {
		t.Fatalf("cluster audit rows: %+v err=%v", ops, err)
	}
	if !strings.Contains(ops[0].Detail, `"new":0.5`) || !strings.Contains(ops[0].Detail, `"bumped":1`) {
		t.Fatalf("cluster audit detail: %s", ops[0].Detail)
	}
}

func TestGetOpsAgentSpec_ClusterRowIsOnlyRatio(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	fs.regs["node-a"] = &store.NodeRegistration{NodeID: "node-a"}
	fs.specs["node-a"] = &store.NodeQuotaSpec{NodeID: "node-a", Revision: 2, NodeManaged: false}
	fs.specs["*"] = &store.NodeQuotaSpec{NodeID: "*", Revision: 1, PausedReleaseRatio: floatPtr(0.25)}

	resp, err := svc.GetOpsAgentSpec(ctx, "node-a")
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	if !resp.Managed || !resp.OnlyPausedRatio {
		t.Fatalf("managed=%t onlyRatio=%t", resp.Managed, resp.OnlyPausedRatio)
	}
	if resp.Spec == nil || resp.Spec.PausedReleaseRatio == nil || *resp.Spec.PausedReleaseRatio != 0.25 {
		t.Fatalf("spec ratio not resolved: %+v", resp.Spec)
	}
	if resp.Spec.MCpuLimit != 0 || resp.Spec.MemLimit != "" || resp.Spec.MvmLimit != 0 || resp.Spec.CreationConcurrentNum != 0 {
		t.Fatalf("cluster row must not carry resource fields: %+v", resp.Spec)
	}
}

func TestGetNodeQuotaView_ResolvesInheritedRatio(t *testing.T) {
	svc, fs := newTestService(t)
	fs.regs["node-a"] = &store.NodeRegistration{NodeID: "node-a"}
	fs.specs["node-a"] = &store.NodeQuotaSpec{NodeID: "node-a", Revision: 2, NodeManaged: true}
	fs.specs["*"] = &store.NodeQuotaSpec{NodeID: "*", Revision: 1, PausedReleaseRatio: floatPtr(0.7)}

	view, err := svc.GetNodeQuotaView(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("get view: %v", err)
	}
	if !view.PausedRatioInherited || view.EffectivePausedRatio != 0.7 {
		t.Fatalf("view ratio = %+v (inherited=%t)", view.EffectivePausedRatio, view.PausedRatioInherited)
	}
}

func TestGetNodeQuotaView_RatioDriftDetected(t *testing.T) {
	svc, fs := newTestService(t)
	fs.regs["node-a"] = &store.NodeRegistration{NodeID: "node-a", PausedReleaseRatio: floatPtr(0.3)}
	fs.specs["node-a"] = &store.NodeQuotaSpec{NodeID: "node-a", Revision: 2, NodeManaged: true}
	fs.specs["*"] = &store.NodeQuotaSpec{NodeID: "*", Revision: 1, PausedReleaseRatio: floatPtr(0.5)}

	view, err := svc.GetNodeQuotaView(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("get view: %v", err)
	}
	if view.Drift != model.DriftDetected {
		t.Fatalf("drift = %s, want detected", view.Drift)
	}
	if view.Actual.PausedReleaseRatio == nil || *view.Actual.PausedReleaseRatio != 0.3 {
		t.Fatalf("actual ratio = %+v", view.Actual.PausedReleaseRatio)
	}
}

// TestSetNodeQuotaRejectsSSRFTargets verifies hostile host_ip skips the push.
func TestSetNodeQuotaRejectsSSRFTargets(t *testing.T) {
	hostile := []string{
		"127.0.0.1",       // loopback
		"169.254.169.254", // link-local (cloud metadata)
		"0.0.0.0",         // unspecified
		"224.0.0.1",       // multicast
		"node-1",          // not an IP literal
		"10.0.0.1:8890",   // IP with port
		"",                // empty
	}
	for _, host := range hostile {
		t.Run(host, func(t *testing.T) {
			svc, fs, fp := newQuotaTestService(t)
			ctx := context.Background()
			if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{
				NodeID: "node-ssrf", HostIP: host,
			}); err != nil {
				t.Fatalf("register: %v", err)
			}
			_, push, err := svc.SetNodeQuota(ctx, "node-ssrf", &model.QuotaSpec{MCpuLimit: 32000}, "a")
			if err != nil {
				t.Fatalf("set must survive skip: %v", err)
			}
			if push.Applied || push.SkipReason == "" {
				t.Fatalf("hostile target %q must skip push, got %+v", host, push)
			}
			if fp.calls != 0 {
				t.Fatalf("push must not be issued for %q, calls=%d", host, fp.calls)
			}
			if _, err := fs.GetQuotaSpec(ctx, "node-ssrf"); err != nil {
				t.Fatalf("spec must persist for pull reconcile: %v", err)
			}
		})
	}
}

// TestSetNodeQuotaConcurrentRevisionNoLostUpdate verifies concurrent writers
// produce strictly increasing revisions via the atomic CAS update.
func TestSetNodeQuotaConcurrentRevisionNoLostUpdate(t *testing.T) {
	svc, fs, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MCpuLimit: int64(1000 + i)}, "a")
			if err != nil && !errors.Is(err, service.ErrQuotaRevisionConflict) {
				t.Errorf("set %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	spec, err := fs.GetQuotaSpec(ctx, "node-q")
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	// Successes = revision (first insert is 1, then +1 per successful update).
	if spec.Revision < 1 {
		t.Fatalf("revision must be positive: %d", spec.Revision)
	}
}

// TestSetNodeQuotaConcurrentFirstWrite verifies the first-write path has the
// same conflict contract as the update path (no silent overwrite, 409 on race).
func TestSetNodeQuotaConcurrentFirstWrite(t *testing.T) {
	svc, fs, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	const n = 20
	var wg sync.WaitGroup
	var succeeded, conflicted int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MCpuLimit: int64(1000 + i)}, "a")
			switch {
			case err == nil:
				atomic.AddInt64(&succeeded, 1)
			case errors.Is(err, service.ErrQuotaRevisionConflict):
				atomic.AddInt64(&conflicted, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// final revision == successful writes; a silent overwrite would collapse it.
	spec, err := fs.GetQuotaSpec(ctx, "node-q")
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	if spec.Revision != succeeded {
		t.Fatalf("revision %d != successful writes %d: a write was silently overwritten", spec.Revision, succeeded)
	}
	if succeeded+conflicted != n {
		t.Fatalf("succeeded(%d) + conflicted(%d) != %d", succeeded, conflicted, n)
	}
}

func TestSetNodeQuotaRejectsCorruptCapacity(t *testing.T) {
	svc, fs, _ := newQuotaTestService(t)
	ctx := context.Background()
	registerQuotaNode(t, svc)

	// Corrupt capacity snapshot: the write must fail closed, not skip the guard.
	fs.regs["node-q"].CapacityJSON = "{not valid json"

	_, _, err := svc.SetNodeQuota(ctx, "node-q", &model.QuotaSpec{MCpuLimit: 999999999}, "a")
	if err == nil {
		t.Fatal("corrupt capacity must reject the write")
	}
	if !errors.Is(err, service.ErrQuotaValidation) {
		t.Fatalf("want ErrQuotaValidation, got %v", err)
	}
	if _, err := fs.GetQuotaSpec(ctx, "node-q"); err == nil {
		t.Fatal("no spec must be persisted on corrupt capacity")
	}
}
