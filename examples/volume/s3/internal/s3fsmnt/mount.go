// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package s3fsmnt manages per-volume s3fs FUSE mounts (Node hook data plane).
package s3fsmnt

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

// Spec is one volume's mount binding: where the data lives and which
// credentials address it. Legacy volumes derive it from volume-s3.conf;
// dynamic volumes derive it from the persisted private_data state.
type Spec struct {
	Bucket   string
	Subdir   string // object key under the bucket, without a trailing slash
	Endpoint string
	Region   string
	Dynamic  bool

	// Addressing is the bucket addressing mode. s3fs defaults to
	// virtual-hosted style, so path-style adds -ouse_path_request_style and
	// explicit virtual removes any inherited path-style flag.
	Addressing config.Addressing

	// PasswdFile holds bucket:access-key:secret for static credentials.
	PasswdFile string

	// AccessKeyID and SecretAccessKey are written into PasswdFile. They are
	// never placed in the s3fs argument list.
	AccessKeyID     string
	SecretAccessKey string

	// InstanceRole uses -oiam_role=auto instead of a passwd file.
	InstanceRole bool

	// ExtraOpts come from resolved configuration and are appended last.
	ExtraOpts []string
}

// Manager mounts and unmounts one s3fs process per volume.
type Manager struct {
	cfg           *config.Config
	commandRunner func(string, ...string) ([]byte, error)
	mountChecker  func(string) (bool, error)

	// BeforeMount, when set, runs only when a mount is really about to happen
	// (not for a volume that is already mounted); an error aborts the attach.
	BeforeMount func() error
}

// New creates a Manager.
func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg}
}

// MountPoint returns the per-volume FUSE mount path under baseDir.
func (m *Manager) MountPoint(baseDir, volumeID string) string {
	return config.MountPointUnder(baseDir, volumeID)
}

// defaultSpec is the legacy binding from volume-s3.conf.
func (m *Manager) defaultSpec(volumeID string) Spec {
	return Spec{
		Bucket:          m.cfg.Bucket,
		Subdir:          config.VolumeSubdir(volumeID),
		Endpoint:        m.cfg.Endpoint,
		Region:          m.cfg.Region,
		Addressing:      m.cfg.Addressing,
		PasswdFile:      m.cfg.PasswdFile,
		AccessKeyID:     m.cfg.AccessKeyID,
		SecretAccessKey: m.cfg.SecretAccessKey,
		InstanceRole:    m.cfg.UseInstanceRole(),
		ExtraOpts:       m.cfg.S3FSExtraOpts,
	}
}

// EnsurePasswdFile writes the legacy s3fs credential file
// (Bucket:AccessKeyId:SecretAccessKey).
//
// The path is per-bucket so several plugin instances (different driver names,
// different buckets) on one node never race on a shared credential file. It is
// rewritten only when the credentials changed.
//
// In instance-role mode no file is written; see RemoveStalePasswdFile.
func (m *Manager) EnsurePasswdFile() error {
	return ensurePasswdFile(m.defaultSpec(""))
}

func (m *Manager) run(name string, args ...string) ([]byte, error) {
	if m.commandRunner != nil {
		return m.commandRunner(name, args...)
	}
	return exec.Command(name, args...).CombinedOutput()
}

func (m *Manager) isMounted(path string) (bool, error) {
	if m.mountChecker != nil {
		return m.mountChecker(path)
	}
	return IsMountPoint(path)
}

// RemoveVolumePasswdFile deletes only the path derived for this dynamic volume.
// Metadata is a validation hint only; the path used for deletion always comes
// from configuration so symlinks in an untrusted hint cannot redirect it.
// Missing files count as already cleaned up.
func (m *Manager) RemoveVolumePasswdFile(volumeID, metadataPath string) error {
	if err := binding.ValidateVolumeID(volumeID); err != nil {
		return err
	}
	path := m.cfg.PasswdFileForVolume(volumeID)
	if metadataPath != "" {
		if !m.cfg.IsDynamicPasswdFile(volumeID, metadataPath) {
			return fmt.Errorf("metadata passwd_file is not the dynamic credential path for volume %q", volumeID)
		}
	}
	if !m.cfg.IsDynamicPasswdFile(volumeID, path) {
		return fmt.Errorf("refusing to remove an invalid dynamic passwd path for volume %q", volumeID)
	}
	if err := removeFileIfExists(path); err != nil {
		return fmt.Errorf("remove passwd file %q: %w", path, err)
	}
	return nil
}

