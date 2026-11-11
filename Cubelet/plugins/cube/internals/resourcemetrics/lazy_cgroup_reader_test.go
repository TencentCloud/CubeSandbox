// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package resourcemetrics

import (
	"context"
	"testing"

	"github.com/containerd/plugin"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type fakeCgroupUsageReader struct {
	calls int
}

func (f *fakeCgroupUsageReader) UsageSnapshot(context.Context, string) (handle.UsageSnapshot, error) {
	f.calls++
	return handle.UsageSnapshot{CPUUsageTotalNS: 42}, nil
}

func newInitContextWithSet(t *testing.T) (*plugin.InitContext, *plugin.Set) {
	t.Helper()
	set := plugin.NewPluginSet()
	return plugin.NewContext(context.Background(), set, nil), set
}

// TestLazyCgroupUsageReaderResolvesLateRegistration pins the order
// independence of the sibling cgroup-reader lookup: containerd/plugin v1.1
// forbids a plugin from requiring its own type, so there is no graph ordering
// edge between resource-metrics and the cgroup plugin, and neither Instance()
// nor GetByID initializes anything — they only observe what the graph already
// initialized. The resolver must therefore look the sibling up at first
// collection time (when the full plugin set is populated), not at
// resource-metrics init time. Here the reader is created before the sibling
// exists in the set and still resolves once it is added.
func TestLazyCgroupUsageReaderResolvesLateRegistration(t *testing.T) {
	ic, set := newInitContextWithSet(t)

	reader := &lazyCgroupUsageReader{ic: ic}

	// The sibling is added after the resolver was created — the shape that a
	// builtins.go ordering change would produce at plugin-init time.
	sibling := (plugin.Registration{
		Type: constants.InternalPlugin,
		ID:   constants.CgroupID.ID(),
		InitFn: func(*plugin.InitContext) (interface{}, error) {
			return &fakeCgroupUsageReader{}, nil
		},
	}).Init(ic)
	if err := set.Add(sibling); err != nil {
		t.Fatalf("add sibling plugin: %v", err)
	}

	got, err := reader.UsageSnapshot(context.Background(), "/sys/fs/cgroup/foo")
	if err != nil {
		t.Fatalf("UsageSnapshot: %v", err)
	}
	if got.CPUUsageTotalNS != 42 {
		t.Fatalf("CPUUsageTotalNS = %d, want 42", got.CPUUsageTotalNS)
	}
}

func TestLazyCgroupUsageReaderMissingSibling(t *testing.T) {
	ic, _ := newInitContextWithSet(t)

	reader := &lazyCgroupUsageReader{ic: ic}
	if _, err := reader.UsageSnapshot(context.Background(), "/sys/fs/cgroup/foo"); err == nil {
		t.Fatal("UsageSnapshot must fail when the cgroup plugin is absent from the set")
	}
}

func TestLazyCgroupUsageReaderRejectsNonUsageSibling(t *testing.T) {
	ic, set := newInitContextWithSet(t)

	sibling := (plugin.Registration{
		Type: constants.InternalPlugin,
		ID:   constants.CgroupID.ID(),
		InitFn: func(*plugin.InitContext) (interface{}, error) {
			return struct{}{}, nil
		},
	}).Init(ic)
	if err := set.Add(sibling); err != nil {
		t.Fatalf("add sibling plugin: %v", err)
	}

	if _, err := (&lazyCgroupUsageReader{ic: ic}).UsageSnapshot(context.Background(), "/sys/fs/cgroup/foo"); err == nil {
		t.Fatal("UsageSnapshot must fail when the sibling does not expose usage snapshots")
	}
}
