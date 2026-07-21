// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package cosfs manages per-volume cosfs FUSE mounts (Node Hook data plane).
package cosfs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/config"
)

// Manager handles cosfs mount lifecycle inside Cubelet's mount namespace.
type Manager struct {
	cfg   *config.Config
	locks sync.Map // volumeID -> *sync.Mutex
}

// New creates a Manager.
func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg}
}

func (m *Manager) lock(volumeID string) func() {
	v, _ := m.locks.LoadOrStore(volumeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// EnsurePasswdFile writes the cosfs passwd file (Bucket:SecretId:SecretKey).
func (m *Manager) EnsurePasswdFile() error {
	content := fmt.Sprintf("%s:%s:%s", m.cfg.Bucket, m.cfg.SecretID, m.cfg.SecretKey)
	if b, err := os.ReadFile(m.cfg.PasswdFile); err == nil && string(b) == content+"\n" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.cfg.PasswdFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.cfg.PasswdFile, []byte(content+"\n"), 0o600); err != nil {
		return err
	}
	return nil
}

// MountPoint returns the per-volume FUSE mount path under baseDir (the parent
// directory Cubelet requires via AttachRequest.VolumeBaseDir).
func (m *Manager) MountPoint(baseDir, volumeID string) string {
	return config.MountPointUnder(baseDir, volumeID)
}

// Mount idempotently mounts BUCKET:/volumes/<volumeID> under baseDir.
func (m *Manager) Mount(baseDir, volumeID string) (string, error) {
	unlock := m.lock(volumeID)
	defer unlock()

	mnt := config.MountPointUnder(baseDir, volumeID)
	if mounted, err := isMountPoint(mnt); err != nil {
		return "", err
	} else if mounted {
		return mnt, nil
	}

	if err := m.EnsurePasswdFile(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return "", err
	}

	cosPath := fmt.Sprintf("%s:/%s", m.cfg.Bucket, config.CosFSSubdir(volumeID))
	endpoint := fmt.Sprintf("https://cos.%s.myqcloud.com", m.cfg.Region)

	cmd := exec.Command("cosfs", cosPath, mnt,
		fmt.Sprintf("-ourl=%s", endpoint),
		fmt.Sprintf("-opasswd_file=%s", m.cfg.PasswdFile),
		"-oallow_other",
		"-ononempty",
		"-odbglevel=info",
		"-onoxattr",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cosfs mount %s: %w: %s", mnt, err, string(out))
	}
	return mnt, nil
}

// Unmount removes the per-volume FUSE mount at mnt when it is still active.
// mnt is the mount_dir recorded in the attach metadata.
func (m *Manager) Unmount(volumeID, mnt string) error {
	unlock := m.lock(volumeID)
	defer unlock()

	mounted, err := isMountPoint(mnt)
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
			return fmt.Errorf("unmount %s failed", mnt)
		}
	}

	return removeMountDir(mnt)
}

// removeMountDir deletes the empty mountpoint directory created at attach.
func removeMountDir(mnt string) error {
	if err := os.Remove(mnt); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove mount dir %s: %w", mnt, err)
	}
	return nil
}

func isMountPoint(path string) (bool, error) {
	err := exec.Command("mountpoint", "-q", path).Run()
	if err == nil {
		return true, nil
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		return false, err
	}
	switch exitErr.ExitCode() {
	case 32:
		return false, nil
	case 1:
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return false, nil
		}
		return false, err
	default:
		return false, err
	}
}
