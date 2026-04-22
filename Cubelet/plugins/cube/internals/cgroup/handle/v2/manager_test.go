// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package v2

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"

	"github.com/containerd/cgroups/v3"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

func checkCgroupMode(t *testing.T) {
	if cgroups.Mode() == cgroups.Legacy {
		t.Skipf("System running in cgroupv1 mode")
	}
}

func checkValue(t *testing.T, path string, expected string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %q", path)
	}
	if strings.TrimSpace(string(data)) != expected {
		t.Fatalf("expected %q, got %q", expected, string(data))
	}
}

func TestHandler_Base(t *testing.T) {
	checkCgroupMode(t)

	h := &handler{
		root: handle.RootMountPoint,
	}
	testCgroupBase := "/cubelet_cgroupv2_test"
	h.baseGroup = testCgroupBase

	ctx := context.Background()
	testGroup := path.Join(testCgroupBase, "test")
	err := h.Create(ctx, testGroup)
	assert.NoError(t, err, "create")
	checkValue(t, path.Join(h.root, testCgroupBase, "cgroup.subtree_control"), "cpu memory")

	cpu := resource.MustParse("100m")
	mem := resource.MustParse("128Mi")
	err = h.Update(ctx, testGroup, cpu, mem)
	assert.NoError(t, err, "update")

	checkValue(t, path.Join(h.root, testGroup, "cpu.max"), "10000 100000")
	checkValue(t, path.Join(h.root, testGroup, "memory.max"), "134217728")

	assert.Equal(t, 100, h.GetAllocatedCpuNum(testGroup), "get cpu")
	assert.Equal(t, int64(134217728), h.GetAllocatedMem(testGroup), "get mem")

	err = h.RemoveLimit(ctx, testGroup)
	assert.NoError(t, err, "remove limit")
	assert.Equal(t, 0, h.GetAllocatedCpuNum(testGroup), "get cpu")
	assert.Equal(t, int64(0), h.GetAllocatedMem(testGroup), "get mem")

	err = h.Delete(ctx, testGroup)
	assert.NoError(t, err, "delete")
	_, err = os.Stat(path.Join(h.root, testGroup))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("group %q should not exists", testGroup)
	}

	err = h.Delete(ctx, testGroup)
	assert.NoError(t, err, "delete")

	err = h.CleanForReuse(ctx, testGroup)
	assert.NoError(t, err, "reuse")
	checkValue(t, path.Join(h.root, testGroup, "cpu.max"), "max 100000")
	checkValue(t, path.Join(h.root, testGroup, "memory.max"), "max")

	list, err := h.List()
	assert.NoError(t, err, "list")
	assert.Equal(t, []string{"test"}, list)

	err = h.Delete(ctx, testCgroupBase)
	assert.NoError(t, err, "failed to clean test root group %q", testCgroupBase)
}

func TestHandler_Reuse(t *testing.T) {
	checkCgroupMode(t)

	h := &handler{
		root: handle.RootMountPoint,
	}
	testCgroupBase := "/cubelet_cgroupv2_reuse_test"
	h.baseGroup = testCgroupBase

	ctx := context.Background()
	testGroup := path.Join(testCgroupBase, "test")
	err := h.Create(ctx, testGroup)
	assert.NoError(t, err, "create")
	checkValue(t, path.Join(h.root, testCgroupBase, "cgroup.subtree_control"), "cpu memory")

	cmd := exec.Command("sleep", "60")
	err = cmd.Start()
	assert.NoError(t, err, "start process")

	err = h.AddProc(testGroup, uint64(cmd.Process.Pid))
	assert.NoErrorf(t, err, "add proc %d", cmd.Process.Pid)

	err = h.CleanForReuse(ctx, testGroup)
	assert.NoError(t, err, "reuse")

	err = h.Delete(ctx, testGroup)
	assert.NoError(t, err, "delete")
}

func TestHandler_Delete(t *testing.T) {
	checkCgroupMode(t)

	h := &handler{
		root: handle.RootMountPoint,
	}
	testCgroupBase := "/cubelet_cgroupv2_delete_test"
	h.baseGroup = testCgroupBase

	ctx := context.Background()
	testGroup := path.Join(testCgroupBase, "test")
	err := h.Create(ctx, testGroup)
	assert.NoError(t, err, "create")
	checkValue(t, path.Join(h.root, testCgroupBase, "cgroup.subtree_control"), "cpu memory")

	cmd := exec.Command("sleep", "60")
	err = cmd.Start()
	assert.NoError(t, err, "start process")

	err = h.AddProc(testGroup, uint64(cmd.Process.Pid))
	assert.NoErrorf(t, err, "add proc %d", cmd.Process.Pid)

	err = h.Delete(ctx, testGroup)
	assert.NoError(t, err, "delete")
}
