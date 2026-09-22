// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// runSnapshotWithRootfs captures memory and rootfs in one frozen window.
func runSnapshotWithRootfs(snapshot, rootfs, resume func() error) (snapshotErr, rootfsErr, resumeErr error) {
	if snapshotErr = snapshot(); snapshotErr != nil {
		resumeErr = resume()
		return
	}
	defer func() { resumeErr = resume() }()
	rootfsErr = rootfs()
	return
}

func checkCommitSnapshotDestination(ctx context.Context, backend, snapshotID string, inspect func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error)) error {
	// Include work and sealed objects: S3 may resolve an existing writable
	// memory volume instead of rejecting it, so none may be reused here.
	refs := storage.DefaultTemplateObjectRefs(snapshotID)
	statuses, err := inspect(ctx, backend, refs)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		if status.Exists {
			return fmt.Errorf("%w: name=%s kind=%s", storage.ErrCowObjectAlreadyExists, status.Name, status.Kind)
		}
	}
	return nil
}
