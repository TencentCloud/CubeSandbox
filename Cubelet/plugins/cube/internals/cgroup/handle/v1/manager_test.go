// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package v1

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func checkCgroupMode(t *testing.T) {
	if cgroups.Mode() == cgroups.Unified {
		t.Skipf("System running in cgroupv2 mode")
	}
}

func defaults(root string) []cgroup1.Subsystem {
	s := []cgroup1.Subsystem{
		cgroup1.NewCpu(root),
		cgroup1.NewMemory(root),
	}
	return s
}

func newMock(tb testing.TB) (*mockCgroup, error) {
	root := tb.TempDir()
	subsystems := defaults(root)
	for _, s := range subsystems {
		if err := os.MkdirAll(filepath.Join(root, string(s.Name())), 0o755); err != nil {
			return nil, err
		}
	}

	return &mockCgroup{
		root:       root,
		subsystems: subsystems,
	}, nil
}

type mockCgroup struct {
	root       string
	subsystems []cgroup1.Subsystem
}

func (m *mockCgroup) delete() error {
	return os.RemoveAll(m.root)
}

func (m *mockCgroup) hierarchy() ([]cgroup1.Subsystem, error) {
	return m.subsystems, nil
}

func checkValue(t *testing.T, mock *mockCgroup, path string, expected string) {
	p := filepath.Join(mock.root, path)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read file %q", p)
	}
	if string(data) != expected {
		t.Fatalf("expected %q, got %q", expected, string(data))
	}
}

func TestHandler_Base(t *testing.T) {
	checkCgroupMode(t)
	mock, err := newMock(t)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(mock.root)
	defer func() {
		if err := mock.delete(); err != nil {
			t.Errorf("failed delete: %v", err)
		}
	}()

	h := &handler{
		hierarchy: mock.hierarchy,
		root:      mock.root,
	}

	ctx := context.Background()
	testGroup := "test"
	err = h.Create(ctx, testGroup)
	assert.NoError(t, err, "create")
	for _, s := range mock.subsystems {
		exists, _ := utils.DenExist(path.Join(mock.root, string(s.Name()), testGroup))
		if !exists {
			t.Fatalf("subsystem %q for %q not created", string(s.Name()), testGroup)
		}
	}

	cpu := resource.MustParse("100m")
	mem := resource.MustParse("128Mi")
	err = h.Update(ctx, testGroup, cpu, mem)
	assert.NoError(t, err, "update")

	checkValue(t, mock, path.Join("cpu", testGroup, "cpu.cfs_period_us"), "100000")
	checkValue(t, mock, path.Join("cpu", testGroup, "cpu.cfs_quota_us"), "10000")
	checkValue(t, mock, path.Join("memory", testGroup, "memory.limit_in_bytes"), "134217728")

	assert.Equal(t, 100, h.GetAllocatedCpuNum(testGroup), "get cpu")
	assert.Equal(t, int64(134217728), h.GetAllocatedMem(testGroup), "get mem")

	err = h.RemoveLimit(ctx, testGroup)
	assert.NoError(t, err, "remove limit")
	assert.Equal(t, 0, h.GetAllocatedCpuNum(testGroup), "get cpu")
	assert.Equal(t, int64(0), h.GetAllocatedMem(testGroup), "get mem")

	err = h.Delete(ctx, testGroup)
	assert.NoError(t, err, "delete")

	empty, _ := utils.IsEmpty(path.Join(mock.root, "cpu"))
	assert.Equal(t, true, empty, "cpu dir should be empty")
	empty, _ = utils.IsEmpty(path.Join(mock.root, "memory"))
	assert.Equal(t, true, empty, "memory dir should be empty")

	err = h.CleanForReuse(ctx, testGroup)
	assert.NoError(t, err, "reuse")
	for _, s := range mock.subsystems {
		exists, _ := utils.DenExist(path.Join(mock.root, string(s.Name()), testGroup))
		if !exists {
			t.Fatalf("subsystem %q for %q not created", string(s.Name()), testGroup)
		}
	}

	list, err := h.List()
	assert.NoError(t, err, "list")
	assert.Equal(t, []string{"test"}, list)
}
