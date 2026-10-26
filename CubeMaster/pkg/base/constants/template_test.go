// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import "testing"

func TestNormalizeAppSnapshotAnnotations(t *testing.T) {
	annotations := map[string]string{
		CubeAnnotationAppSnapshotTemplateVersion: "v2",
	}

	NormalizeAppSnapshotAnnotations(annotations)

	if annotations[CubeAnnotationAppSnapshotVersion] != "v2" {
		t.Fatalf("expected normalized version key to be v2, got %q", annotations[CubeAnnotationAppSnapshotVersion])
	}
	if GetAppSnapshotVersion(annotations) != "v2" {
		t.Fatalf("expected normalized version lookup to return v2, got %q", GetAppSnapshotVersion(annotations))
	}
}