func (m *Manager) removeFailedDynamicPasswd(volumeID string, spec Spec) {
	if !spec.Dynamic || !m.cfg.IsDynamicPasswdFile(volumeID, spec.PasswdFile) {
		return
	}
	if err := removeFileIfExists(spec.PasswdFile); err != nil {
		log.Printf("warning: remove dynamic passwd file %q after failed mount: %v", spec.PasswdFile, err)
	}
}

func ensurePasswdFile(spec Spec) error {
	if spec.InstanceRole {
		return nil
	}
	if spec.Bucket == "" || spec.PasswdFile == "" {
		return errors.New("s3fs: bucket and passwd file are required for static credentials")
	}
	if spec.AccessKeyID == "" || spec.SecretAccessKey == "" {
		return errors.New("s3fs: access key and secret are required for static credentials")
	}
	content := fmt.Sprintf("%s:%s:%s\n", spec.Bucket, spec.AccessKeyID, spec.SecretAccessKey)
	if !spec.Dynamic {
		// Preserve the legacy shared-file behavior, including operator-managed
		// symlinks and permissions. Dynamic files are private to one volume and
		// use the stricter handling below.
		if b, err := os.ReadFile(spec.PasswdFile); err == nil && string(b) == content {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(spec.PasswdFile), 0o755); err != nil {
			return fmt.Errorf("create passwd dir: %w", err)
		}
		if err := os.WriteFile(spec.PasswdFile, []byte(content), 0o600); err != nil {
			return fmt.Errorf("write passwd file %q: %w", spec.PasswdFile, err)
		}
		return nil
	}
	if info, err := os.Lstat(spec.PasswdFile); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("passwd path %q is not a regular file", spec.PasswdFile)
		}
		if b, readErr := os.ReadFile(spec.PasswdFile); readErr == nil && string(b) == content {
			if info.Mode().Perm() != 0o600 {
				if err := os.Chmod(spec.PasswdFile, 0o600); err != nil {
					return fmt.Errorf("secure passwd file %q: %w", spec.PasswdFile, err)
				}
			}
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect passwd file %q: %w", spec.PasswdFile, err)
	}
	if err := os.MkdirAll(filepath.Dir(spec.PasswdFile), 0o755); err != nil {
		return fmt.Errorf("create passwd dir: %w", err)
	}
	if err := os.WriteFile(spec.PasswdFile, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write passwd file %q: %w", spec.PasswdFile, err)
	}
	if err := os.Chmod(spec.PasswdFile, 0o600); err != nil {
		return fmt.Errorf("secure passwd file %q: %w", spec.PasswdFile, err)
	}
	return nil
}

