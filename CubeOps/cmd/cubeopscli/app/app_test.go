// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/version"
)

// captureStdout runs fn and returns everything written to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	fn()
	_ = w.Close()
	data, _ := io.ReadAll(r)
	_ = r.Close()
	return string(data)
}

// TestVersionFlag exercises the init()-installed cli.VersionPrinter end to end.
func TestVersionFlag(t *testing.T) {
	out := captureStdout(t, func() {
		assert.NoError(t, New().Run([]string{"cubeopscli", "--version"}))
	})
	assert.Contains(t, out, "cubeopscli")
	assert.Contains(t, out, version.ShowVersion())
	assert.Contains(t, out, version.BuildTime)
}
