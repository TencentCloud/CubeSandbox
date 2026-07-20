// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
)

func TestDefaultConfigOsImageOnData(t *testing.T) {
	cfg := defaultConfig()
	want := pmem.DefaultCubeboxOsImageDir()
	if cfg.ImageBasePath != want {
		t.Fatalf("ImageBasePath=%q, want %q", cfg.ImageBasePath, want)
	}
	if cfg.KernelBasePath != want {
		t.Fatalf("KernelBasePath=%q, want %q", cfg.KernelBasePath, want)
	}
}
