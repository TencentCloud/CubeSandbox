// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestBuildAnnotationsFromLabelsIncludesRuntimeSnapshotBinding(t *testing.T) {
	labels := map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID:     "snap-1",
		constants.CubeAnnotationRuntimeSnapshotID:         "snap-1",
		constants.CubeAnnotationRuntimeSnapshotAttachedAt: "2026-05-10T09:00:00Z",
	}

	got := buildAnnotationsFromLabels(labels)
	if got[constants.CubeAnnotationAppSnapshotTemplateID] != "snap-1" {
		t.Fatalf("template annotation = %q, want snap-1", got[constants.CubeAnnotationAppSnapshotTemplateID])
	}
	if got[constants.CubeAnnotationRuntimeSnapshotID] != "snap-1" {
		t.Fatalf("runtime snapshot id = %q, want snap-1", got[constants.CubeAnnotationRuntimeSnapshotID])
	}
	if got[constants.CubeAnnotationRuntimeSnapshotAttachedAt] != "2026-05-10T09:00:00Z" {
		t.Fatalf("runtime attached_at = %q, want 2026-05-10T09:00:00Z", got[constants.CubeAnnotationRuntimeSnapshotAttachedAt])
	}
}
