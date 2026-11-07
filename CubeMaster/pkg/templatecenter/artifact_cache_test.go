// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestRootfsCacheRecoverableRequiresS3(t *testing.T) {
	for _, backend := range []string{"", "fs", "s3"} {
		got := rootfsCacheRecoverable(&models.RootfsArtifact{StorageBackend: backend, ArtifactURL: "https://object.example/old-signed-url"})
		if got != (backend == "s3") {
			t.Fatalf("backend=%q recoverable=%t", backend, got)
		}
	}
	if rootfsCacheRecoverable(nil) {
		t.Fatal("missing artifact accepted")
	}
}
