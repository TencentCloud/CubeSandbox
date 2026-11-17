// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

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
