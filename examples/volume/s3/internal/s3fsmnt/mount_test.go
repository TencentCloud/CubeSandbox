// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3fsmnt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	return New(&config.Config{
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
		Bucket:          "cube-volumes",
		Endpoint:        "http://minio:9000",
		Region:          "us-east-1",
		PasswdFile:      filepath.Join(t.TempDir(), "passwd"),
		S3FSExtraOpts:   []string{"-ouse_path_request_style"},
	})
}

func TestMountArgs(t *testing.T) {
	m := testManager(t)
	args := m.MountArgs("/data/cube-shared/volume/s3-v1", "v1")

	if args[0] != "cube-volumes:/volumes/v1" {
		t.Errorf("args[0] = %q, want cube-volumes:/volumes/v1", args[0])
	}
	if args[1] != "/data/cube-shared/volume/s3-v1" {
		t.Errorf("args[1] = %q, want the mountpoint", args[1])
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-ourl=http://minio:9000",
		"-oendpoint=us-east-1",
		"-opasswd_file=" + m.cfg.PasswdFile,
		"-oallow_other",
		"-ouse_path_request_style",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}

	// compat_dir is unknown to s3fs 1.90; create writes the volumes/<id>/
	// directory object instead, so the option must never be hardcoded.
	if strings.Contains(joined, "compat_dir") {
		t.Errorf("args must not hardcode compat_dir: %v", args)
	}

	// Extra options come from operator config and must stay last so they can
	// override the defaults above.
	if args[len(args)-1] != "-ouse_path_request_style" {
		t.Errorf("extra opts must be appended last, got %v", args)
	}
}

func TestMountArgsInstanceRole(t *testing.T) {
	m := testManager(t)
	m.cfg.Credentials = config.CredentialsInstanceRole
	m.cfg.AccessKeyID, m.cfg.SecretAccessKey = "", ""
	args := m.MountArgs("/data/cube-shared/volume/s3-v1", "v1")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-oiam_role=auto") {
		t.Errorf("args missing -oiam_role=auto: %v", args)
	}
	if strings.Contains(joined, "passwd_file") {
		t.Errorf("args must not reference a passwd file without static keys: %v", args)
	}
	if args[len(args)-1] != "-ouse_path_request_style" {
		t.Errorf("extra opts must be appended last, got %v", args)
	}
}

func TestEnsurePasswdFileSkippedForInstanceRole(t *testing.T) {
	m := testManager(t)
	m.cfg.Credentials = config.CredentialsInstanceRole
	m.cfg.AccessKeyID, m.cfg.SecretAccessKey = "", ""

	if err := m.EnsurePasswdFile(); err != nil {
		t.Fatalf("EnsurePasswdFile: %v", err)
	}
	if _, err := os.Stat(m.cfg.PasswdFile); !os.IsNotExist(err) {
		t.Errorf("passwd file written without static keys: %v", err)
	}
}

func TestEnsurePasswdFile(t *testing.T) {
	m := testManager(t)

	if err := m.EnsurePasswdFile(); err != nil {
		t.Fatalf("EnsurePasswdFile: %v", err)
	}
	body, err := os.ReadFile(m.cfg.PasswdFile)
	if err != nil {
		t.Fatalf("read passwd: %v", err)
	}
	if got, want := string(body), "cube-volumes:ak:sk\n"; got != want {
		t.Errorf("passwd = %q, want %q", got, want)
	}

	info, err := os.Stat(m.cfg.PasswdFile)
	if err != nil {
		t.Fatalf("stat passwd: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("passwd mode = %v, want 0600", perm)
	}

	// A second call must be a no-op rather than a rewrite.
	if err := m.EnsurePasswdFile(); err != nil {
		t.Fatalf("EnsurePasswdFile (repeat): %v", err)
	}
	again, err := os.Stat(m.cfg.PasswdFile)
	if err != nil {
		t.Fatalf("stat passwd (repeat): %v", err)
	}
	if !again.ModTime().Equal(info.ModTime()) {
		t.Error("passwd file was rewritten even though credentials did not change")
	}
}

func TestEnsurePasswdFileRewritesOnCredentialChange(t *testing.T) {
	m := testManager(t)
	if err := m.EnsurePasswdFile(); err != nil {
		t.Fatalf("EnsurePasswdFile: %v", err)
	}

	m.cfg.SecretAccessKey = "rotated"
	if err := m.EnsurePasswdFile(); err != nil {
		t.Fatalf("EnsurePasswdFile after rotation: %v", err)
	}
	body, err := os.ReadFile(m.cfg.PasswdFile)
	if err != nil {
		t.Fatalf("read passwd: %v", err)
	}
	if got, want := string(body), "cube-volumes:ak:rotated\n"; got != want {
		t.Errorf("passwd = %q, want %q", got, want)
	}
}

func TestIsMountPointTreatsAnyNonZeroAsNotMounted(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "mountpoint")
	script := "#!/bin/sh\nexit ${MOUNTPOINT_EXIT:-1}\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, code := range []int{1, 32} {
		t.Setenv("MOUNTPOINT_EXIT", fmt.Sprintf("%d", code))
		mounted, err := IsMountPoint(dir)
		if err != nil {
			t.Fatalf("exit %d: IsMountPoint: %v", code, err)
		}
		if mounted {
			t.Errorf("exit %d: mounted=true, want false", code)
		}
	}
}

func requireMountpointCmd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mountpoint"); err != nil {
		t.Skip("mountpoint(1) not available")
	}
}

