// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package binary

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
)

func writeAttachStub(t *testing.T, dir, argsFile string) string {
	t.Helper()
	wrapper := filepath.Join(dir, "wrapper.sh")
	content := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > \"" + argsFile + "\"\n" +
		"base=\"\"\n" +
		"prev=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--volume-base-dir\" ]; then base=\"$a\"; fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		"mkdir -p \"$base/vol-1\"\n" +
		"printf '%s\\n' \"{\\\"host_path\\\":\\\"$base/vol-1\\\",\\\"metadata\\\":{},\\\"error\\\":\\\"\\\"}\"\n"
	if err := os.WriteFile(wrapper, []byte(content), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	return wrapper
}

func TestAttachPassesPrivateDataFlag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	wrapper := writeAttachStub(t, dir, argsFile)

	p := New("cos")
	if err := p.Init(context.Background(), volume.PluginConfig{BinaryPath: wrapper}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	baseDir := filepath.Join(dir, "plugin-base")
	res, err := p.Attach(context.Background(), &volume.AttachRequest{
		SandboxID:     "sb-1",
		Namespace:     "default",
		VolumeID:      "vol-1",
		Driver:        "cos",
		VolumeBaseDir: baseDir,
		PrivateData:   "volumes/vol-1/",
		RefCount:      0,
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	wantHost := filepath.Join(baseDir, "vol-1")
	if res.HostPath != wantHost {
		t.Fatalf("host_path=%q want %q", res.HostPath, wantHost)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if !strings.Contains(string(raw), "--private-data volumes/vol-1/") {
		t.Fatalf("Attach argv missing --private-data; got %q", raw)
	}
}

func TestAttachPassesEmptyPrivateDataFlag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	wrapper := writeAttachStub(t, dir, argsFile)

	p := New("cos")
	if err := p.Init(context.Background(), volume.PluginConfig{BinaryPath: wrapper}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	baseDir := filepath.Join(dir, "plugin-base")
	if _, err := p.Attach(context.Background(), &volume.AttachRequest{
		SandboxID:     "sb-1",
		Namespace:     "default",
		VolumeID:      "vol-1",
		VolumeBaseDir: baseDir,
		PrivateData:   "",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if strings.Contains(string(raw), "--private-data") {
		t.Fatalf("empty private_data must omit --private-data flag; got %q", raw)
	}
}
