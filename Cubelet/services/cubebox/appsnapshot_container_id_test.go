// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestSnapshotContainerIDFromAnnotationsUsesExistingAnnotation(t *testing.T) {
	t.Parallel()

	got := snapshotContainerIDFromAnnotations(map[string]string{
		constants.AnnotationAppSnapshotContainerID: "tpl-1e0d677b60a0499c80f49e55_0",
	}, "sb-123")
	if got != "tpl-1e0d677b60a0499c80f49e55_0" {
		t.Fatalf("snapshotContainerIDFromAnnotations=%q, want %q", got, "tpl-1e0d677b60a0499c80f49e55_0")
	}
}

func TestSnapshotContainerIDFromAnnotationsFallsBackToSandboxID(t *testing.T) {
	t.Parallel()

	got := snapshotContainerIDFromAnnotations(nil, "sb-123")
	if got != "sb-123" {
		t.Fatalf("snapshotContainerIDFromAnnotations=%q, want %q", got, "sb-123")
	}
}
