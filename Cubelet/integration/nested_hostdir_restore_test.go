// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"google.golang.org/protobuf/proto"
)

// TestAppSnapshotRestoreWithReadOnlyParentAndWritableNestedHostDir is a manual
// E2E that requires CUBE_E2E_TEMPLATE_REQUEST to contain the cubelet_request
// returned by `cubemastercli template render`. The snapshot must contain a
// long-running, busybox-compatible container so the permission checks can exec.
func TestAppSnapshotRestoreWithReadOnlyParentAndWritableNestedHostDir(t *testing.T) {
	if !IsCube() {
		t.Skip("nested host_dir restore requires the cube runtime")
	}

	fixture := appSnapshotFixture(t)

	teamDir := t.TempDir()
	agentADir := filepath.Join(teamDir, "members", "agent-a")
	agentBDir := filepath.Join(teamDir, "members", "agent-b")
	require.NoError(t, os.MkdirAll(agentADir, 0o755))
	require.NoError(t, os.MkdirAll(agentBDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(teamDir, "shared-input.txt"), []byte("team-input\n"), 0o644))

	// Repeating the restore catches nondeterministic map iteration. The child is
	// deliberately listed before the parent in every request; Cubelet must still
	// mount the parent first so that the writable child remains visible.
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprintf("restore-%02d", i), func(t *testing.T) {
			suffix := uuid.New().String()
			agentVolumeName := "agent-output-" + suffix
			teamVolumeName := "team-share-" + suffix
			containerTeamDir := "/cube-e2e/nested-" + suffix + "/team"
			containerAgentADir := containerTeamDir + "/members/agent-a"

			req := proto.Clone(fixture).(*cubebox.RunCubeSandboxRequest)
			req.RequestID = uuid.New().String()
			req.Volumes = append(req.Volumes,
				hostDirVolume(agentVolumeName, agentADir),
				hostDirVolume(teamVolumeName, teamDir),
			)
			req.Containers[0].VolumeMounts = append(req.Containers[0].VolumeMounts,
				&cubebox.VolumeMounts{
					Name:          agentVolumeName,
					ContainerPath: containerAgentADir,
				},
				&cubebox.VolumeMounts{
					Name:          teamVolumeName,
					ContainerPath: containerTeamDir,
					Readonly:      true,
				},
			)

			createCtx, createCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			createResp, createErr := cubeClient.Create(createCtx, req)
			createCancel()
			require.NoError(t, createErr)
			require.Equal(t, errorcode.ErrorCode_Success, createResp.GetRet().GetRetCode(),
				"restore sandbox: %s", createResp.GetRet().GetRetMsg())
			require.NotEmpty(t, createResp.GetSandboxID())
			t.Cleanup(func() {
				DestroyCubeboxSuccess(t, createResp.GetSandboxID())
			})

			items := ListCubebox(t, &cubebox.ListCubeSandboxRequest{Id: &createResp.SandboxID})
			require.Len(t, items, 1)
			require.NotEmpty(t, items[0].GetContainers())

			marker := fmt.Sprintf("restore-%02d-ok", i)
			resultPath := filepath.Join(agentADir, fmt.Sprintf("result-%02d", i))
			parentWritePath := filepath.Join(teamDir, fmt.Sprintf("parent-write-%02d", i))
			siblingWritePath := filepath.Join(agentBDir, fmt.Sprintf("sibling-write-%02d", i))
			script := fmt.Sprintf(`set -eu
test "$(cat %s/shared-input.txt)" = "team-input"
! touch %s/parent-write-%02d
! touch %s/members/agent-b/sibling-write-%02d
printf '%%s' '%s' > %s/result-%02d
`, containerTeamDir, containerTeamDir, i, containerTeamDir, i, marker, containerAgentADir, i)

			execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
			execResp, execErr := cubeClient.Exec(execCtx, &cubebox.ExecCubeSandboxRequest{
				RequestID:   uuid.New().String(),
				SandboxId:   createResp.GetSandboxID(),
				ContainerId: items[0].GetContainers()[0].GetId(),
				Args:        []string{"/bin/sh", "-c", script},
				Cwd:         "/",
			})
			execCancel()
			require.NoError(t, execErr)
			require.Equal(t, errorcode.ErrorCode_Success, execResp.GetRet().GetRetCode(),
				"exec permission checks: %s", execResp.GetRet().GetRetMsg())

			require.Eventually(t, func() bool {
				contents, readErr := os.ReadFile(resultPath)
				return readErr == nil && string(contents) == marker
			}, 20*time.Second, 100*time.Millisecond, "writable nested mount did not produce its marker")

			_, statErr := os.Stat(parentWritePath)
			require.ErrorIs(t, statErr, os.ErrNotExist, "read-only parent accepted a write")
			_, statErr = os.Stat(siblingWritePath)
			require.ErrorIs(t, statErr, os.ErrNotExist, "read-only sibling directory accepted a write")
		})
	}
}

func appSnapshotFixture(t *testing.T) *cubebox.RunCubeSandboxRequest {
	t.Helper()

	fixtureJSON := os.Getenv("CUBE_E2E_TEMPLATE_REQUEST")
	if fixtureJSON == "" {
		t.Skip("CUBE_E2E_TEMPLATE_REQUEST must contain a rendered Cubelet template request")
	}
	fixture := &cubebox.RunCubeSandboxRequest{}
	require.NoError(t, json.Unmarshal([]byte(fixtureJSON), fixture))
	require.NotEmpty(t, fixture.GetAnnotations()[constants.MasterAnnotationAppSnapshotTemplateID],
		"fixture must restore an app snapshot template")
	require.NotEmpty(t, fixture.GetContainers())
	return fixture
}

func hostDirVolume(name, hostPath string) *cubebox.Volume {
	return &cubebox.Volume{
		Name: name,
		VolumeSource: &cubebox.VolumeSource{
			HostDirVolumes: &cubebox.HostDirVolumeSources{
				VolumeSources: []*cubebox.HostDirSource{{
					Name:     name,
					HostPath: hostPath,
				}},
			},
		},
	}
}
