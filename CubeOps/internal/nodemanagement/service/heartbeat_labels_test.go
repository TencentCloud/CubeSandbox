// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
)

// memSnapshots stands in for the Redis snapshot store.
type memSnapshots struct {
	mu sync.Mutex
	m  map[string][]byte
}

func installMemSnapshots(t *testing.T) *memSnapshots {
	t.Helper()
	ms := &memSnapshots{m: map[string][]byte{}}
	t.Cleanup(nodemetric.SetNodeSnapshotHooks(ms.read, ms.write))
	t.Cleanup(nodemetric.SetScanNodeSnapshotsHook(ms.scan))
	return ms
}

func (ms *memSnapshots) write(s *model.NodeSnapshot) {
	b, _ := json.Marshal(s)
	ms.mu.Lock()
	ms.m[s.NodeID] = b
	ms.mu.Unlock()
}

func (ms *memSnapshots) read(id string) (*model.NodeSnapshot, error) {
	ms.mu.Lock()
	b, ok := ms.m[id]
	ms.mu.Unlock()
	if !ok {
		return nil, nil
	}
	var s model.NodeSnapshot
	return &s, json.Unmarshal(b, &s)
}

func (ms *memSnapshots) scan() ([]*model.NodeSnapshot, error) {
	ms.mu.Lock()
	ids := make([]string, 0, len(ms.m))
	for id := range ms.m {
		ids = append(ids, id)
	}
	ms.mu.Unlock()
	out := make([]*model.NodeSnapshot, 0, len(ids))
	for _, id := range ids {
		s, err := ms.read(id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (ms *memSnapshots) raw(id string) []byte {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return append([]byte(nil), ms.m[id]...)
}

func (ms *memSnapshots) put(id string, b []byte) {
	ms.mu.Lock()
	ms.m[id] = b
	ms.mu.Unlock()
}

// A heartbeat that read the snapshot before an isolation and wrote it after
// must not leave the node schedulable: the database says isolated, and the
// next heartbeat has to bring the snapshot back in line.
func TestHeartbeatDoesNotResurrectLabelsLostToIsolationRace(t *testing.T) {
	ms := installMemSnapshots(t)
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1", HostIP: "10.0.0.1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	beforeIsolation := ms.raw("node-1")

	if _, err := svc.SetNodeSchedulingDisabled(ctx, "node-1", true, "admin", "maintenance"); err != nil {
		t.Fatalf("isolate: %v", err)
	}
	// The racing heartbeat's write lands after the isolation's write.
	ms.put("node-1", beforeIsolation)

	if _, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	stored, _ := ms.read("node-1")
	if !stored.SchedulingDisabled || stored.Labels[model.LabelSchedulingDisabled] != model.LabelSchedulingDisabledValue {
		t.Fatalf("heartbeat wrote back a schedulable snapshot for an isolated node: labels=%v", stored.Labels)
	}
	nodes, err := svc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 1 || !nodes[0].SchedulingDisabled {
		t.Fatalf("ListNodes (what CubeMaster reads) shows the node schedulable")
	}
}

// The same holds for un-isolation: a stale isolated snapshot must not keep
// the node out of scheduling.
func TestHeartbeatAppliesUnisolateFromRegistration(t *testing.T) {
	ms := installMemSnapshots(t)
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})
	_, _ = svc.SetNodeSchedulingDisabled(ctx, "node-1", true, "admin", "")
	isolated := ms.raw("node-1")
	_, _ = svc.SetNodeSchedulingDisabled(ctx, "node-1", false, "admin", "")
	ms.put("node-1", isolated)

	if _, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if stored, _ := ms.read("node-1"); stored.SchedulingDisabled {
		t.Fatalf("stale isolation survived the heartbeat")
	}
}
