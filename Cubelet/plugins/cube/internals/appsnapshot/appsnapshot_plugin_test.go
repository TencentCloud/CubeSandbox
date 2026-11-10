// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package appsnapshot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

type recordingRunTemplateManager struct {
	templateIDs []string
}

func (m *recordingRunTemplateManager) EnsureCubeRunTemplate(
	_ context.Context,
	templateID string,
	_ string,
) (*templatetypes.LocalRunTemplate, error) {
	m.templateIDs = append(m.templateIDs, templateID)
	return &templatetypes.LocalRunTemplate{}, nil
}

func (*recordingRunTemplateManager) ListLocalTemplates(
	context.Context,
) (map[string]*templatetypes.LocalRunTemplate, error) {
	return nil, nil
}

func (*recordingRunTemplateManager) SetInstanceType(string) {}

func TestCreatePauseResumeKeepsJFSVariantTemplateID(t *testing.T) {
	t.Parallel()

	const (
		logicalID = "tpl-a"
		variantID = "tpl-a-hf1-abc"
		pauseID   = "snap-pause"
	)
	manager := &recordingRunTemplateManager{}
	completer := &appsnapshotCompleter{runtemplateManager: manager}
	opts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotVersion:       "v2",
				constants.MasterAnnotationStorageBackend:           "jfs",
				constants.MasterAnnotationAppSnapshotTemplateID:    logicalID,
				constants.MasterAnnotationJFSTemplateVariantPolicy: constants.MasterJFSTemplateVariantPolicyV1,
				constants.MasterAnnotationJFSTemplateVariantID:     variantID,
				constants.MasterAnnotationPauseSnapshotID:          pauseID,
				constants.MasterAnnotationRuntimeSnapshotID:        pauseID,
			},
		},
	}

	err := completer.Create(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, []string{variantID}, manager.templateIDs)
}
