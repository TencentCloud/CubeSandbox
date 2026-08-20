// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	containerdconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
)

func TestParseCubeLogFileSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      CubeLogFileSize
		wantMB  int
		wantErr bool
	}{
		{in: "", wantMB: DefaultCubeLogFileSizeMB},
		{in: "0", wantMB: DefaultCubeLogFileSizeMB},
		{in: "-1", wantMB: DefaultCubeLogFileSizeMB},
		{in: "500", wantMB: 500},
		{in: "500m", wantMB: 500},
		{in: "500M", wantMB: 500},
		{in: "1g", wantMB: 1024},
		{in: "2g", wantMB: 2048},
		{in: "1gib", wantMB: 1024},
		{in: "512k", wantErr: true},
		{in: "bogus", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			got, err := ParseCubeLogFileSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCubeLogFileSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCubeLogFileSize(%q): %v", tc.in, err)
			}
			if got != tc.wantMB {
				t.Fatalf("ParseCubeLogFileSize(%q) = %d, want %d", tc.in, got, tc.wantMB)
			}
		})
	}
}

func TestCubeLogApplyDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         CubeLogConfig
		wantPath   string
		wantNum    int
		wantSize   CubeLogFileSize
		wantSizeMB int
		wantErr    bool
	}{
		{
			name:       "empty",
			in:         CubeLogConfig{},
			wantPath:   DefaultCubeLogPath,
			wantNum:    DefaultCubeLogFileNum,
			wantSize:   DefaultCubeLogFileSize,
			wantSizeMB: DefaultCubeLogFileSizeMB,
		},
		{
			name: "zero_size_and_num",
			in: CubeLogConfig{
				Path:     "/custom",
				FileNum:  0,
				FileSize: "0",
			},
			wantPath:   "/custom",
			wantNum:    DefaultCubeLogFileNum,
			wantSize:   DefaultCubeLogFileSize,
			wantSizeMB: DefaultCubeLogFileSizeMB,
		},
		{
			name: "zero_unit_size_normalized",
			in: CubeLogConfig{
				Path:     "/custom",
				FileNum:  4,
				FileSize: "0m",
			},
			wantPath:   "/custom",
			wantNum:    4,
			wantSize:   DefaultCubeLogFileSize,
			wantSizeMB: DefaultCubeLogFileSizeMB,
		},
		{
			name: "explicit_unitless_500_kept",
			in: CubeLogConfig{
				Path:     "/keep",
				FileNum:  3,
				FileSize: "500",
			},
			wantPath:   "/keep",
			wantNum:    3,
			wantSize:   "500",
			wantSizeMB: DefaultCubeLogFileSizeMB,
		},
		{
			name: "explicit_500m_kept",
			in: CubeLogConfig{
				Path:     "/keep",
				FileNum:  3,
				FileSize: "500m",
			},
			wantPath:   "/keep",
			wantNum:    3,
			wantSize:   "500m",
			wantSizeMB: DefaultCubeLogFileSizeMB,
		},
		{
			name: "human_size_1g",
			in: CubeLogConfig{
				Path:     "/keep",
				FileNum:  3,
				FileSize: "1g",
			},
			wantPath:   "/keep",
			wantNum:    3,
			wantSize:   "1g",
			wantSizeMB: 1024,
		},
		{
			name: "invalid_size",
			in: CubeLogConfig{
				FileSize: "nope",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.in
			err := got.ApplyDefaults()
			if tc.wantErr {
				if err == nil {
					t.Fatal("ApplyDefaults() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyDefaults(): %v", err)
			}
			if got.Path != tc.wantPath || got.FileNum != tc.wantNum || got.FileSize != tc.wantSize || got.FileSizeMB() != tc.wantSizeMB {
				t.Fatalf("ApplyDefaults() = path=%q num=%d size=%q mb=%d, want path=%q num=%d size=%q mb=%d",
					got.Path, got.FileNum, got.FileSize, got.FileSizeMB(),
					tc.wantPath, tc.wantNum, tc.wantSize, tc.wantSizeMB)
			}
		})
	}
}

