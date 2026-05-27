// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestSetRuntimeSnapshotBindingLabels(t *testing.T) {
	cb := &cubeboxstore.CubeBox{}
	attachedAt := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)

	setRuntimeSnapshotBindingLabels(cb, "snap-1", attachedAt)

	if got := cb.Labels[constants.MasterAnnotationRuntimeSnapshotID]; got != "snap-1" {
		t.Fatalf("runtime snapshot id = %q, want snap-1", got)
	}
	if got := cb.Labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt]; got != attachedAt.Format(time.RFC3339Nano) {
		t.Fatalf("runtime snapshot attached at = %q, want %q", got, attachedAt.Format(time.RFC3339Nano))
	}
}

func TestSetRuntimeSnapshotBindingLabelsSkipsEmptyID(t *testing.T) {
	cb := &cubeboxstore.CubeBox{}
	setRuntimeSnapshotBindingLabels(cb, "", time.Now().UTC())
	if len(cb.Labels) != 0 {
		t.Fatalf("expected no labels when snapshot id empty, got %v", cb.Labels)
	}
}
