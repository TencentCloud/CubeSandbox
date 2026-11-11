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
		Addressing:      config.AddressingPath,
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

// A dynamic volume mounts its own bucket/prefix with the credentials its
// reference resolved; nothing may fall back to the deployment defaults.
func TestMountArgsForDynamicSpec(t *testing.T) {
	spec := Spec{
		Bucket:          "user-bucket",
		Subdir:          "workspaces/user-a",
		Endpoint:        "https://cos.ap-guangzhou.myqcloud.com",
		Region:          "ap-guangzhou",
		Addressing:      config.AddressingPath,
		PasswdFile:      "/etc/cube/.passwd-s3fs-dynamic-vol-1",
		AccessKeyID:     "AKA",
		SecretAccessKey: "SKA",
	}
	args := MountArgsFor("/data/cube-shared/volume/s3-vol-1", spec)

	if args[0] != "user-bucket:/workspaces/user-a" {
		t.Errorf("args[0] = %q, want user-bucket:/workspaces/user-a", args[0])
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-ourl=https://cos.ap-guangzhou.myqcloud.com",
		"-oendpoint=ap-guangzhou",
		"-opasswd_file=/etc/cube/.passwd-s3fs-dynamic-vol-1",
		"-oallow_other",
		"-ouse_path_request_style",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "AKA") || strings.Contains(joined, "SKA") {
		t.Errorf("credentials leaked into argv: %v", args)
	}
}

// If the operator already set path-style in the extra options, the plugin must
// not pass the flag twice.
func TestMountArgsForDynamicSpecDoesNotDuplicatePathStyle(t *testing.T) {
	spec := Spec{
		Bucket:     "user-bucket",
		Subdir:     "workspaces/user-a/",
		Endpoint:   "http://minio:9000",
		PasswdFile: "/tmp/passwd",
		Addressing: config.AddressingPath,
		ExtraOpts:  []string{"-ouse_path_request_style"},
	}
	args := MountArgsFor("/mnt/v", spec)
	count := 0
	for _, a := range args {
		if strings.Contains(a, "path_request_style") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("path-style option appears %d times, want 1: %v", count, args)
	}
}

// Explicit virtual addressing must remove an inherited path-style flag before
// s3fs sees it, even when the Spec was built without ResolveCredentials.

func TestResolvedVirtualReferenceMountsWithVirtualAddressing(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, config.ConfigFileName)
	body := "ACCESS_KEY_ID=ak\nSECRET_ACCESS_KEY=sk\nBUCKET=legacy\nENDPOINT=https://s3.example.com\n" +
		"S3FS_EXTRA_OPTS='-ouse_path_request_style -odbglevel=info'\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(conf)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.CredentialsDir = t.TempDir()
	refBody := "ACCESS_KEY_ID=AKV\nSECRET_ACCESS_KEY=SKV\nENDPOINT=https://virtual.example.com\n" +
		"ADDRESSING_STYLE=virtual\n"
	if err := os.WriteFile(filepath.Join(cfg.CredentialsDir, "team-v"+config.CredentialFileSuffix), []byte(refBody), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}

	creds, err := cfg.ResolveCredentials("team-v")
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if creds.Addressing != config.AddressingVirtual {
		t.Fatalf("control-plane addressing = %v, want virtual", creds.Addressing)
	}

	spec := Spec{
		Bucket:     "user-bucket",
		Subdir:     "workspaces/user-a",
		Endpoint:   creds.Endpoint,
		Region:     creds.Region,
		Addressing: creds.Addressing,
		PasswdFile: "/etc/cube/.passwd-s3fs-dynamic-vol-1",
		ExtraOpts:  creds.ExtraOpts,
	}
	args := MountArgsFor("/mnt/v", spec)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "use_path_request_style") {
		t.Errorf("s3fs still forces path-style: %v", args)
	}
	for _, want := range []string{"-odbglevel=info"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing unrelated option %q: %v", want, args)
		}
	}
}

func TestEnsurePasswdFileForDynamicSpec(t *testing.T) {
	body, err := os.ReadFile(writeDynamicPasswd(t, "user-bucket", "AKA", "SKA"))
	if err != nil {
		t.Fatalf("read passwd: %v", err)
	}
	if got, want := string(body), "user-bucket:AKA:SKA\n"; got != want {
		t.Errorf("passwd = %q, want %q", got, want)
	}
}

