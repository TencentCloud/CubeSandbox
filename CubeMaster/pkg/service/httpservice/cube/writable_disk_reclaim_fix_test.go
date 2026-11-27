// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func setCubeBoxReqTemplateForTest(t *testing.T, templateReq *types.CreateCubeSandboxReq) {
	t.Helper()
	original := config.GetConfig().ReqTemplateConf
	t.Cleanup(func() { config.GetConfig().ReqTemplateConf = original })

	raw, err := json.Marshal(templateReq)
	require.NoError(t, err)
	config.GetConfig().ReqTemplateConf = &config.ReqTemplateConf{CubeBoxReqTemplate: string(raw)}
}

// dealCubeboxReqTemplateByLocalConfig is the legacy v1 resolution path, distinct
// from dealCubeboxCreateReqWithTemplateCenter. Writable-disk reclaim supports
// the xfs, jfs and s3 backends, and the template owns the storage backend. A
// caller that supplies a different backend while the stored local template
// enables reclaim on xfs must have the template's xfs backend pinned onto the
// request; the enabled discard policy must never ride along onto a caller
// backend the template did not choose.
func TestLocalConfigWritableDiskReclaimRejectsOrPinsXFSBackend(t *testing.T) {
	for _, backend := range []string{constants.SnapshotBackendS3, constants.SnapshotBackendJFS} {
		t.Run(backend, func(t *testing.T) {
			setCubeBoxReqTemplateForTest(t, &types.CreateCubeSandboxReq{
				Backend: constants.SnapshotBackendXFS,
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim: "true",
				},
				Containers: []*types.Container{{Name: "template-ctr"}},
			})

			req := &types.CreateCubeSandboxReq{
				InstanceType: cubebox.InstanceType_cubebox.String(),
				Backend:      backend,
				Annotations: map[string]string{
					constants.CubeAnnotationStorageBackend: backend,
				},
				Volumes:    []*types.Volume{{Name: "data"}},
				Containers: []*types.Container{{Name: "main"}},
			}

			err := dealCubeboxReqTemplateByLocalConfig(context.Background(), req)
			if err != nil {
				// Rejecting the unsupported backend is an acceptable outcome.
				return
			}

			require.Equal(t, constants.SnapshotBackendXFS, strings.ToLower(strings.TrimSpace(req.Backend)),
				"legacy local-config branch must inherit the template's pinned xfs backend")
			require.Equal(t, constants.SnapshotBackendXFS, req.Annotations[constants.CubeAnnotationStorageBackend],
				"the Master->Cubelet backend annotation must be pinned too")
			require.Equal(t, "true", req.Annotations[constants.CubeAnnotationWritableDiskReclaim],
				"the enabled reclaim policy must still apply once the backend is pinned to xfs")
		})
	}
}

// A legacy locally stored template that enables reclaim on the JuiceFS backend
// is valid: the template owns both the policy and the backend, so creates
// inherit jfs. A conflicting caller backend (xfs or s3) must not override it.
func TestLocalConfigWritableDiskReclaimAcceptsAndInheritsJFSBackend(t *testing.T) {
	stored := []struct {
		name     string
		template *types.CreateCubeSandboxReq
	}{
		{
			name: "backend column jfs",
			template: &types.CreateCubeSandboxReq{
				Backend: constants.SnapshotBackendJFS,
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim: "true",
				},
			},
		},
		{
			name: "legacy storage backend annotation jfs",
			template: &types.CreateCubeSandboxReq{
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim: "true",
					constants.CubeAnnotationStorageBackend:      constants.SnapshotBackendJFS,
				},
			},
		},
	}
	for _, storedCase := range stored {
		for _, callerBackend := range []string{"", constants.SnapshotBackendXFS, constants.SnapshotBackendS3} {
			storedCase, callerBackend := storedCase, callerBackend
			name := storedCase.name + "/caller=" + callerBackend
			t.Run(name, func(t *testing.T) {
				template := *storedCase.template
				template.Containers = []*types.Container{{Name: "template-ctr"}}
				setCubeBoxReqTemplateForTest(t, &template)

				req := &types.CreateCubeSandboxReq{
					InstanceType: cubebox.InstanceType_cubebox.String(),
					Volumes:      []*types.Volume{{Name: "data"}},
					Containers:   []*types.Container{{Name: "main"}},
				}
				if callerBackend != "" {
					req.Backend = callerBackend
					req.Annotations = map[string]string{
						constants.CubeAnnotationStorageBackend: callerBackend,
					}
				}

				require.NoError(t, dealCubeboxReqTemplateByLocalConfig(context.Background(), req))
				require.Equal(t, constants.SnapshotBackendJFS, req.Backend,
					"the stored jfs backend must win over the caller")
				require.Equal(t, constants.SnapshotBackendJFS, req.Annotations[constants.CubeAnnotationStorageBackend],
					"the Master->Cubelet backend annotation must inherit jfs")
				require.Equal(t, "true", req.Annotations[constants.CubeAnnotationWritableDiskReclaim],
					"the enabled reclaim policy must apply on jfs")
			})
		}
	}
}

