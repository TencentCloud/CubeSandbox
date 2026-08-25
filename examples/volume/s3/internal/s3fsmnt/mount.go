// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package s3fsmnt manages per-volume s3fs FUSE mounts (Node hook data plane).
package s3fsmnt

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

// Manager mounts and unmounts one s3fs process per volume.
type Manager struct {
	cfg *config.Config
}

// New creates a Manager.
func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg}
}

// MountPoint returns the per-volume FUSE mount path under baseDir.
func (m *Manager) MountPoint(baseDir, volumeID string) string {
	return config.MountPointUnder(baseDir, volumeID)
}

// EnsurePasswdFile writes the s3fs credential file (Bucket:AccessKeyId:SecretAccessKey).
//
// The path is per-bucket so several plugin instances (different driver names,
// different buckets) on one node never race on a shared credential file. It is
// rewritten only when the credentials changed.
func (m *Manager) EnsurePasswdFile() error {
	content := fmt.Sprintf("%s:%s:%s\n", m.cfg.Bucket, m.cfg.AccessKeyID, m.cfg.SecretAccessKey)
	if b, err := os.ReadFile(m.cfg.PasswdFile); err == nil && string(b) == content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.cfg.PasswdFile), 0o755); err != nil {
		return fmt.Errorf("create passwd dir: %w", err)
	}
	if err := os.WriteFile(m.cfg.PasswdFile, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write passwd file %q: %w", m.cfg.PasswdFile, err)
	}
	return nil
}

// MountArgs builds the s3fs argument list for one volume.
//
//	-o url          the S3-compatible endpoint from volume-s3.conf
//	-o endpoint     region used for SigV4 signing
//	-o passwd_file  per-bucket credential file
//	-o allow_other  Cubelet (a different user) must traverse the mount to bind
//	                it into the microVM via virtiofs
//	-o nonempty     the mountpoint may already hold a stale dir entry
//
// Create already PUT volumes/<id>/, the same object s3fs mkdir would write, so
// no compat_dir option is needed.
func (m *Manager) MountArgs(mnt, volumeID string) []string {
	args := []string{
		fmt.Sprintf("%s:/%s", m.cfg.Bucket, config.VolumeSubdir(volumeID)),
		mnt,
		"-ourl=" + m.cfg.Endpoint,
		"-oendpoint=" + m.cfg.Region,
		"-opasswd_file=" + m.cfg.PasswdFile,
		"-oallow_other",
		"-ononempty",
	}
	return append(args, m.cfg.S3FSExtraOpts...)
}

// Mount idempotently mounts BUCKET:/volumes/<volumeID> under baseDir and
// returns the mountpoint. Callers must hold the per-volume lock.
func (m *Manager) Mount(baseDir, volumeID string) (string, error) {
	mnt := m.MountPoint(baseDir, volumeID)

	mounted, err := IsMountPoint(mnt)
	if err != nil {
		return "", err
	}
	if mounted {
		return mnt, nil
	}

	if err := m.EnsurePasswdFile(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return "", fmt.Errorf("create mount dir %q: %w", mnt, err)
	}

	out, runErr := exec.Command("s3fs", m.MountArgs(mnt, volumeID)...).CombinedOutput()

	// s3fs can exit 0 even when authentication fails; require a real mountpoint.
	mounted, mntErr := IsMountPoint(mnt)
	if runErr != nil || mntErr != nil || !mounted {
		_ = os.Remove(mnt)
		switch {
		case runErr != nil:
			return "", fmt.Errorf("s3fs mount %q: %w: %s", mnt, runErr, out)
		case mntErr != nil:
			return "", mntErr
		default:
			return "", fmt.Errorf("s3fs mount %q: process exited 0 but path is not a mountpoint: %s", mnt, out)
		}
	}
	return mnt, nil
}

// Unmount removes the per-volume FUSE mount at mnt when it is still active and
// deletes the mountpoint directory created at attach.
func (m *Manager) Unmount(mnt string) error {
	mounted, err := IsMountPoint(mnt)
	if err != nil {
		return err
	}
	if mounted {
		unmounted := false
		for _, args := range [][]string{
			{"fusermount", "-u", mnt},
			{"umount", "-l", mnt},
		} {
			if err := exec.Command(args[0], args[1:]...).Run(); err == nil {
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
