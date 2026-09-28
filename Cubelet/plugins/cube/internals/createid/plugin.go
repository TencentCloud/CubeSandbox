// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package createid

import (
	"context"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type Config struct {
	RootPath string `toml:"root_path"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.CreateID.ID(),
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			return &local{}, nil
		},
	})
}

type local struct {
}

func (l *local) ID() string {
	return constants.CreateID.ID()
}
func (l *local) Init(ctx context.Context, opts *workflow.InitInfo) error {
	return nil
}
func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "opts nil")
	}

	// Resume-from-pause (and other same-ID recreate paths) pass an explicit
	// sandbox ID via annotation so the new shim/task reuses the caller's ID.
	if desired := desiredSandboxID(opts); desired != "" {
		opts.SandboxID = desired
		return nil
	}

	opts.SandboxID = utils.GenerateID()

	if opts.IsCreateSnapshot() {

		templateID, ok := opts.GetSnapshotTemplateID()
		if !ok {
			return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "cube.master.appsnapshot.template.id should provide")
		}

		opts.SandboxID = templateID + "_" + "0"
	}
	return nil
}

func desiredSandboxID(opts *workflow.CreateContext) string {
	if opts == nil || opts.ReqInfo == nil {
		return ""
	}
	annos := opts.ReqInfo.GetAnnotations()
	if len(annos) == 0 {
		return ""
	}
	return annos[constants.MasterAnnotationDesiredSandboxID]
}

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	return nil
}

func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	return nil
}
