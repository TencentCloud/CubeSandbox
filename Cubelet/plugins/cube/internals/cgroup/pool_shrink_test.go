// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

// Directory enumeration is backed by a persistent tree, not the list of Create
// calls. This models slots that predate the current process and have no DB row.
type shrinkTestHandle struct {
	*poolTestHandle
	root     string
	cleanErr error
}

func (h *shrinkTestHandle) IsExist(_ context.Context, group string) bool {
	_, err := os.Stat(filepath.Join(h.root, group))
	return err == nil
}
func (h *shrinkTestHandle) Create(ctx context.Context, group string) error {
	if err := h.poolTestHandle.Create(ctx, group); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(h.root, group), 0755)
}
func shrinkSubdirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
func (h *shrinkTestHandle) List() ([]string, error) {
	return shrinkSubdirs(filepath.Join(h.root, handle.DefaultPathPoolV1))
}
func (h *shrinkTestHandle) ListSubdir(subdir string) ([]string, error) {
	return shrinkSubdirs(filepath.Join(h.root, handle.DefaultPathPoolV2, subdir))
}
func (h *shrinkTestHandle) CleanForReuse(ctx context.Context, group string) error {
	if err := h.poolTestHandle.CleanForReuse(ctx, group); err != nil {
		return err
	}
	return h.cleanErr
}

func newShrinkTestHandle(t *testing.T) *shrinkTestHandle {
	t.Helper()
	return &shrinkTestHandle{poolTestHandle: newPoolTestHandle(t), root: t.TempDir()}
}

func TestPoolShrinkPreservesUnrecordedDirectories(t *testing.T) {
	for _, activeV2 := range []bool{false, true} {
		t.Run(fmt.Sprint(activeV2), func(t *testing.T) {
			stubPrewarmNodes(t, 1)
			v1, v2 := newShrinkTestHandle(t), newShrinkTestHandle(t)
			for id := 0; id <= 3000; id++ {
				for _, v2slot := range []bool{false, true} {
					h := v1
					if v2slot {
						h = v2
					}
					group := MakeCgroupPathByID(MakeFullCgID(uint16(id), v2slot, 0))
					require.NoError(t, os.MkdirAll(filepath.Join(h.root, group), 0755))
					if id == 500 || id == 2500 {
						require.NoError(t, os.WriteFile(filepath.Join(h.root, group, "cgroup.procs"), []byte("123\n"), 0644))
					}
				}
			}
			p, db := newPoolForTest(t, v1, v2, 1000, activeV2)
			records, err := db.ReadAll(bucket)
			require.NoError(t, err)
			require.Empty(t, records)
			require.NoError(t, p.init())
			t.Cleanup(p.stop)
			require.Empty(t, p.All(), "there are no recovered assignments")
			inactive := v2
			if activeV2 {
				inactive = v1
			}
			require.Empty(t, inactive.recorded().cpus, "the inactive pool must not be bootstrapped")
			require.Empty(t, p.Tidy())
			for _, h := range []*shrinkTestHandle{v1, v2} {
				calls := h.recorded()
				require.Empty(t, calls.deleted, "no historical slot may reach forced deletion")
				require.Empty(t, calls.cleaned, "unrecorded slots are not dirty returned slots")
				require.Empty(t, calls.created, "all warm slots already existed")
			}
			v1dirs, err := v1.List()
			require.NoError(t, err)
			require.Len(t, v1dirs, 3001)
			v2dirs, err := v2.ListSubdir("numa0")
			require.NoError(t, err)
			require.Len(t, v2dirs, 3001)
			for _, id := range []uint16{500, 2500} {
				for _, v2slot := range []bool{false, true} {
					h := v1
					if v2slot {
						h = v2
					}
					group := MakeCgroupPathByID(MakeFullCgID(id, v2slot, 0))
					procs, err := os.ReadFile(filepath.Join(h.root, group, "cgroup.procs"))
					require.NoError(t, err)
					require.Equal(t, "123\n", string(procs))
				}
			}
		})
	}
}

func TestPoolShrinkStillRetriesDirtySlots(t *testing.T) {
	for _, activeV2 := range []bool{false, true} {
		t.Run(strconv.FormatBool(activeV2), func(t *testing.T) {
			stubPrewarmNodes(t, 1)
			h := newShrinkTestHandle(t)
			p := &cgPool{initialSize: 1, poolV1Handle: h, poolV2Handle: h,
				numaAffinityInUse: func() bool { return activeV2 },
				dirtySet:          map[uint32]struct{}{}, pendingSet: map[uint32]struct{}{},
				baselineCache: map[uint32]handle.UsageSnapshot{}, queue: newIDQueue()}
			p.poolV2.enabled = activeV2
			if activeV2 {
				require.NoError(t, p.initPoolV2())
			} else {
				require.NoError(t, p.initPoolV1())
			}
			var dirty []uint32
			for _, id := range []uint16{0, 40} {
				full := MakeFullCgID(id, activeV2, 0)
				if activeV2 {
					require.NoError(t, p.poolV2.initialAssign(full))
				} else {
					require.NoError(t, p.poolV1.initialAssign(full))
				}
				require.NoError(t, os.MkdirAll(filepath.Join(h.root, MakeCgroupPathByID(full)), 0755))
				dirty = append(dirty, full)
				p.dirtySet[full] = struct{}{}
			}
			// One unrecorded directory is outside the range expanded by recovery.
			orphan := MakeCgroupPathByID(MakeFullCgID(99, activeV2, 0))
			require.NoError(t, os.MkdirAll(filepath.Join(h.root, orphan), 0755))
			h.cleanErr = errors.New("returned slot is still busy")
			require.Empty(t, p.Tidy())
			require.Len(t, p.dirtySet, 2)
			require.Empty(t, p.pendingSet)
			require.Zero(t, p.queue.size())
			h.cleanErr = nil
			require.Empty(t, p.Tidy())
			require.Empty(t, p.dirtySet)
			require.Len(t, p.pendingSet, 2)
			require.Equal(t, 2, p.queue.size())
			for range dirty {
				id, ok := p.queue.tryPop()
				require.True(t, ok)
				p.finishBaseline(context.Background(), id)
			}
			require.Empty(t, p.pendingSet)
			require.Empty(t, p.All())
			require.Len(t, h.recorded().cleaned, 4, "each dirty slot was retried")
			require.Empty(t, h.recorded().deleted)
			require.DirExists(t, filepath.Join(h.root, orphan))
		})
	}
}