func dynamicMountFixture(t *testing.T) (*Manager, string, Spec) {
	t.Helper()
	m := testManager(t)
	m.cfg.PasswdDir = filepath.Join(t.TempDir(), "passwd")
	volumeID := "vol-1"
	spec := Spec{
		Bucket: "user-bucket", Subdir: "workspaces/user-a", Endpoint: "http://minio:9000",
		Region: "us-east-1", PasswdFile: m.cfg.PasswdFileForVolume(volumeID),
		AccessKeyID: "reference-ak", SecretAccessKey: "reference-sk", Dynamic: true,
	}
	return m, volumeID, spec
}

func TestMountWithSpecCleansDynamicPasswdOnFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mountDir   bool
		commandErr error
		wantError  string
	}{
		{name: "mount directory creation", wantError: "create mount dir"},
		{name: "s3fs error", mountDir: true, commandErr: errors.New("s3fs denied"), wantError: "s3fs denied"},
		{name: "exit zero without mount", mountDir: true, wantError: "not a mountpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, volumeID, spec := dynamicMountFixture(t)
			base := t.TempDir()
			if !tc.mountDir {
				base = filepath.Join(base, "not-a-directory")
				if err := os.WriteFile(base, []byte("block mkdir"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			m.mountChecker = func(string) (bool, error) { return false, nil }
			m.commandRunner = func(name string, _ ...string) ([]byte, error) {
				if name != "s3fs" {
					t.Fatalf("unexpected command %q", name)
				}
				return []byte("simulated mount output"), tc.commandErr
			}
			_, err := m.MountWithSpec(base, volumeID, spec)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("MountWithSpec error = %v, want it to contain %q", err, tc.wantError)
			}
			if _, statErr := os.Stat(spec.PasswdFile); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("dynamic passwd file remains after confirmed mount failure: stat error=%v", statErr)
			}
		})
	}
}

func TestMountWithSpecPreservesPasswdWhenMountMayBeActive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondCall bool
		mountErr   error
		wantError  bool
	}{
		{name: "mount active despite command error", secondCall: true, mountErr: errors.New("late s3fs error")},
		{name: "mount state unknown", mountErr: errors.New("s3fs error"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, volumeID, spec := dynamicMountFixture(t)
			checks := 0
			m.mountChecker = func(string) (bool, error) {
				checks++
				if checks == 1 {
					return false, nil
				}
				if tc.secondCall {
					return true, nil
				}
				return false, errors.New("mount status unavailable")
			}
			m.commandRunner = func(string, ...string) ([]byte, error) { return []byte("s3fs output"), tc.mountErr }
			mnt, err := m.MountWithSpec(t.TempDir(), volumeID, spec)
			if tc.wantError && err == nil {
				t.Fatal("MountWithSpec succeeded with unknown mount state")
			}
			if !tc.wantError && (err != nil || mnt == "") {
				t.Fatalf("MountWithSpec = %q, %v; want the active mount preserved", mnt, err)
			}
			if _, statErr := os.Stat(spec.PasswdFile); statErr != nil {
				t.Errorf("passwd file removed while mount may be active: %v", statErr)
			}
		})
	}
}

func TestRemoveVolumePasswdFileIsBoundToVolume(t *testing.T) {
	m, volumeID, _ := dynamicMountFixture(t)
	path := m.cfg.PasswdFileForVolume(volumeID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bucket:ak:sk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveVolumePasswdFile(volumeID, ""); err != nil {
		t.Fatalf("fallback cleanup: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dynamic passwd file remains: %v", err)
	}
	if err := m.RemoveVolumePasswdFile(volumeID, ""); err != nil {
		t.Errorf("missing file cleanup = %v, want idempotent success", err)
	}

	other := m.cfg.PasswdFileForVolume("vol-2")
	external := filepath.Join(t.TempDir(), "external-passwd")
	for name, hint := range map[string]string{
		"other volume":       other,
		"legacy shared file": m.cfg.PasswdFile,
		"external path":      external,
		"prefix lookalike":   path + "-extra",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(hint, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := m.RemoveVolumePasswdFile(volumeID, hint); err == nil {
				t.Fatalf("accepted untrusted hint %q", hint)
			}
			if _, err := os.Stat(hint); err != nil {
				t.Errorf("untrusted path was removed: %v", err)
			}
		})
	}
}

