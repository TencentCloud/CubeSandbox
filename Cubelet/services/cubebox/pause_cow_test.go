// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestResolvePauseSnapshotID(t *testing.T) {
	t.Parallel()
	_, err := resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{})
	if err == nil {
		t.Fatal("expected error when snapshot id missing")
	}

	id, err := resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: "snap-abc123def456abc123def456",
		},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != "snap-abc123def456abc123def456" {
		t.Fatalf("id=%q", id)
	}

	_, err = resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: "pause-sbx1",
		},
	})
	if err == nil {
		t.Fatal("expected error for non snap- prefix")
	}

	id, err = resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationRuntimeSnapshotID: "snap-from-runtime000000000001",
		},
	})
	if err != nil {
		t.Fatalf("runtime fallback err: %v", err)
	}
	if id != "snap-from-runtime000000000001" {
		t.Fatalf("id=%q", id)
	}
}
