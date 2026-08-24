// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package appsnapshot

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: constants.InternalPlugin,
		ID:   constants.APPSnapshotID.ID(),
		Requires: []plugin.Type{
			constants.ControllerPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			obj, err := ic.GetByID(constants.ControllerPlugin, constants.PluginRunTemplateManager.ID())
			if err != nil {
				return nil, fmt.Errorf("failed to get run template manager: %w", err)
			}

			return &appsnapshotCompleter{
				runtemplateManager: obj.(runtemplate.RunTemplateManager),
			}, nil
		},
	})
}

type appsnapshotCompleter struct {
	runtemplateManager runtemplate.RunTemplateManager
}

func (l *appsnapshotCompleter) ID() string {
	return constants.APPSnapshotID.ID()
}
func (l *appsnapshotCompleter) Init(ctx context.Context, opts *workflow.InitInfo) error {
	return nil
}
func (l *appsnapshotCompleter) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "opts nil")
	}

	templateID, ok := opts.GetSnapshotTemplateID()
	if !ok {
		return nil
	}

	if opts.IsCreateSnapshot() {
		return nil
	}

	if !opts.IsCubeboxV2() {
		return nil
	}

	ann := opts.ReqInfo.GetAnnotations()
	// A restore carries the same kernel／image metadata the template would
	// have, so whatever holds that metadata on this node can stand in for a
	// template that was never built here.
	fallbackID := strings.TrimSpace(ann[constants.MasterAnnotationRuntimeSnapshotID])
	if imp := storage.CrossNodeSandboxImport(ann); imp != nil {
		// Cross-node there is no package here to stand in: the description
		// arrived on the sandbox's own metadata disk, mounted under its home
		// by the Create entry. Recovery reads whichever id owns that home.
		fallbackID = imp.SandboxID
	}
	// Pause snaps are storage catalogs, not Cube run templates, so Resume
	// asks for the original tpl-* first.
	if opts.IsPauseResume() {
		if orig := strings.TrimSpace(ann[constants.MasterAnnotationAppSnapshotTemplateID]); orig != "" {
			templateID = orig
		}
	}

	lrt, err := l.runtemplateManager.EnsureCubeRunTemplate(ctx, templateID)
	if err != nil && fallbackID != "" && fallbackID != templateID {
		fallback, fallbackErr := l.runtemplateManager.EnsureCubeRunTemplate(ctx, fallbackID)
		if fallbackErr == nil {
			log.G(ctx).Infof("run template %s is not on this node; restoring from %s instead",
				templateID, fallbackID)
			lrt, err = fallback, nil
		}
	}
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "ensure cube run template %s failed: %v", templateID, err)
	}
	opts.LocalRunTemplate = lrt
	return nil
}

func (l *appsnapshotCompleter) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	return nil
}

func (l *appsnapshotCompleter) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	return nil
}