func TestRemoveVolumePasswdFileRefusesDirectory(t *testing.T) {
	m, volumeID, _ := dynamicMountFixture(t)
	path := m.cfg.PasswdFileForVolume(volumeID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveVolumePasswdFile(volumeID, path); err == nil {
		t.Fatal("removed a directory as a passwd file")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("directory was altered: info=%v err=%v", info, err)
	}
}

func TestLegacySharedPasswdSymlinkRemainsSupported(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "operator-managed-passwd")
	link := filepath.Join(dir, "legacy-passwd")
	if err := os.WriteFile(target, []byte("old:ak:sk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	spec := Spec{Bucket: "legacy", PasswdFile: link, AccessKeyID: "new-ak", SecretAccessKey: "new-sk"}
	if err := ensurePasswdFile(spec); err != nil {
		t.Fatalf("legacy shared passwd symlink: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("legacy passwd path is no longer a symlink: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "legacy:new-ak:new-sk\n" {
		t.Fatalf("legacy symlink target = %q, %v", got, err)
	}
}

func TestDynamicPasswdCleanupRemovesSymlinkWithoutFollowingTarget(t *testing.T) {
	cfg := &config.Config{PasswdDir: t.TempDir()}
	manager := New(cfg)
	const volumeID = "vol-link"
	path := cfg.PasswdFileForVolume(volumeID)
	target := filepath.Join(t.TempDir(), "external-passwd")
	if err := os.WriteFile(target, []byte("external:ak:sk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := manager.RemoveVolumePasswdFile(volumeID, path); err != nil {
		t.Fatalf("RemoveVolumePasswdFile: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dynamic symlink remains: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "external:ak:sk\n" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
}

func TestDynamicPasswdCleanupDoesNotFollowMetadataSymlinkAndDotDot(t *testing.T) {
	root := t.TempDir()
	passwdDir := filepath.Join(root, "passwd")
	outsideDir := filepath.Join(root, "outside")
	redirectTarget := filepath.Join(outsideDir, "inner")
	if err := os.MkdirAll(passwdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(redirectTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{PasswdDir: passwdDir}
	const volumeID = "vol-redirect"
	expected := cfg.PasswdFileForVolume(volumeID)
	if err := os.Symlink(redirectTarget, filepath.Join(passwdDir, "redirect")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	metadataPath := filepath.Join(passwdDir, "redirect") + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(expected)
	if filepath.Clean(metadataPath) != filepath.Clean(expected) {
		t.Fatal("fixture must normalize to the expected path")
	}
	external := filepath.Join(outsideDir, filepath.Base(expected))
	if err := os.WriteFile(external, []byte("external secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expected, []byte("volume secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := New(cfg).RemoveVolumePasswdFile(volumeID, metadataPath); err != nil {
		t.Fatalf("RemoveVolumePasswdFile: %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("metadata path redirected deletion outside PASSWD_DIR: %v", err)
	}
	if _, err := os.Stat(expected); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fixed dynamic passwd file remains: %v", err)
	}
}

func writeDynamicPasswd(t *testing.T, bucket, ak, sk string) string {
	t.Helper()
	passwd := filepath.Join(t.TempDir(), ".passwd-s3fs-volume-vol-1")
	spec := Spec{Bucket: bucket, PasswdFile: passwd, AccessKeyID: ak, SecretAccessKey: sk}
	if err := ensurePasswdFile(spec); err != nil {
		t.Fatalf("ensurePasswdFile: %v", err)
	}
	return passwd
}

func TestMountWithSpecRejectsIncompleteBinding(t *testing.T) {
	m := testManager(t)
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{"no bucket", Spec{Subdir: "a", Endpoint: "http://e", PasswdFile: "/p"}, "bucket and subdir"},
		{"no subdir", Spec{Bucket: "b", Endpoint: "http://e", PasswdFile: "/p"}, "bucket and subdir"},
		{"no endpoint", Spec{Bucket: "b", Subdir: "a", PasswdFile: "/p"}, "endpoint"},
		{"no passwd file", Spec{Bucket: "b", Subdir: "a", Endpoint: "http://e"}, "passwd file"},
		{"no keys", Spec{Bucket: "b", Subdir: "a", Endpoint: "http://e", PasswdFile: "/p"}, "access key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.MountWithSpec(t.TempDir(), "v1", tc.spec)
			if err == nil {
				t.Fatal("MountWithSpec succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
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
		Endpoint:    "http://minio:9000",
		Region:      "us-east-1",
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
