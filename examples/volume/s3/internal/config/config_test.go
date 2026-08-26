// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConf writes a volume-s3.conf and points Load at it.
func writeConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	return path
}

const minimalConf = `
ACCESS_KEY_ID=ak
SECRET_ACCESS_KEY=sk
BUCKET=cube-volumes
ENDPOINT=https://s3.example.com
`

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != DefaultRegion {
		t.Errorf("Region = %q, want %q", cfg.Region, DefaultRegion)
	}
	if cfg.LockDir != DefaultLockDir {
		t.Errorf("LockDir = %q, want %q", cfg.LockDir, DefaultLockDir)
	}
	if cfg.PathStyle {
		t.Error("PathStyle = true, want false without ADDRESSING_STYLE or s3fs opts")
	}
	if want := "/etc/cube/.passwd-s3fs-volume-cube-volumes"; cfg.PasswdFile != want {
		t.Errorf("PasswdFile = %q, want %q", cfg.PasswdFile, want)
	}
	if len(cfg.S3FSExtraOpts) != 0 {
		t.Errorf("S3FSExtraOpts = %v, want empty", cfg.S3FSExtraOpts)
	}
}

// The config file is shared with shell tooling that quotes multi-option values,
// so quotes must not leak into the s3fs argument list.
func TestLoadStripsQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want []string
	}{
		{"single", `S3FS_EXTRA_OPTS='-ouse_path_request_style'`, []string{"-ouse_path_request_style"}},
		{"double", `S3FS_EXTRA_OPTS="-ouse_path_request_style -odbglevel=info"`,
			[]string{"-ouse_path_request_style", "-odbglevel=info"}},
		{"bare", `S3FS_EXTRA_OPTS=-onomultipart`, []string{"-onomultipart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConf(t, minimalConf+tc.line+"\n"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.S3FSExtraOpts) != len(tc.want) {
				t.Fatalf("S3FSExtraOpts = %v, want %v", cfg.S3FSExtraOpts, tc.want)
			}
			for i := range tc.want {
				if cfg.S3FSExtraOpts[i] != tc.want[i] {
					t.Errorf("S3FSExtraOpts[%d] = %q, want %q", i, cfg.S3FSExtraOpts[i], tc.want[i])
				}
			}
		})
	}
}

// The control-plane client must address the endpoint the same way s3fs does,
// otherwise MinIO and dotted bucket names break on one side only.
func TestLoadPathStyle(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"explicit", minimalConf + "ADDRESSING_STYLE=path\n", true},
		{"from s3fs opts", minimalConf + "S3FS_EXTRA_OPTS='-ouse_path_request_style'\n", true},
		{"virtual host", minimalConf + "ADDRESSING_STYLE=virtual\n", false},
		{"unset", minimalConf, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConf(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PathStyle != tc.want {
				t.Errorf("PathStyle = %v, want %v", cfg.PathStyle, tc.want)
			}
		})
	}
}

func TestLoadRegionOverride(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf+"REGION=ap-guangzhou\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != "ap-guangzhou" {
		t.Errorf("Region = %q, want ap-guangzhou", cfg.Region)
	}
}

// An empty REGION must not override the default, since the shell config often
// carries a commented-out or blank assignment.
func TestLoadEmptyRegionKeepsDefault(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf+"REGION=\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != DefaultRegion {
		t.Errorf("Region = %q, want %q", cfg.Region, DefaultRegion)
	}
}

func TestLoadIgnoresCommentsAndExport(t *testing.T) {
	body := `
# ACCESS_KEY_ID=ignored
export ACCESS_KEY_ID=ak
SECRET_ACCESS_KEY=sk
BUCKET=b
ENDPOINT=http://minio:9000

# trailing comment
`
	cfg, err := Load(writeConf(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AccessKeyID != "ak" {
		t.Errorf("AccessKeyID = %q, want ak", cfg.AccessKeyID)
	}
}

// An inline trailing `# comment` after a value must be dropped, matching the
// shell `source` semantics the old plugin relied on. Without this,
// `S3FS_EXTRA_OPTS=-ouse_path_request_style  # MinIO` would pass `#` and
// `MinIO` as spurious s3fs arguments.
func TestLoadStripsInlineComment(t *testing.T) {
	cfg, err := Load(writeConf(t, minimalConf+"S3FS_EXTRA_OPTS=-ouse_path_request_style  # MinIO\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"-ouse_path_request_style"}
	if len(cfg.S3FSExtraOpts) != len(want) {
		t.Fatalf("S3FSExtraOpts = %v, want %v", cfg.S3FSExtraOpts, want)
	}
	if cfg.S3FSExtraOpts[0] != want[0] {
		t.Errorf("S3FSExtraOpts[0] = %q, want %q", cfg.S3FSExtraOpts[0], want[0])
	}
}

func TestLoadMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no access key", "SECRET_ACCESS_KEY=sk\nBUCKET=b\nENDPOINT=http://e\n"},
		{"no secret", "ACCESS_KEY_ID=ak\nBUCKET=b\nENDPOINT=http://e\n"},
		{"no bucket", "ACCESS_KEY_ID=ak\nSECRET_ACCESS_KEY=sk\nENDPOINT=http://e\n"},
		{"no endpoint", "ACCESS_KEY_ID=ak\nSECRET_ACCESS_KEY=sk\nBUCKET=b\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConf(t, tc.body)); err == nil {
				t.Fatal("Load succeeded, want error")
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.conf")); err == nil {
		t.Fatal("Load succeeded on missing file, want error")
	}
}

func TestLoadHonoursEnvOverrides(t *testing.T) {
	path := writeConf(t, minimalConf)
	t.Setenv("CUBE_S3_CONFIG", path)
	t.Setenv("CUBE_S3_LOCK_DIR", "/tmp/locks")
	t.Setenv("CUBE_S3_PASSWD_FILE", "/tmp/passwd")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LockDir != "/tmp/locks" {
		t.Errorf("LockDir = %q, want /tmp/locks", cfg.LockDir)
	}
	if cfg.PasswdFile != "/tmp/passwd" {
		t.Errorf("PasswdFile = %q, want /tmp/passwd", cfg.PasswdFile)
	}
}

// s3fs 1.91+ stats the trailing-slash directory object on mount, so the prefix
// create writes and the subdir s3fs mounts must not drift apart.
func TestVolumeKeys(t *testing.T) {
	if got, want := VolumePrefix("vol-1"), "volumes/vol-1/"; got != want {
		t.Errorf("VolumePrefix = %q, want %q", got, want)
	}
	if got, want := VolumeSubdir("vol-1"), "volumes/vol-1"; got != want {
		t.Errorf("VolumeSubdir = %q, want %q", got, want)
	}
}

func TestMountPointUnder(t *testing.T) {
	if got, want := MountPointUnder("/data/cube-shared/volume", "v1"), "/data/cube-shared/volume/s3-v1"; got != want {
		t.Errorf("MountPointUnder = %q, want %q", got, want)
	}
	// Detach records from an older Cubelet carry no base dir.
	if got, want := MountPointUnder("", "v1"), DefaultVolumeBaseDir+"/s3-v1"; got != want {
		t.Errorf("MountPointUnder with empty base = %q, want %q", got, want)
	}
}
