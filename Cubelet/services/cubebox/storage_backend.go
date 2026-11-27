// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

var (
	finalizePublishedPackageFn = storage.FinalizePublishedPackage
	discardSealedPackageFn     = storage.DiscardSealedPackage
)

// resolveRequestStorageBackend maps Master-supplied backend (xfs｜s3) onto a
// live Store. Empty defaults to xfs.
func resolveRequestStorageBackend(raw string) (string, error) {
	return cow.NormalizeBackend(raw)
}

// storageBackendFromAnnotations reads cube.master.storage.backend from a
// Master Update / Create annotation map.
func storageBackendFromAnnotations(ann map[string]string) (string, error) {
	if ann == nil {
		return cow.BackendXFS, nil
	}
	return resolveRequestStorageBackend(ann[constants.MasterAnnotationStorageBackend])
}

// snapshotRootForBackend returns Cubelet's on-disk backend root
// (<work>/xfs or <work>/s3).
func snapshotRootForBackend(backend string) string {
	return storage.BackendStorageRoot(backend)
}

// snapshotDirForRequest is the kind root for ordinary snapshots.
// requested paths outside the Cubelet work tree are ignored.
func snapshotDirForRequest(backend, requested string) string {
	return snapshotKindDirForRequest(backend, storage.SnapshotKindNormal, requested)
}

func snapshotKindDirForRequest(backend, kind, requested string) string {
	root := storage.SnapshotKindRoot(backend, kind)
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return root
	}
	if _, err := pathutil.ValidatePathUnderBase(root, requested); err != nil {
		return root
	}
	return requested
}

// snapshotWorkLayout is the on-disk package for one Pause/Commit/AppSnapshot:
// stage under <home>.tmp and publish by rename.
type snapshotWorkLayout struct {
	Backend    string
	Kind       string
	SnapshotID string
	Home       string
	TmpHome    string
	MetaDir    string // <home>/metadata
	MetaWork   string // <home>.tmp/metadata: the only directory handed to the shim
}

func prepareSnapshotWorkLayout(backend, kind, snapshotID, requestedDir, specDir string) (snapshotWorkLayout, error) {
	normalized, err := resolveRequestStorageBackend(backend)
	if err != nil {
		return snapshotWorkLayout{}, err
	}
	if err := pathutil.ValidateSafeID(snapshotID); err != nil {
		return snapshotWorkLayout{}, fmt.Errorf("invalid snapshot id: %w", err)
	}
	_ = specDir
	kindRoot := snapshotKindDirForRequest(normalized, kind, requestedDir)
	home := filepath.Join(kindRoot, snapshotID)
	tmpHome := home + ".tmp"
	layout := snapshotWorkLayout{
		Backend:    normalized,
		Kind:       storage.SnapshotKindNormal,
		SnapshotID: snapshotID,
		Home:       home,
		TmpHome:    tmpHome,
		MetaDir:    filepath.Join(home, storage.SnapshotMetadataDir),
		MetaWork:   filepath.Join(tmpHome, storage.SnapshotMetadataDir),
	}
	if kind == storage.SnapshotKindPause {
		layout.Kind = storage.SnapshotKindPause
	}
	for _, p := range []string{layout.Home, layout.TmpHome} {
		if _, err := pathutil.ValidatePathUnderBase(kindRoot, p); err != nil {
			return snapshotWorkLayout{}, err
		}
	}
	return layout, nil
}

func (l snapshotWorkLayout) prepareWork(context.Context) error {
	return storage.EnsureSnapshotPackage(l.Backend, l.TmpHome)
}

func (l snapshotWorkLayout) discardTmpDir() {
	_ = os.RemoveAll(l.TmpHome) // NOCC:Path Traversal()
}

// publish moves the staged package to its final home.
func (l snapshotWorkLayout) publish() error {
	if err := os.RemoveAll(l.Home); err != nil { // NOCC:Path Traversal()
		return fmt.Errorf("remove existing snapshot home %s: %w", l.Home, err)
	}
	return os.Rename(l.TmpHome, l.Home)
}

func (l snapshotWorkLayout) releaseMetadata(ctx context.Context) {
	_ = storage.ReleasePackageMetadata(ctx, l.Backend, l.SnapshotID)
}

// finalizePublishedPackage seals a published package. Sealing failures discard
// the package; post-seal cleanup errors only warn.
func finalizePublishedPackage(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	backend, snapshotID, snapshotPath string,
	onFatalCleanup func(),
	onCleanupDeferred func(),
) error {
	res, err := finalizePublishedPackageFn(ctx, backend, snapshotID)
	if err != nil {
		if stepLog != nil {
			stepLog.Errorf("failed to finalize package snapshots for %s: %v", snapshotID, err)
		}
		if onFatalCleanup != nil {
			onFatalCleanup()
		}
		_ = discardSealedPackageFn(ctx, backend, snapshotID)
		storage.DeleteSnapshotCatalogFor(backend, snapshotID)
		_ = os.RemoveAll(snapshotPath) // NOCC:Path Traversal()
		return fmt.Errorf("failed to finalize package snapshots for %s: %w", snapshotID, err)
	}
	if res.CleanupErr != nil {
		if stepLog != nil {
			stepLog.Warnf("package %s sealed with deferred post-seal cleanup: %v", snapshotID, res.CleanupErr)
		}
		if onCleanupDeferred != nil {
			onCleanupDeferred()
		}
	}
	return nil
}
