// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestStatusStatePrefersPauseOverFinished(t *testing.T) {
	now := time.Now().UnixNano()
	cases := []struct {
		name string
		s    Status
		want cubebox.ContainerState
	}{
		{
			name: "pausing beats finished",
			s:    Status{StartedAt: now, PausingAt: now, FinishedAt: now},
			want: cubebox.ContainerState_CONTAINER_PAUSING,
		},
		{
			name: "paused beats finished",
			s:    Status{StartedAt: now, PausedAt: now, FinishedAt: now},
			want: cubebox.ContainerState_CONTAINER_PAUSED,
		},
		{
			name: "finished without pause is exited",
			s:    Status{StartedAt: now, FinishedAt: now},
			want: cubebox.ContainerState_CONTAINER_EXITED,
		},
		{
			name: "running",
			s:    Status{StartedAt: now},
			want: cubebox.ContainerState_CONTAINER_RUNNING,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.s.State())
		})
	}
}

func TestStatusIsPausedWhileFinishedAtSet(t *testing.T) {
	now := time.Now().UnixNano()
	st := StoreStatus(Status{StartedAt: now, PausingAt: now, FinishedAt: now})
	assert.True(t, st.IsPaused())
	assert.False(t, st.IsTerminated())
}
