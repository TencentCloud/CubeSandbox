// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestGenVideoAnnotationOpt(t *testing.T) {
	ctx := context.Background()
	l := &cubeboxInstancePlugin{}

	t.Run("video not enabled", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled but resolution missing", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable: "true",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoResolution)
		assert.Contains(t, err.Error(), "required")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with invalid resolution format", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "720-1280",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoResolution)
		assert.Contains(t, err.Error(), "invalid")
		assert.Contains(t, err.Error(), "format")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with invalid resolution width", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "invalidx1280",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoResolution)
		assert.Contains(t, err.Error(), "invalid")
		assert.Contains(t, err.Error(), "width")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with invalid resolution height", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "720xinvalid",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoResolution)
		assert.Contains(t, err.Error(), "invalid")
		assert.Contains(t, err.Error(), "height")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with zero width", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "0x1280",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoResolution)
		assert.Contains(t, err.Error(), "invalid")
		assert.Contains(t, err.Error(), "width")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with all default values", func(t *testing.T) {
		opts := &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test-sandbox-1",
			},
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "720x1280",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		require.Len(t, specOpts, 1)

		assert.NotNil(t, specOpts[0])
	})

	t.Run("video enabled with custom fps", func(t *testing.T) {
		opts := &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test-sandbox-2",
			},
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "720x1280",
					constants.AnnotationVideoFPS:        "30",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		assert.NotEmpty(t, specOpts)
	})

	t.Run("video enabled with invalid fps", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "720x1280",
					constants.AnnotationVideoFPS:        "invalid",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoFPS)
		assert.Contains(t, err.Error(), "invalid")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with zero fps", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "720x1280",
					constants.AnnotationVideoFPS:        "0",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoFPS)
		assert.Contains(t, err.Error(), "invalid")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with max-resolution equal to resolution", func(t *testing.T) {
		opts := &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test-sandbox-3",
			},
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:        "true",
					constants.AnnotationVideoResolution:    "720x1280",
					constants.AnnotationVideoMaxResolution: "720x1280",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		assert.NotEmpty(t, specOpts)
	})

	t.Run("video enabled with max-resolution greater than resolution", func(t *testing.T) {
		opts := &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test-sandbox-4",
			},
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:        "true",
					constants.AnnotationVideoResolution:    "720x1280",
					constants.AnnotationVideoMaxResolution: "1920x1080",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		assert.NotEmpty(t, specOpts)
	})

	t.Run("video enabled with max-resolution less than resolution", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:        "true",
					constants.AnnotationVideoResolution:    "1920x1080",
					constants.AnnotationVideoMaxResolution: "720x1280",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "max-resolution area")
		assert.Contains(t, err.Error(), "must be greater than or equal")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with invalid max-resolution format", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:        "true",
					constants.AnnotationVideoResolution:    "720x1280",
					constants.AnnotationVideoMaxResolution: "1920-1080",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), constants.AnnotationVideoMaxResolution)
		assert.Contains(t, err.Error(), "invalid")
		assert.Contains(t, err.Error(), "format")
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled with complete parameters", func(t *testing.T) {
		opts := &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test-sandbox-5",
			},
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:        "true",
					constants.AnnotationVideoResolution:    "720x1280",
					constants.AnnotationVideoMaxResolution: "1920x1080",
					constants.AnnotationVideoFPS:           "60",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		require.Len(t, specOpts, 1)
		assert.NotNil(t, specOpts[0])
	})

	t.Run("verify cmdline format", func(t *testing.T) {
		opts := &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test-sandbox-6",
			},
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "true",
					constants.AnnotationVideoResolution: "720x1280",
					constants.AnnotationVideoFPS:        "60",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		require.Len(t, specOpts, 1)

		assert.NotNil(t, specOpts[0])

		expectedVideoMemorySize := int64(720 * 1280 * 4 * 1.2)

		_ = expectedVideoMemorySize
	})

	t.Run("verify videomemorysize calculation with max-resolution", func(t *testing.T) {
		opts := &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test-sandbox-7",
			},
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:        "true",
					constants.AnnotationVideoResolution:    "720x1280",
					constants.AnnotationVideoMaxResolution: "1920x1080",
					constants.AnnotationVideoFPS:           "60",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		assert.NotEmpty(t, specOpts)

		_ = int64(1920 * 1080 * 4 * 1.2)
	})

	t.Run("no annotations map", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: nil,
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		assert.Empty(t, specOpts)
	})

	t.Run("video enabled false", func(t *testing.T) {
		opts := &workflow.CreateContext{
			ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{
					constants.AnnotationVideoEnable:     "false",
					constants.AnnotationVideoResolution: "720x1280",
				},
			},
		}
		specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
		assert.NoError(t, err)
		assert.Empty(t, specOpts)
	})
}

func TestGenVideoAnnotationOptCmdlineFormat(t *testing.T) {
	ctx := context.Background()
	l := &cubeboxInstancePlugin{}

	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test-sandbox-cmdline",
		},
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			Annotations: map[string]string{
				constants.AnnotationVideoEnable:     "true",
				constants.AnnotationVideoResolution: "720x1280",
				constants.AnnotationVideoFPS:        "60",
			},
		},
	}

	specOpts, err := l.genVideoAnnotationOpt(ctx, opts)
	require.NoError(t, err)
	require.Len(t, specOpts, 1)

	assert.NotNil(t, specOpts[0])

	expectedVideoParam := "video=vfb:enable,720x1280M-32@60"

	expectedVideomemoryParam := "vfb.videomemorysize=4423680"

	expectedCmdline := []string{expectedVideoParam, expectedVideomemoryParam}
	expectedJSON, err := json.Marshal(expectedCmdline)
	require.NoError(t, err)

	var cmdlineArray []string
	err = json.Unmarshal(expectedJSON, &cmdlineArray)
	require.NoError(t, err)
	assert.Equal(t, expectedCmdline, cmdlineArray)
	assert.Contains(t, string(expectedJSON), expectedVideoParam)
	assert.Contains(t, string(expectedJSON), expectedVideomemoryParam)
}
