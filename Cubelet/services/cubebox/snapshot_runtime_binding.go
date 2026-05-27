// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

// runtimeSnapshotBindingLabels returns the labels that bind a sandbox to a
// snapshot. v4: only the logical snapshot id (and attach timestamp) are
// recorded on the sandbox metadata. Physical memory volume / dev names are
// not propagated here because Cubelet's local snapshot catalog is the sole
// source of truth and is keyed by the snapshot id.
func runtimeSnapshotBindingLabels(snapshotID string, attachedAt time.Time) map[string]string {
	if snapshotID == "" {
		return nil
	}
	labels := map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: snapshotID,
	}
	if !attachedAt.IsZero() {
		labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt] = attachedAt.UTC().Format(time.RFC3339Nano)
	}
	return labels
}

func setRuntimeSnapshotBindingLabels(cb *cubeboxstore.CubeBox, snapshotID string, attachedAt time.Time) {
	if cb == nil {
		return
	}
	labels := runtimeSnapshotBindingLabels(snapshotID, attachedAt)
	if len(labels) == 0 {
		return
	}
	cb.Metadata.AddLabels(labels)
}
