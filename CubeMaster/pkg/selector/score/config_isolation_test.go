// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"os"
	"os/exec"
	"regexp"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

const isolatedScoreConfigTestEnv = "CUBEMASTER_ISOLATED_SCORE_CONFIG_TEST"

// runIsolatedScoreConfigTest runs one exact config-mutating test in a child
// process. The child owns any watcher and package-global config created by Init.
func runIsolatedScoreConfigTest(t *testing.T) bool {
	t.Helper()
	if os.Getenv(isolatedScoreConfigTestEnv) == t.Name() {
		return false
	}

	originalConfig := config.GetConfig()
	t.Cleanup(func() {
		if got := config.GetConfig(); got != originalConfig {
			t.Errorf("global config changed in parent process: got %p, want %p", got, originalConfig)
		}
	})

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^"+regexp.QuoteMeta(t.Name())+"$",
		"-test.count=1",
	)
	cmd.Env = append(os.Environ(), isolatedScoreConfigTestEnv+"="+t.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated test process failed: %v\n%s", err, output)
	}
	return true
}
