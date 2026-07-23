// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package binary

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateParsesPrivateData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-plugin.sh")
	content := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"token\":\"tok\",\"private_data\":\"volumes/vol-1/\",\"error\":\"\"}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	p := New("cos", script)
	info, err := p.Create(context.Background(), "vol-1", "vol-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Token != "tok" {
		t.Fatalf("token=%q", info.Token)
	}
	if info.PrivateData != "volumes/vol-1/" {
		t.Fatalf("private_data=%q", info.PrivateData)
	}
	if info.PluginName != "cos" || info.VolumeID != "vol-1" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestCreateAllowsEmptyPrivateData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-plugin.sh")
	content := "#!/bin/sh\nprintf '%s\\n' '{\"token\":\"\",\"private_data\":\"\",\"error\":\"\"}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	p := New("cos", script)
	info, err := p.Create(context.Background(), "vol-2", "vol-2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.PrivateData != "" {
		t.Fatalf("private_data=%q", info.PrivateData)
	}
}
