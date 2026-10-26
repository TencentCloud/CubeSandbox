// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"fmt"
	"os"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

const knownSandboxTestID = "0123456789abcdef0123456789abcdef"

func registerKnownSandboxTestID(t *testing.T) {
	t.Helper()
	localcache.SetSandboxCache(knownSandboxTestID, &localcache.SandboxCache{
		SandboxID: knownSandboxTestID,
		HostIP:    "10.0.0.1",
	})
	t.Cleanup(func() {
		localcache.DeleteSandboxCache(knownSandboxTestID)
	})
}

// minimalTestConfig is a bare-minimum configuration that passes validate().
// It is written to disk on demand so that tests which require a non-nil
// config.GetConfig() can run without an external fixture file.
const minimalTestConfig = `common:
  http_port: 8089
  default_headless_service_nodes_num: 1

log:
  module: "cubemaster-test"
  path: "/tmp"
  file_size: 10
  file_num: 2
  level: "error"

scheduler:
  priority_select_num: 1
`

func init() {
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		file, err := os.CreateTemp("", "cubemaster-test-conf-*.yaml")
		if err != nil {
			panic(fmt.Sprintf("cannot create test config: %v", err))
		}
		if _, err := file.WriteString(minimalTestConfig); err != nil {
			_ = file.Close()
			panic(fmt.Sprintf("cannot write test config: %v", err))
		}
		if err := file.Close(); err != nil {
			panic(fmt.Sprintf("cannot close test config: %v", err))
		}
		if err := os.Setenv("CUBE_MASTER_CONFIG_PATH", file.Name()); err != nil {
			panic(fmt.Sprintf("cannot set test config path: %v", err))
		}
	}
	config.Init()
}
