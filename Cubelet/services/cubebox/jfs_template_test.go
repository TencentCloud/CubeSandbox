// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestPrepareSharedTemplateAdoptsOnlyJFSTemplateCreates(t *testing.T) {
	var adopted []string
	previous := adoptJFSTemplate
	adoptJFSTemplate = func(_ context.Context, id string) (bool, error) {
		adopted = append(adopted, id)
		return true, nil
	}
	t.Cleanup(func() { adoptJFSTemplate = previous })

	create := func(extra map[string]string) {
		ann := map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: "tpl-1",
			constants.MasterAnnotationStorageBackend:        cow.BackendJFS,
		}
		for k, v := range extra {
			ann[k] = v
		}
		require.NoError(t, prepareSharedTemplate(context.Background(), &cubebox.RunCubeSandboxRequest{Annotations: ann}))
	}
	create(nil)
	create(map[string]string{constants.MasterAnnotationStorageBackend: cow.BackendXFS})
	create(map[string]string{constants.MasterAnnotationsAppSnapshotCreate: "true"})
	create(map[string]string{constants.MasterAnnotationRuntimeSnapshotID: "snap-1"})
	create(map[string]string{constants.MasterAnnotationSnapshotCrossNode: "true"})
	create(map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: ""})
	require.Equal(t, []string{"tpl-1"}, adopted)
}

func TestWaitJFSTemplateUpload(t *testing.T) {
	previous := jfsTemplateUploadStatus
	t.Cleanup(func() { jfsTemplateUploadStatus = previous })
	t.Run("other backends bypass upload check", func(t *testing.T) {
		jfsTemplateUploadStatus = func(context.Context, string, string) (*cow.RemoteStatus, error) {
			t.Fatal("non-JFS template queried upload status")
			return nil, nil
		}
		for _, backend := range []string{cow.BackendXFS, cow.BackendS3} {
			require.NoError(t, waitJFSTemplateUpload(context.Background(), backend, "tpl"))
		}
	})
	t.Run("wait until ready", func(t *testing.T) {
		calls := 0
		jfsTemplateUploadStatus = func(_ context.Context, backend, id string) (*cow.RemoteStatus, error) {
			require.Equal(t, cow.BackendJFS, backend)
			require.Equal(t, "tpl", id)
			calls++
			state := cow.RemoteStateRunning
			if calls == 2 {
				state = cow.RemoteStateReady
			}
			return &cow.RemoteStatus{State: state}, nil
		}
		require.NoError(t, waitJFSTemplateUpload(context.Background(), cow.BackendJFS, "tpl"))
		require.Equal(t, 2, calls)
	})
	for _, state := range []string{cow.RemoteStateFailed, "unknown", ""} {
		t.Run("reject state "+state, func(t *testing.T) {
			jfsTemplateUploadStatus = func(context.Context, string, string) (*cow.RemoteStatus, error) {
				return &cow.RemoteStatus{State: state}, nil
			}
			require.Error(t, waitJFSTemplateUpload(context.Background(), cow.BackendJFS, "tpl"))
		})
	}
	t.Run("inspection error", func(t *testing.T) {
		failure := errors.New("cache unavailable")
		jfsTemplateUploadStatus = func(context.Context, string, string) (*cow.RemoteStatus, error) { return nil, failure }
		require.ErrorIs(t, waitJFSTemplateUpload(context.Background(), cow.BackendJFS, "tpl"), failure)
	})
	t.Run("nil status", func(t *testing.T) {
		jfsTemplateUploadStatus = func(context.Context, string, string) (*cow.RemoteStatus, error) { return nil, nil }
		require.Error(t, waitJFSTemplateUpload(context.Background(), cow.BackendJFS, "tpl"))
	})
	t.Run("cancel while uploading", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		jfsTemplateUploadStatus = func(context.Context, string, string) (*cow.RemoteStatus, error) {
			cancel()
			return &cow.RemoteStatus{State: cow.RemoteStateRunning}, nil
		}
		require.ErrorIs(t, waitJFSTemplateUpload(ctx, cow.BackendJFS, "tpl"), context.Canceled)
	})
	t.Run("deadline during backoff", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		calls := 0
		jfsTemplateUploadStatus = func(context.Context, string, string) (*cow.RemoteStatus, error) {
			calls++
			return &cow.RemoteStatus{State: cow.RemoteStatePending}, nil
		}
		require.ErrorIs(t, waitJFSTemplateUpload(ctx, cow.BackendJFS, "tpl"), context.DeadlineExceeded)
		require.Equal(t, 1, calls)
	})
}
