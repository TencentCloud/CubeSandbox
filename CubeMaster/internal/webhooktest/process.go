// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhooktest

import (
	"os"
	"os/exec"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// Isolated reclaims InitTask's global configuration watchers with the process.
// Call only from a top-level test, before installing any dependency patches.
func Isolated(t *testing.T) bool {
	t.Helper()
	if os.Getenv("WEBHOOK_TEST_FIXTURE_PROCESS") == t.Name() {
		return false
	}
	args := append(os.Args[1:], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "WEBHOOK_TEST_FIXTURE_PROCESS="+t.Name())
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
	return true
}