func removeFileIfExists(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("refusing to remove directory %q as a passwd file", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveStalePasswdFile deletes, in instance-role mode, a credential file left
// from when the node ran with static keys: a long-lived plaintext secret that
// nothing reads any more, which is what this mode is for. Nothing depends on
// it being gone, so a failure is logged rather than failing the attach.
func (m *Manager) RemoveStalePasswdFile() {
	if !m.cfg.UseInstanceRole() {
		return
	}
	if err := os.Remove(m.cfg.PasswdFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("warning: remove stale passwd file %q: %v", m.cfg.PasswdFile, err)
	}
}

// MountArgs builds the s3fs argument list for one legacy volume.
func (m *Manager) MountArgs(mnt, volumeID string) []string {
	return MountArgsFor(mnt, m.defaultSpec(volumeID))
}

// MountArgsFor builds the s3fs argument list for one binding.
//
//	-o url          the S3-compatible endpoint
//	-o endpoint     region used for SigV4 signing
//	-o passwd_file  per-bucket or per-volume credential file, or
//	                -o iam_role=auto with CREDENTIALS=instance_role
//	-o allow_other  Cubelet (a different user) must traverse the mount to bind
//	                it into the microVM via virtiofs
//
// Create already PUT the prefix directory object, the same object s3fs mkdir
// would write, so no compat_dir option is needed.
func MountArgsFor(mnt string, spec Spec) []string {
	args := []string{
		fmt.Sprintf("%s:/%s", spec.Bucket, strings.TrimSuffix(spec.Subdir, "/")),
		mnt,
		"-ourl=" + spec.Endpoint,
		"-oendpoint=" + spec.Region,
		"-oallow_other",
	}
	if spec.InstanceRole {
		args = append(args, "-oiam_role=auto")
	} else {
		args = append(args, "-opasswd_file="+spec.PasswdFile)
	}
	if spec.Addressing == config.AddressingPath && !config.HasPathRequestStyle(spec.ExtraOpts) {
		args = append(args, "-ouse_path_request_style")
	}
	return append(args, spec.ExtraOpts...)
}

// Mount idempotently mounts the configured bucket under baseDir and returns the
// mountpoint. Callers must hold the per-volume lock.
func (m *Manager) Mount(baseDir, volumeID string) (string, error) {
	return m.MountWithSpec(baseDir, volumeID, m.defaultSpec(volumeID))
}

// MountWithSpec idempotently mounts one binding under baseDir and returns the
// mountpoint. Callers must hold the per-volume lock.
func (m *Manager) MountWithSpec(baseDir, volumeID string, spec Spec) (string, error) {
	if spec.Bucket == "" || spec.Subdir == "" {
		return "", errors.New("s3fs: bucket and subdir are required")
	}
	if spec.Endpoint == "" {
		return "", errors.New("s3fs: endpoint is required")
	}
	if !spec.InstanceRole && spec.PasswdFile == "" {
		return "", errors.New("s3fs: passwd file is required for static credentials")
	}

	mnt := m.MountPoint(baseDir, volumeID)

	// Before the early return, so nodes whose volumes are all mounted already
	// are cleaned up too.
	m.RemoveStalePasswdFile()

	mounted, err := m.isMounted(mnt)
	if err != nil {
		return "", err
	}
	if mounted {
		return mnt, nil
	}

	if m.BeforeMount != nil {
		if err := m.BeforeMount(); err != nil {
			return "", err
		}
	}

	if err := ensurePasswdFile(spec); err != nil {
		m.removeFailedDynamicPasswd(volumeID, spec)
		return "", err
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		m.removeFailedDynamicPasswd(volumeID, spec)
		return "", fmt.Errorf("create mount dir %q: %w", mnt, err)
	}

	out, runErr := m.run("s3fs", MountArgsFor(mnt, spec)...)

	// s3fs can exit 0 even when authentication fails; require a real mountpoint.
	// Conversely, if s3fs returned an error but the mount is active, preserve it
	// and its credential file instead of damaging a mount still in use.
	mounted, mntErr := m.isMounted(mnt)
	if mntErr != nil {
		return "", mntErr
	}
	if mounted {
		return mnt, nil
	}
	_ = os.Remove(mnt)
	m.removeFailedDynamicPasswd(volumeID, spec)
	if runErr != nil {
		return "", fmt.Errorf("s3fs mount %q: %w: %s", mnt, runErr, out)
	}
	return "", fmt.Errorf("s3fs mount %q: process exited 0 but path is not a mountpoint: %s", mnt, out)
}

// Unmount removes the per-volume FUSE mount at mnt when it is still active and
// deletes the mountpoint directory created at attach.
func (m *Manager) Unmount(mnt string) error {
	mounted, err := m.isMounted(mnt)
	if err != nil {
		return err
	}
	if mounted {
		unmounted := false
		for _, args := range [][]string{
			{"fusermount", "-u", mnt},
			{"umount", "-l", mnt},
		} {
			if _, err := m.run(args[0], args[1:]...); err == nil {
				unmounted = true
				break
			}
		}
		if !unmounted {
			return fmt.Errorf("unmount %q failed", mnt)
		}
	}
	// A non-empty leftover directory is not fatal: the bucket data is intact and
	// the next attach reuses the path.
	_ = os.Remove(mnt)
	return nil
}

// IsMountPoint reports whether path is an active mountpoint, using mountpoint(1)
// so FUSE mounts are detected the same way the shell implementation saw them.
func IsMountPoint(path string) (bool, error) {
	err := exec.Command("mountpoint", "-q", path).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// mountpoint(1) missing from PATH, or the process could not be started.
		return false, fmt.Errorf("mountpoint %q: %w", path, err)
	}
	// util-linux documents 32 as "not a mountpoint". Ubuntu 20.04's mountpoint
	// (util-linux 2.34, the one-click builder image) still uses 1 for the same
	// case, matching `if mountpoint -q "$mnt"` in the old bash plugin. Any
	// non-zero exit therefore means "not mounted".
	return false, nil
}
