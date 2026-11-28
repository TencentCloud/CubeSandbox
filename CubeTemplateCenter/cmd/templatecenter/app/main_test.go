// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

package app

import (
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
)

func TestCoreInitRejectsInvalidArtifactS3Addressing(t *testing.T) {
	t.Setenv(configenv.EnvArtifactStoreBackend, "s3")
	t.Setenv(configenv.EnvS3AddressingStyle, "vritual")
	t.Setenv(configenv.EnvS3Endpoint, "https://storage.example.test")
	t.Setenv(configenv.EnvS3Bucket, "artifacts")
	t.Setenv(configenv.EnvS3AccessKeyID, "ak")
	t.Setenv(configenv.EnvS3SecretAccessKey, "sk")

	err := coreInit(t.Context(), &config.Config{})
	if err == nil || !strings.Contains(err.Error(), `CUBE_S3_ADDRESSING_STYLE: blobstore/s3: invalid addressing_style "vritual"`) {
		t.Fatalf("expected startup to reject the invalid addressing style, got %v", err)
	}
}
