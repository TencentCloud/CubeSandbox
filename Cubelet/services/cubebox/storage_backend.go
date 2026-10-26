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
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
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

// snapshotWorkLayout is the on-disk package for one Pause／Commit／AppSnapshot.
// Both backends use <work>/<backend>/{snapshots|pause-snapshots}/<id>/.
// S3 keeps memory next to metadata (disk/ is an empty package placeholder);
// XFS keeps files in xfs/objects and shares MetaWork for package metadata.
type snapshotWorkLayout struct {
	Backend      string
	Kind         string
	SnapshotID   string
	Home         string
	TmpHome      string
	ValidateBase string
	MetaDir      string
	MemoryDir    string
	MetaWork     string
	MemoryWork   string
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
		Backend:      normalized,
		Kind:         storage.SnapshotKindNormal,
		SnapshotID:   snapshotID,
		Home:         home,
		TmpHome:      tmpHome,
		ValidateBase: kindRoot,
		MetaDir:      filepath.Join(home, storage.SnapshotMetadataDir),
		MetaWork:     filepath.Join(tmpHome, storage.SnapshotMetadataDir),
	}
	if kind == storage.SnapshotKindPause {
		layout.Kind = storage.SnapshotKindPause
	}
	if normalized == cow.BackendS3 {
		// S3 writes in place: mount the metadata snapshot at the final
		// metadata/ and skip the XFS tmp+rename package dance.
		layout.TmpHome = home
		layout.MetaWork = layout.MetaDir
		layout.MemoryDir = filepath.Join(home, storage.SnapshotMemoryDir)
		layout.MemoryWork = layout.MemoryDir
	} else {
		layout.MemoryDir = layout.MetaDir
		layout.MemoryWork = layout.MetaWork
	}
	for _, p := range []string{layout.Home, layout.TmpHome} {
		if _, err := pathutil.ValidatePathUnderBase(kindRoot, p); err != nil {
			return snapshotWorkLayout{}, err
		}
	}
	return layout, nil
}

func (l snapshotWorkLayout) usesTmpRename() bool {
	return l.Backend != cow.BackendS3
}

func (l snapshotWorkLayout) ensureTmp() error {
	if err := storage.EnsureSnapshotPackage(l.Backend, l.TmpHome); err != nil {
		return err
	}
	return os.MkdirAll(l.MetaWork, 0o755)
}

func (l snapshotWorkLayout) prepareWork(ctx context.Context) error {
	if l.Backend == cow.BackendS3 {
		if err := storage.EnsureSnapshotPackage(l.Backend, l.Home); err != nil {
			return err
		}
		return storage.PrepareS3MetadataMount(ctx, l.Backend, l.SnapshotID, l.MetaDir)
	}
	return l.ensureTmp()
}

func (l snapshotWorkLayout) resetTmpDir() {
	if !l.usesTmpRename() {
		return
	}
	_ = os.RemoveAll(l.TmpHome) // NOCC:Path Traversal()
}

func (l snapshotWorkLayout) discardTmpDir() {
	if !l.usesTmpRename() {
		return
	}
	_ = os.RemoveAll(l.TmpHome) // NOCC:Path Traversal()
}

func (l snapshotWorkLayout) releaseMetadata(ctx context.Context) {
	_ = storage.ReleaseS3MetadataVolume(ctx, l.Backend, l.SnapshotID)
}
