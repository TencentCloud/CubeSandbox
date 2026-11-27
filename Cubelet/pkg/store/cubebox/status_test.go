// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
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

func TestExitUnobservedIsNotPersisted(t *testing.T) {
	raw, err := json.Marshal(Status{StartedAt: 1, FinishedAt: 2, ExitUnobserved: true})
	assert.NoError(t, err)
	assert.NotContains(t, string(raw), "xitUnobserved")

	var restored Status
	assert.NoError(t, json.Unmarshal(raw, &restored))
	assert.False(t, restored.ExitUnobserved, "a persisted row cannot claim an unobserved exit")
}

func TestExitUnobservedDoesNotChangeState(t *testing.T) {
	now := time.Now().UnixNano()
	assert.Equal(t, cubebox.ContainerState_CONTAINER_EXITED,
		Status{StartedAt: now, FinishedAt: now, ExitUnobserved: true}.State())
	assert.Equal(t, cubebox.ContainerState_CONTAINER_UNKNOWN,
		Status{StartedAt: now, Unknown: true, ExitUnobserved: true}.State())
}

func TestExitUnobservedSurvivesDeepCopy(t *testing.T) {
	st := StoreStatus(Status{StartedAt: 1, FinishedAt: 2, ExitUnobserved: true})
	assert.True(t, st.DeepCopy().Get().ExitUnobserved)
}
