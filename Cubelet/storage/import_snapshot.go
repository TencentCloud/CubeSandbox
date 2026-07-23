// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

// ErrImportSourceInvalid wraps rejections of the staged source artifact so
// callers can map them to a parameter error instead of a storage failure.
var ErrImportSourceInvalid = errors.New("invalid import source")

// ImportRootfsArtifact registers a staged writable-layer artifact as the
// template rootfs object (tpl-<templateID>-rootfs) and returns it together
// with the overlay upper subdir detected inside the artifact: the restored
// guest's container id differs from the exporting sandbox's, so it must be
// pointed at the artifact's original upper path.
func ImportRootfsArtifact(ctx context.Context, templateID, srcPath string) (*CowSnapshotObject, string, error) {
	manager, err := requireCowManager()
	if err != nil {
		return nil, "", err
	}
	src, size, err := openImportArtifact(srcPath)
	if err != nil {
		return nil, "", err
	}
	defer src.Close()
	wlayerSubdir, err := detectWlayerSubdir(ctx, src)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrImportSourceInvalid, err)
	}
	// The vehicle reuses the template build-rootfs name so an orphan left by a
	// crash is reclaimed by the regular template cleanup refs.
	vehicleName := cowTemplateBuildRootfsName(templateID)
	volume, err := ingestArtifactSnapshot(ctx, manager, vehicleName, src, size,
		func() (*cowVolume, error) { return manager.CommitTemplateRootfs(ctx, vehicleName, templateID) })
	if err != nil {
		return nil, "", err
	}
	object, err := cubecowVolumeToSnapshotObjectWithoutActivation(ctx, manager, volume)
	if err != nil {
		// The rootfs snapshot is already committed at this point, so drop it
		// here instead of leaving it for the caller's cleanup path.
		if delErr := manager.DeleteByKind(ctx, cowTemplateRootfsName(templateID), cowKindSnapshot); delErr != nil {
			log.G(ctx).Warnf("import: drop committed rootfs object for %s after describe failure: %v", templateID, delErr)
		}
		return nil, "", err
	}
	return object, wlayerSubdir, nil
}

// detectWlayerSubdir loop-mounts the already-validated artifact read-only and
// scans it. Mounting a caller-staged filesystem image is acceptable here
// because staging requires root on the node and the RPC is control-plane only.
func detectWlayerSubdir(ctx context.Context, src *os.File) (string, error) {
	mntDir, err := os.MkdirTemp("", "cube-import-wlayer-")
	if err != nil {
		return "", fmt.Errorf("create wlayer scan mountpoint: %w", err)
	}
	defer func() {
		// Fails while the mount is still live, which is the case worth seeing.
		if err := os.Remove(mntDir); err != nil {
			log.G(ctx).Warnf("remove wlayer scan mountpoint %s: %v", mntDir, err)
		}
	}()

	// Mount through the validated fd so the path cannot be swapped after validation.
	cmd := exec.CommandContext(ctx, "mount", "-t", "ext4", "-o", "loop,ro,nosuid,nodev,noexec", "/proc/self/fd/3", mntDir)
	cmd.ExtraFiles = []*os.File{src}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mount artifact %s for wlayer scan: %v: %s", src.Name(), err, strings.TrimSpace(string(out)))
	}
	defer func() {
		if out, err := exec.Command("umount", mntDir).CombinedOutput(); err != nil {
			log.G(ctx).Warnf("umount wlayer scan mount %s: %v: %s", mntDir, err, strings.TrimSpace(string(out)))
		}
	}()

	return findWlayerSubdir(mntDir)
}

func findWlayerSubdir(root string) (string, error) {
	hasWlayerDirs := func(rel string) bool {
		upper, err := os.Stat(filepath.Join(root, rel, "upper"))
		if err != nil || !upper.IsDir() {
			return false
		}
		work, err := os.Stat(filepath.Join(root, rel, "work"))
		return err == nil && work.IsDir()
	}

	var candidates []string
	tops, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("scan artifact root: %w", err)
	}
	for _, top := range tops {
		if !top.IsDir() || top.Name() == "lost+found" {
			continue
		}
		if hasWlayerDirs(top.Name()) {
			candidates = append(candidates, top.Name())
		}
		subs, err := os.ReadDir(filepath.Join(root, top.Name()))
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if rel := filepath.Join(top.Name(), sub.Name()); sub.IsDir() && hasWlayerDirs(rel) {
				candidates = append(candidates, rel)
			}
		}
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("expected exactly one overlay upper dir, found %v", candidates)
	}
	return candidates[0], nil
}

