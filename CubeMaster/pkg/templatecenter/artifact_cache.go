// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// Only explicitly S3-backed artifacts are eligible for worker cache eviction.
// ArtifactUsesObjectStore also includes the fs blob backend, which may share
// the worker's local disk and must not qualify as a durable remote copy.
func rootfsCacheRecoverable(artifact *models.RootfsArtifact) bool {
	return artifact != nil && strings.EqualFold(strings.TrimSpace(artifact.StorageBackend), "s3")
}
