// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

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
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	fmt.Printf("mydir=%s\n", mydir)
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		cfgPath := filepath.Clean(filepath.Join(mydir, "../../../../test/conf.yaml"))
		os.Setenv("CUBE_MASTER_CONFIG_PATH", cfgPath)
		// Create a minimal config file on the fly so that config.Init()
		// succeeds regardless of whether a fixture file was checked in.
		if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
				panic(fmt.Sprintf("cannot create test config dir: %v", err))
			}
			if err := os.WriteFile(cfgPath, []byte(minimalTestConfig), 0644); err != nil {
				panic(fmt.Sprintf("cannot write test config: %v", err))
			}
		}
	}
	config.Init()
}
