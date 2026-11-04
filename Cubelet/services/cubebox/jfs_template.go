// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

var adoptJFSTemplate = storage.AdoptJFSTemplate

// prepareSharedTemplate makes the jfs template a create starts from usable
// on this node. Master builds a jfs template on one node only and may place
// the create anywhere, so the first create on every other node adopts the
// package before the template is looked up locally.
func prepareSharedTemplate(ctx context.Context, req *cubebox.RunCubeSandboxRequest) error {
	if req == nil {
		return nil
	}
	ann := req.GetAnnotations()
	templateID := strings.TrimSpace(ann[constants.MasterAnnotationAppSnapshotTemplateID])
	if templateID == "" ||
		strings.EqualFold(strings.TrimSpace(ann[constants.MasterAnnotationsAppSnapshotCreate]), "true") ||
		strings.TrimSpace(ann[constants.MasterAnnotationRuntimeSnapshotID]) != "" ||
		isCrossNodeRestore(ann) {
		return nil
	}
	backend, err := storageBackendFromAnnotations(ann)
	if err != nil || backend != cow.BackendJFS {
		return nil
	}
	start := time.Now()
	adopted, err := adoptJFSTemplate(ctx, templateID)
	if err != nil {
		return fmt.Errorf("adopt jfs template %s: %w", templateID, err)
	}
	if adopted {
		log.G(ctx).Infof("adopted jfs template %s in %v", templateID, time.Since(start))
	}
	return nil
}

var jfsTemplateUploadStatus = storage.SnapshotUploadStatus

// waitJFSTemplateUpload runs only on the builder, before publishing success.
// Keep artifacts on failure for the existing template cleanup/retry flow.
func waitJFSTemplateUpload(ctx context.Context, backend, templateID string) error {
	if backend != cow.BackendJFS {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotDefaultWorkTimeout)
	defer cancel()
	delay := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := jfsTemplateUploadStatus(ctx, backend, templateID)
		if err != nil {
			return fmt.Errorf("check template %s upload: %w", templateID, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if status == nil {
			return fmt.Errorf("template %s upload returned no status", templateID)
		}
		switch status.State {
		case cow.RemoteStateReady:
			return nil
		case cow.RemoteStatePending, cow.RemoteStateRunning:
		default:
			return fmt.Errorf("template %s upload state %q: %s", templateID, status.State, status.Message)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 5*time.Second)
	}
}