func ingestArtifactSnapshot(ctx context.Context, manager cowVolumeManager, vehicleName string, src *os.File, size uint64, commit func() (*cowVolume, error)) (*cowVolume, error) {
	vehicle, err := manager.CreateRawVolume(ctx, vehicleName, size)
	if err != nil {
		return nil, fmt.Errorf("import: create vehicle volume %s: %w", vehicleName, err)
	}
	committed := false
	defer func() {
		if !committed {
			if delErr := manager.DeleteByKind(ctx, vehicleName, cowKindVolume); delErr != nil {
				log.G(ctx).Warnf("import: cleanup vehicle %s after failure: %v", vehicleName, delErr)
			}
		}
	}()
	if err := reflinkArtifactIntoFile(ctx, vehicle.FilePath, src, size); err != nil {
		return nil, err
	}
	volume, err := commit()
	if err != nil {
		return nil, fmt.Errorf("import: snapshot vehicle %s: %w", vehicleName, err)
	}
	committed = true
	if delErr := manager.DeleteByKind(ctx, vehicleName, cowKindVolume); delErr != nil {
		log.G(ctx).Warnf("import: drop vehicle volume %s: %v", vehicleName, delErr)
	}
	return volume, nil
}

// CleanupImportedSnapshot best-effort rolls back a partially-completed import.
func CleanupImportedSnapshot(ctx context.Context, templateID string) {
	manager, err := requireCowManager()
	if err != nil {
		return
	}
	if delErr := manager.DeleteByKind(ctx, cowTemplateRootfsName(templateID), cowKindSnapshot); delErr != nil {
		log.G(ctx).Warnf("import cleanup: drop rootfs object for %s: %v", templateID, delErr)
	}
	DeleteSnapshotCatalog(templateID)
}

// openImportArtifact validates that srcPath is a regular file under the import
// staging dir and returns it opened; callers consume the artifact through the
// fd so a symlink swapped in after validation cannot redirect the reflink.
func openImportArtifact(srcPath string) (*os.File, uint64, error) {
	src := strings.TrimSpace(srcPath)
	if src == "" {
		return nil, 0, fmt.Errorf("%w: empty source path", ErrImportSourceInvalid)
	}
	if localStorage == nil || localStorage.config == nil {
		return nil, 0, errors.New("import: storage not initialized")
	}
	staging := filepath.Clean(defaultImportStagingDir(localStorage.config.DataPath))
	cleaned := filepath.Clean(src)
	if !strings.HasPrefix(cleaned, staging+string(filepath.Separator)) {
		return nil, 0, fmt.Errorf("%w: source %s is outside staging dir %s", ErrImportSourceInvalid, cleaned, staging)
	}
	f, err := os.OpenFile(cleaned, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: open source %s: %v", ErrImportSourceInvalid, cleaned, err)
	}
	real, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("%w: resolve source %s: %v", ErrImportSourceInvalid, cleaned, err)
	}
	if !strings.HasPrefix(real, staging+string(filepath.Separator)) {
		f.Close()
		return nil, 0, fmt.Errorf("%w: source resolves outside staging dir: %s", ErrImportSourceInvalid, real)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("import: stat source: %w", err)
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, 0, fmt.Errorf("%w: source %s is not a regular file", ErrImportSourceInvalid, cleaned)
	}
	return f, uint64(st.Size()), nil
}

func reflinkArtifactIntoFile(ctx context.Context, dstPath string, src *os.File, expectSize uint64) error {
	dstFile, err := os.OpenFile(dstPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("import: open target %s: %w", dstPath, err)
	}
	defer dstFile.Close()
	srcStat, err := src.Stat()
	if err != nil {
		return fmt.Errorf("import: stat source: %w", err)
	}
	dstStat, err := dstFile.Stat()
	if err != nil {
		return fmt.Errorf("import: stat target: %w", err)
	}
	if uint64(srcStat.Size()) != expectSize || srcStat.Size() != dstStat.Size() {
		return fmt.Errorf("import: source size %d != volume size %d", srcStat.Size(), dstStat.Size())
	}
	if err := unix.IoctlFileClone(int(dstFile.Fd()), int(src.Fd())); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("%w: source %s must be on the same filesystem as the volume (reflink EXDEV)", ErrImportSourceInvalid, src.Name())
		}
		return fmt.Errorf("import: reflink %s into %s: %w", src.Name(), dstPath, err)
	}
	if err := dstFile.Sync(); err != nil {
		return fmt.Errorf("import: sync target: %w", err)
	}
	log.G(ctx).Infof("import: reflinked %s into %s (%d bytes)", src.Name(), dstPath, expectSize)
	return nil
}
