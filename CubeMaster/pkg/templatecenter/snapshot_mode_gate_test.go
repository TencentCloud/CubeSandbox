// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
	"gorm.io/gorm"
)

// A writable plugin volume is not part of the image, so an fs-only snapshot of
// such a sandbox would let the cold-booted sandbox read another sandbox's data.
func TestSubmitSandboxSnapshotRejectsWritableVolumesForFilesystemOnly(t *testing.T) {
	stubSnapshotCreateLifecycle(t)
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(context.Context, string) (*models.TemplateImageJob, error) {
		return nil, gorm.ErrRecordNotFound
	})

	origLoad := loadSnapshotOriginRequestFn
	defer func() { loadSnapshotOriginRequestFn = origLoad }()
	loadSnapshotOriginRequestFn = func(context.Context, string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return &sandboxtypes.CreateCubeSandboxReq{
			Annotations: map[string]string{
				sandbox.AnnotationPluginVolumeMounts: `[{"name":"data","readonly":false}]`,
			},
		}, nil
	}

	_, err := SubmitSandboxSnapshot(context.Background(), "req-1", "sb-1", "node-1", "10.0.0.1", "", "", snapshotmode.FsOnly)
	if !errors.Is(err, ErrFilesystemOnlySharedVolume) {
		t.Fatalf("err = %v, want ErrFilesystemOnlySharedVolume", err)
	}
}