func TestIsMountPointMissingPath(t *testing.T) {
	requireMountpointCmd(t)

	mounted, err := IsMountPoint(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("IsMountPoint: %v", err)
	}
	if mounted {
		t.Error("IsMountPoint on missing path = true, want false")
	}
}

func TestIsMountPointPlainDir(t *testing.T) {
	requireMountpointCmd(t)

	// A regular temp directory is not a mount. Older mountpoint(1) exits 1
	// here and newer util-linux exits 32; both must report mounted=false.
	mounted, err := IsMountPoint(t.TempDir())
	if err != nil {
		t.Fatalf("IsMountPoint: %v", err)
	}
	if mounted {
		t.Error("IsMountPoint on plain dir = true, want false")
	}
}

// Detach must clear the mountpoint directory that attach created, and must not
// fail when the volume was never mounted on this node.
func TestUnmountRemovesUnmountedDir(t *testing.T) {
	requireMountpointCmd(t)

	m := testManager(t)
	mnt := filepath.Join(t.TempDir(), "s3-v1")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := m.Unmount(mnt); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if _, err := os.Stat(mnt); !os.IsNotExist(err) {
		t.Errorf("mount dir still present after Unmount: %v", err)
	}
}

func TestUnmountAbsentDir(t *testing.T) {
	requireMountpointCmd(t)

	m := testManager(t)
	if err := m.Unmount(filepath.Join(t.TempDir(), "never-there")); err != nil {
		t.Fatalf("Unmount on absent dir: %v", err)
	}
}

// A leftover non-empty directory is not fatal: the bucket data is intact and the
// next attach reuses the path.
func TestUnmountKeepsNonEmptyDir(t *testing.T) {
	requireMountpointCmd(t)

	m := testManager(t)
	mnt := filepath.Join(t.TempDir(), "s3-v1")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "stale"), nil, 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	if err := m.Unmount(mnt); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if _, err := os.Stat(mnt); err != nil {
		t.Errorf("non-empty mount dir should be left in place: %v", err)
	}
}

func TestMountPoint(t *testing.T) {
	m := testManager(t)
	if got, want := m.MountPoint("/base", "v1"), "/base/s3-v1"; got != want {
		t.Errorf("MountPoint = %q, want %q", got, want)
	}
}

// A node that used to run with static keys keeps the s3fs credential file
// until something removes it; in instance-role mode nothing reads it again,
// so leaving it would be leaving a plaintext secret on every such host.
func TestEnsurePasswdFileRemovesStaleFileForInstanceRole(t *testing.T) {
	passwd := filepath.Join(t.TempDir(), ".passwd-s3fs-volume-bucket")
	if err := os.WriteFile(passwd, []byte("bucket:AK:SK\n"), 0o600); err != nil {
		t.Fatalf("seed passwd file: %v", err)
	}

	m := &Manager{cfg: &config.Config{
		Credentials: config.CredentialsInstanceRole,
		Bucket:      "bucket",
		PasswdFile:  passwd,
	}}
	m.RemoveStalePasswdFile()
	if _, err := os.Stat(passwd); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale passwd file still there: %v", err)
	}

	// Already absent is fine too.
	m.RemoveStalePasswdFile()
}

// With static keys the file is the live credential and must be left alone.
func TestRemoveStalePasswdFileKeepsStaticKeys(t *testing.T) {
	m := testManager(t)
	if err := m.EnsurePasswdFile(); err != nil {
		t.Fatalf("EnsurePasswdFile: %v", err)
	}
	m.RemoveStalePasswdFile()
	if _, err := os.Stat(m.cfg.PasswdFile); err != nil {
		t.Fatalf("static-key passwd file removed: %v", err)
	}
}

// The instance-role check must only gate a real mount: a repeat attach of a
// volume that is already mounted must not depend on IMDS. Stale-secret cleanup,
// on the other hand, runs either way.
func TestMountRunsBeforeMountOnlyWhenMounting(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"mountpoint": "#!/bin/sh\nexit ${MOUNTPOINT_EXIT:-1}\n",
		"s3fs":       "#!/bin/sh\necho s3fs must not run >&2\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	passwd := filepath.Join(t.TempDir(), "passwd")
	m := New(&config.Config{
		Credentials: config.CredentialsInstanceRole,
		Bucket:      "b",
		PasswdFile:  passwd,
	})
	calls := 0
	probeErr := errors.New("no role")
	m.BeforeMount = func() error { calls++; return probeErr }

	t.Run("already mounted", func(t *testing.T) {
		if err := os.WriteFile(passwd, []byte("b:AK:SK\n"), 0o600); err != nil {
			t.Fatalf("seed passwd: %v", err)
		}
		t.Setenv("MOUNTPOINT_EXIT", "0")
		if _, err := m.Mount(t.TempDir(), "v1"); err != nil {
			t.Fatalf("Mount: %v", err)
		}
		if calls != 0 {
			t.Errorf("BeforeMount called %d times for a mounted volume, want 0", calls)
		}
		if _, err := os.Stat(passwd); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stale passwd file kept on a mounted node: %v", err)
		}
	})

	t.Run("not mounted", func(t *testing.T) {
		t.Setenv("MOUNTPOINT_EXIT", "1")
		if _, err := m.Mount(t.TempDir(), "v1"); !errors.Is(err, probeErr) {
			t.Fatalf("Mount error = %v, want the BeforeMount error", err)
		}
		if calls != 1 {
			t.Errorf("BeforeMount called %d times, want 1", calls)
		}
	})
}
