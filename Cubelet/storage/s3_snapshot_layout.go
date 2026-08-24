// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

const (
	// SnapshotKindNormal is the on-disk directory for ordinary snapshots.
	SnapshotKindNormal = "snapshots"
	// SnapshotKindPause is the on-disk directory for pause temporary snapshots.
	SnapshotKindPause = "pause-snapshots"
	// SnapshotMetadataDir holds catalog.json, sandbox_spec.json, hypervisor state.
	SnapshotMetadataDir = "metadata"
	// SnapshotMemoryDir holds S3 memory package placeholders (no host .dev files).
	SnapshotMemoryDir = "memory"
	// SnapshotDiskDir is the S3 package disk role directory (empty placeholder;
	// rootfs identity lives in catalog.json, not a host-local .dev file).
	SnapshotDiskDir = "disk"
	// SnapshotObjectsDir is the XFS cubecow object pool (files, not metadata).
	SnapshotObjectsDir = "objects"

	// Deprecated aliases kept so existing S3 helpers keep compiling.
	S3SnapshotRootName    = "s3"
	S3SnapshotMemoryDir   = SnapshotMemoryDir
	S3SnapshotDiskDir     = SnapshotDiskDir
	S3SnapshotMetadataDir = SnapshotMetadataDir
)

// CubeletStorageWorkPath is Cubelet storage's on-disk work directory
// (config data_path, typically /data/cubelet/storage).
func CubeletStorageWorkPath() string {
	if localStorage != nil && localStorage.config != nil {
		if p := strings.TrimSpace(localStorage.config.DataPath); p != "" {
			return filepath.Clean(stripStoragePluginDataDir(p))
		}
		if p := strings.TrimSpace(localStorage.config.RootPath); p != "" {
			return filepath.Clean(p)
		}
	}
	return filepath.Join(constants.CubeConfigBasePath, "storage")
}

func stripStoragePluginDataDir(dataPath string) string {
	pluginDir := fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.StorageID)
	if filepath.Base(dataPath) == pluginDir {
		return filepath.Dir(dataPath)
	}
	return dataPath
}

func normalizeSnapshotKind(kind string) string {
	if strings.TrimSpace(kind) == SnapshotKindPause ||
		strings.EqualFold(strings.TrimSpace(kind), CatalogKindPauseSnapshot) {
		return SnapshotKindPause
	}
	return SnapshotKindNormal
}

func backendDirName(backend string) string {
	normalized, err := cow.NormalizeBackend(backend)
	if err == nil && normalized == cow.BackendS3 {
		return cow.BackendS3
	}
	return cow.BackendXFS
}

// BackendStorageRoot is <work>/xfs or <work>/s3.
func BackendStorageRoot(backend string) string {
	return filepath.Join(CubeletStorageWorkPath(), backendDirName(backend))
}

// SnapshotKindRoot is <work>/<backend>/{snapshots|pause-snapshots}.
func SnapshotKindRoot(backend, kind string) string {
	return filepath.Join(BackendStorageRoot(backend), normalizeSnapshotKind(kind))
}

// SnapshotHome is <kind-root>/<snapshotID>.
func SnapshotHome(backend, kind, snapshotID string) string {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return ""
	}
	return filepath.Join(SnapshotKindRoot(backend, kind), snapshotID)
}

// SnapshotMetaDir is <home>/metadata.
func SnapshotMetaDir(backend, kind, snapshotID string) string {
	return filepath.Join(SnapshotHome(backend, kind, snapshotID), SnapshotMetadataDir)
}

// XFSObjectsDir is <work>/xfs/objects (cubecow reflink pool).
func XFSObjectsDir() string {
	return filepath.Join(BackendStorageRoot(cow.BackendXFS), SnapshotObjectsDir)
}

// S3SnapshotRoot is the S3 backend root (<work>/s3). Catalogs live in the
// snapshots／pause-snapshots children.
func S3SnapshotRoot() string {
	return BackendStorageRoot(cow.BackendS3)
}

// S3SnapshotHome is the default (normal) S3 package directory.
func S3SnapshotHome(snapshotID string) string {
	return SnapshotHome(cow.BackendS3, SnapshotKindNormal, snapshotID)
}

func S3SnapshotMetaDir(home string) string {
	return filepath.Join(home, SnapshotMetadataDir)
}

func S3SnapshotMemoryDirPath(home string) string {
	return filepath.Join(home, SnapshotMemoryDir)
}

func S3SnapshotDiskDirPath(home string) string {
	return filepath.Join(home, SnapshotDiskDir)
}

func S3CatalogFilePath(home string) string {
	return filepath.Join(S3SnapshotMetaDir(home), snapshotCatalogFileName)
}

// EnsureSnapshotPackage creates the on-disk dirs for one snapshot home.
// S3: memory／disk／metadata. XFS: metadata only (files live in objects/).
func EnsureSnapshotPackage(backend, home string) error {
	home = strings.TrimSpace(home)
	if home == "" {
		return os.ErrInvalid
	}
	dirs := []string{SnapshotMetadataDir}
	if isS3CatalogBackend(backend) {
		dirs = []string{SnapshotMemoryDir, SnapshotDiskDir, SnapshotMetadataDir}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// EnsureS3SnapshotLayout creates memory／disk／metadata under home.
func EnsureS3SnapshotLayout(home string) error {
	return EnsureSnapshotPackage(cow.BackendS3, home)
}

func isS3CatalogBackend(backend string) bool {
	normalized, err := cow.NormalizeBackend(backend)
	return err == nil && normalized == cow.BackendS3
}

// IsS3Backend reports whether backend names the S3 CoW backend. Callers outside
// this package use it to keep S3-only rules — host activation, and snapshots
// living in s3lvol rather than as reflink files — off the XFS paths.
func IsS3Backend(backend string) bool {
	return isS3CatalogBackend(backend)
}

func catalogKindRoots(backend string) []string {
	return []string{
		SnapshotKindRoot(backend, SnapshotKindNormal),
		SnapshotKindRoot(backend, SnapshotKindPause),
	}
}

// EnsureShimSpecDirLink exposes <home>/<specDir> for the installed shim, which
// always loads {base}/{cpu}C{mem}M/metadata.json. Package files stay under
// metadata/; the link is relative so the home can be renamed.
func EnsureShimSpecDirLink(home, specDir string) error {
	home = strings.TrimSpace(home)
	specDir = strings.TrimSpace(specDir)
	if home == "" || specDir == "" || specDir == SnapshotMetadataDir {
		return nil
	}
	if strings.ContainsAny(specDir, `/\`) || strings.Contains(specDir, "..") {
		return fmt.Errorf("invalid shim spec dir %q", specDir)
	}
	meta := filepath.Join(home, SnapshotMetadataDir)
	if _, err := os.Stat(meta); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	link := filepath.Join(home, specDir)
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(link)
			if err == nil && (target == SnapshotMetadataDir || filepath.Clean(target) == meta) {
				return nil
			}
		} else if info.IsDir() {
			if _, err := os.Stat(filepath.Join(link, "metadata.json")); err == nil {
				return nil
			}
		}
		return fmt.Errorf("shim spec path %s already exists", link)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(SnapshotMetadataDir, link)
}