// A legacy locally stored template that enables reclaim on the S3 backend is
// valid now that CubeS3lvol exposes a real discard path: the template owns both
// the policy and the backend, so creates inherit s3. A conflicting caller
// backend (xfs or jfs) must not override it.
func TestLocalConfigWritableDiskReclaimAcceptsAndInheritsS3Backend(t *testing.T) {
	stored := []struct {
		name     string
		template *types.CreateCubeSandboxReq
	}{
		{
			name: "backend column s3",
			template: &types.CreateCubeSandboxReq{
				Backend: constants.SnapshotBackendS3,
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim: "true",
				},
			},
		},
		{
			name: "legacy storage backend annotation s3",
			template: &types.CreateCubeSandboxReq{
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim: "true",
					constants.CubeAnnotationStorageBackend:      constants.SnapshotBackendS3,
				},
			},
		},
	}
	for _, storedCase := range stored {
		for _, callerBackend := range []string{"", constants.SnapshotBackendXFS, constants.SnapshotBackendJFS} {
			storedCase, callerBackend := storedCase, callerBackend
			name := storedCase.name + "/caller=" + callerBackend
			t.Run(name, func(t *testing.T) {
				template := *storedCase.template
				template.Containers = []*types.Container{{Name: "template-ctr"}}
				setCubeBoxReqTemplateForTest(t, &template)

				req := &types.CreateCubeSandboxReq{
					InstanceType: cubebox.InstanceType_cubebox.String(),
					Volumes:      []*types.Volume{{Name: "data"}},
					Containers:   []*types.Container{{Name: "main"}},
				}
				if callerBackend != "" {
					req.Backend = callerBackend
					req.Annotations = map[string]string{
						constants.CubeAnnotationStorageBackend: callerBackend,
					}
				}

				require.NoError(t, dealCubeboxReqTemplateByLocalConfig(context.Background(), req))
				require.Equal(t, constants.SnapshotBackendS3, req.Backend,
					"the stored s3 backend must win over the conflicting caller backend")
				require.Equal(t, constants.SnapshotBackendS3, req.Annotations[constants.CubeAnnotationStorageBackend],
					"the Master->Cubelet backend annotation must inherit s3")
				require.Equal(t, "true", req.Annotations[constants.CubeAnnotationWritableDiskReclaim],
					"the enabled reclaim policy must apply on s3")
			})
		}
	}
}

// rejectCallerSuppliedWritableDiskReclaim is the public HTTP create guard. The
// key is template-owned: a request with no template (or a non-cubebox instance
// type) may not carry it at all, while a template-backed cubebox create is
// deferred to the stored-template resolver.
func TestRejectCallerSuppliedWritableDiskReclaim(t *testing.T) {
	cubeboxType := cubebox.InstanceType_cubebox.String()
	tests := []struct {
		name              string
		req               *types.CreateCubeSandboxReq
		wantErrContains   string
		wantAnnotationsIn map[string]string
	}{
		{
			name: "forged reclaim without a template is rejected",
			req: &types.CreateCubeSandboxReq{
				InstanceType: cubeboxType,
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim: "true",
				},
			},
			wantErrContains: "writable",
		},
		{
			name: "explicit false without a template is rejected",
			req: &types.CreateCubeSandboxReq{
				InstanceType: cubeboxType,
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim: "false",
				},
			},
			wantErrContains: "writable",
		},
		{
			name: "reclaim on a non-cubebox instance with a template id is rejected",
			req: &types.CreateCubeSandboxReq{
				InstanceType: "runc",
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim:   "true",
					constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
				},
			},
			wantErrContains: "writable",
		},
		{
			name: "missing key is unchanged",
			req: &types.CreateCubeSandboxReq{
				InstanceType: cubeboxType,
				Annotations:  map[string]string{},
			},
			wantAnnotationsIn: map[string]string{},
		},
		{
			name: "nil annotations is unchanged",
			req: &types.CreateCubeSandboxReq{
				InstanceType: cubeboxType,
			},
		},
		{
			name: "nil request is unchanged",
			req:  nil,
		},
		{
			name: "template-backed cubebox request is deferred to the stored resolver",
			req: &types.CreateCubeSandboxReq{
				InstanceType: cubeboxType,
				Annotations: map[string]string{
					constants.CubeAnnotationWritableDiskReclaim:   "true",
					constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
				},
			},
			wantAnnotationsIn: map[string]string{
				constants.CubeAnnotationWritableDiskReclaim:   "true",
				constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectCallerSuppliedWritableDiskReclaim(tt.req)
			if tt.wantErrContains != "" {
				require.Error(t, err)
				require.Contains(t, strings.ToLower(err.Error()), tt.wantErrContains)
				return
			}
			require.NoError(t, err)
			if tt.wantAnnotationsIn != nil {
				require.Equal(t, tt.wantAnnotationsIn, tt.req.Annotations,
					"the guard must not mutate the create request")
			}
		})
	}
}
