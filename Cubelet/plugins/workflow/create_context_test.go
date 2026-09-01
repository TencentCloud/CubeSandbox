// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestCreateContextIsPauseResume(t *testing.T) {
	t.Parallel()

	require.False(t, (*CreateContext)(nil).IsPauseResume())
	require.False(t, (&CreateContext{}).IsPauseResume())
	require.False(t, (&CreateContext{ReqInfo: &cubebox.RunCubeSandboxRequest{}}).IsPauseResume())

	fromTpl := &CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-abc",
			},
		},
	}
	require.False(t, fromTpl.IsPauseResume())

	pauseResume := &CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			Annotations: map[string]string{
				constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
				constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
			},
		},
	}
	require.True(t, pauseResume.IsPauseResume())
	require.True(t, pauseResume.IsGuestMountRestore())
	require.False(t, fromTpl.IsGuestMountRestore())

	fromSnap := &CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			Annotations: map[string]string{
				constants.MasterAnnotationRuntimeSnapshotID: "snap-runtime1",
			},
		},
	}
	require.False(t, fromSnap.IsPauseResume())
	require.True(t, fromSnap.IsGuestMountRestore())
	require.False(t, (*CreateContext)(nil).IsGuestMountRestore())
	require.False(t, (&CreateContext{}).IsGuestMountRestore())
}