func TestLoadConfigParsesCubeLogHumanSize(t *testing.T) {
	t.Parallel()

	path := writeCubeletTOML(t, `
version = 3
[cubelog]
  path = "/tmp/cubelet-log"
  file_num = 4
  file_size = "1g"
`)
	out := newLoadTarget()
	if err := LoadConfig(context.Background(), path, out); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := out.CubeLog.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if out.CubeLog.Path != "/tmp/cubelet-log" {
		t.Fatalf("path = %q, want /tmp/cubelet-log", out.CubeLog.Path)
	}
	if out.CubeLog.FileNum != 4 {
		t.Fatalf("file_num = %d, want 4", out.CubeLog.FileNum)
	}
	if out.CubeLog.FileSize != "1g" {
		t.Fatalf("file_size = %q, want 1g", out.CubeLog.FileSize)
	}
	if out.CubeLog.FileSizeMB() != 1024 {
		t.Fatalf("file_size MiB = %d, want 1024", out.CubeLog.FileSizeMB())
	}
}

func TestLoadConfigParsesCubeLogUnitlessInteger(t *testing.T) {
	t.Parallel()

	path := writeCubeletTOML(t, `
version = 3
[cubelog]
  path = "/tmp/cubelet-log"
  file_num = 4
  file_size = 80
`)
	out := newLoadTarget()
	if err := LoadConfig(context.Background(), path, out); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := out.CubeLog.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if out.CubeLog.FileSizeMB() != 80 {
		t.Fatalf("file_size MiB = %d, want 80", out.CubeLog.FileSizeMB())
	}
}

func TestLoadConfigMissingCubeLogKeepsDefaults(t *testing.T) {
	t.Parallel()

	path := writeCubeletTOML(t, `
version = 3
[http]
  address = ":9998"
`)
	out := newLoadTarget()
	if err := LoadConfig(context.Background(), path, out); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := out.CubeLog.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if out.CubeLog.Path != DefaultCubeLogPath {
		t.Fatalf("path = %q, want %q", out.CubeLog.Path, DefaultCubeLogPath)
	}
	if out.CubeLog.FileNum != DefaultCubeLogFileNum {
		t.Fatalf("file_num = %d, want %d", out.CubeLog.FileNum, DefaultCubeLogFileNum)
	}
	if out.CubeLog.FileSizeMB() != DefaultCubeLogFileSizeMB {
		t.Fatalf("file_size MiB = %d, want %d", out.CubeLog.FileSizeMB(), DefaultCubeLogFileSizeMB)
	}
}

func TestLoadConfigZeroCubeLogFileSizeApplyDefaults(t *testing.T) {
	t.Parallel()

	path := writeCubeletTOML(t, `
version = 3
[cubelog]
  path = ""
  file_num = -2
  file_size = 0
`)
	out := newLoadTarget()
	if err := LoadConfig(context.Background(), path, out); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := out.CubeLog.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if out.CubeLog.Path != DefaultCubeLogPath {
		t.Fatalf("path = %q, want %q", out.CubeLog.Path, DefaultCubeLogPath)
	}
	if out.CubeLog.FileNum != DefaultCubeLogFileNum {
		t.Fatalf("file_num = %d, want %d", out.CubeLog.FileNum, DefaultCubeLogFileNum)
	}
	if out.CubeLog.FileSize != DefaultCubeLogFileSize {
		t.Fatalf("file_size = %q, want %q", out.CubeLog.FileSize, DefaultCubeLogFileSize)
	}
	if out.CubeLog.FileSizeMB() != DefaultCubeLogFileSizeMB {
		t.Fatalf("file_size MiB = %d, want %d", out.CubeLog.FileSizeMB(), DefaultCubeLogFileSizeMB)
	}
}

func TestLoadShippedConfigParsesCubeLog(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "config", "config.toml")
	out := newLoadTarget()
	if err := LoadConfig(context.Background(), path, out); err != nil {
		t.Fatalf("LoadConfig shipped config.toml: %v", err)
	}
	if err := out.CubeLog.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if out.CubeLog.Path != DefaultCubeLogPath {
		t.Fatalf("path = %q, want %q", out.CubeLog.Path, DefaultCubeLogPath)
	}
	if out.CubeLog.FileNum != DefaultCubeLogFileNum {
		t.Fatalf("file_num = %d, want %d", out.CubeLog.FileNum, DefaultCubeLogFileNum)
	}
	if out.CubeLog.FileSizeMB() != DefaultCubeLogFileSizeMB {
		t.Fatalf("file_size MiB = %d, want %d", out.CubeLog.FileSizeMB(), DefaultCubeLogFileSizeMB)
	}
}

func newLoadTarget() *Config {
	return &Config{
		Config: &containerdconfig.Config{Version: 3},
		CubeLog: CubeLogConfig{
			Path:     DefaultCubeLogPath,
			FileNum:  DefaultCubeLogFileNum,
			FileSize: DefaultCubeLogFileSize,
		},
	}
}

func writeCubeletTOML(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	return path
}
