// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/version"
	"github.com/urfave/cli"
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

func runVersion(args ...string) error {
	app := cli.NewApp()
	app.Name = "cubeopscli"
	app.Commands = []cli.Command{Command}
	return app.Run(append([]string{"cubeopscli"}, args...))
}

func TestVersionCommand_Default(t *testing.T) {
	out := captureStdout(t, func() {
		assert.NoError(t, runVersion("version"))
	})
	assert.Contains(t, out, "cubeopscli")
	assert.Contains(t, out, version.Version)
	assert.Contains(t, out, version.BuildTime)
}

func TestVersionCommand_VersionOnly(t *testing.T) {
	out := captureStdout(t, func() {
		assert.NoError(t, runVersion("version", "--versiononly"))
	})
	assert.Equal(t, version.Version+"\n", out)
	assert.False(t, strings.Contains(out, "built at"))
}
