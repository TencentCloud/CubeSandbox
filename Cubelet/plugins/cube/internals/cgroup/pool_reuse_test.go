// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/pelletier/go-toml"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type reuseTestHandle struct {
	handle.Interface
	exists                         bool
	calls                          []string
	deleteErr, createErr, cleanErr error
}

func (h *reuseTestHandle) IsExist(_ context.Context, _ string) bool { return h.exists }
func (h *reuseTestHandle) Delete(_ context.Context, group string) error {
	h.calls = append(h.calls, "delete "+group)
	if h.deleteErr == nil {
		h.exists = false
	}
	return h.deleteErr
}
func (h *reuseTestHandle) Create(_ context.Context, group string) error {
	h.calls = append(h.calls, "create "+group)
	if h.createErr == nil {
		h.exists = true
	}
	return h.createErr
}
func (h *reuseTestHandle) CleanForReuse(_ context.Context, group string) error {
	h.calls = append(h.calls, "clean "+group)
	return h.cleanErr
}
func (h *reuseTestHandle) List() ([]string, error) { return nil, nil }

func TestPrepareForReuse(t *testing.T) {
	testLock.Lock()
	defer testLock.Unlock()
	saved := l.config
	defer func() { l.config = saved }()
	boom := errors.New("injected failure")
	for _, v2 := range []bool{false, true} {
		for _, tc := range []struct {
			name                           string
			disable                        bool
			deleteErr, createErr, cleanErr error
			want                           []string
			wantErr                        error
		}{
			{name: "default retains group", want: []string{"clean"}},
			{name: "recreates group", disable: true, want: []string{"delete", "create"}},
			{name: "missing group is recreated", disable: true, want: []string{"delete", "create"}},
			{name: "delete failure stops recreation", disable: true, deleteErr: boom, want: []string{"delete"}, wantErr: boom},
			{name: "create failure propagates", disable: true, createErr: boom, want: []string{"delete", "create"}, wantErr: boom},
			{name: "cleanup failure propagates", cleanErr: boom, want: []string{"clean"}, wantErr: boom},
		} {
			version := "v1"
			if v2 {
				version = "v2"
			}
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				l.config = &Config{DisableCgroupReuse: tc.disable}
				h := &reuseTestHandle{exists: tc.name != "missing group is recreated", deleteErr: tc.deleteErr, createErr: tc.createErr, cleanErr: tc.cleanErr}
				other := &reuseTestHandle{}
				p := &cgPool{poolV1Handle: h, poolV2Handle: other}
				if v2 {
					p.poolV1Handle = other
					p.poolV2Handle = h
				}
				id := MakeFullCgID(7, v2, 0)
				err := p.prepareForReuse(context.Background(), id)
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
				} else {
					require.NoError(t, err)
				}
				group := MakeCgroupPathByID(id)
				var want []string
				for _, op := range tc.want {
					want = append(want, op+" "+group)
				}
				require.Equal(t, want, h.calls)
				require.Empty(t, other.calls)
			})
		}
	}
}

func TestTidyRetriesMissingGroupAfterRecreationFailure(t *testing.T) {
	testLock.Lock()
	defer testLock.Unlock()
	saved := l.config
	defer func() { l.config = saved }()
	l.config = &Config{DisableCgroupReuse: true}
	ranger, err := allocator.NewSimpleLinearRanger(0, 10)
	require.NoError(t, err)
	alloc := allocator.NewAllocator[uint16](ranger)
	require.NoError(t, alloc.Assign(7))
	h := &reuseTestHandle{exists: true, createErr: errors.New("create failed")}
	p := &cgPool{poolV1Handle: h, poolV2Handle: &reuseTestHandle{}, dirtySet: make(map[uint32]struct{}), poolV1: cgPoolV1{allocator: alloc, cgRanger: ranger}}
	p.Put(context.Background(), 7)
	require.Contains(t, p.dirtySet, uint32(7))
	require.Contains(t, alloc.All(), uint16(7), "failed slot must remain reserved")
	require.False(t, h.exists)
	h.createErr = nil
	require.Empty(t, p.Tidy())
	require.Empty(t, p.dirtySet)
	require.Empty(t, alloc.All(), "release slot only after successful recreation")
	require.True(t, h.exists)
	group := MakeCgroupPathByID(7)
	require.Equal(t, []string{"delete " + group, "create " + group, "delete " + group, "create " + group}, h.calls)
}

func TestCgroupReuseConfigDefaultAndOverride(t *testing.T) {
	for _, registration := range registry.Graph(func(*plugin.Registration) bool { return false }) {
		if registration.Type != constants.InternalPlugin || registration.ID != constants.CgroupID.ID() {
			continue
		}
		defaults, ok := registration.Config.(*Config)
		require.True(t, ok)
		require.False(t, defaults.DisableCgroupReuse)
		for _, tc := range []struct {
			name, input string
			want        bool
		}{
			{"omitted reuses", "", false},
			{"explicit true recreates", "disable_cgroup_reuse = true", true},
			{"explicit false reuses", "disable_cgroup_reuse = false", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				config := *defaults
				require.NoError(t, toml.Unmarshal([]byte(tc.input), &config))
				require.Equal(t, tc.want, config.DisableCgroupReuse)
			})
		}
		return
	}
	t.Fatal("cgroup plugin registration not found")
}
