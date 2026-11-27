// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

type s3FinalizeFixture struct {
	id       string
	home     string
	metaDir  string
	metaWork string
	metaSnap string
	memWork  string
	memSnap  string
	engine   *fakeCowEngine
}

type s3FinalizeOpts struct {
	kind           string
	withMemory     bool
	withPauseSpec  bool
	skipValidFiles bool
	skipCatalog    bool
	catalogEntry   func(*s3FinalizeFixture, *SnapshotCatalogEntry)
	setupEngine    func(*s3FinalizeFixture, *fakeCowEngine)
}

func newS3FinalizeFixture(t *testing.T, id string, opts s3FinalizeOpts) *s3FinalizeFixture {
	t.Helper()
	stubS3MetadataMounts(t)

	snapKind := opts.kind
	if snapKind == "" {
		snapKind = SnapshotKindNormal
	}
	f := &s3FinalizeFixture{
		id:       id,
		metaWork: S3MetadataVolumeName(id),
		metaSnap: S3MetadataSnapshotName(id),
		memWork:  "tpl-" + id + "-memory",
		memSnap:  "tpl-" + id + "-memory-snap",
	}
	f.engine = &fakeCowEngine{
		createVolumePath: "/dev/mapper/" + f.metaWork,
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-" + id + "-rootfs": {SizeBytes: 1 << 20},
		},
	}
	if opts.withMemory {
		f.engine.volumeInfos[f.memWork] = &cubecow.Volume{SizeBytes: 64 << 20}
	}
	if opts.setupEngine != nil {
		opts.setupEngine(f, f.engine)
	}
	useTestCowStorageAt(t, f.engine)
	t.Cleanup(func() { DeleteSnapshotCatalogFor(cow.BackendS3, id) })

	f.home = SnapshotHome(cow.BackendS3, snapKind, id)
	require.NoError(t, EnsureSnapshotPackage(cow.BackendS3, f.home))
	f.metaDir = filepath.Join(f.home, SnapshotMetadataDir)
	if !opts.skipValidFiles {
		writeValidS3PackageMetadata(t, f.metaDir, opts.withMemory, opts.withPauseSpec)
	}
	if !opts.skipCatalog {
		entry := &SnapshotCatalogEntry{
			SnapshotID:   id,
			SnapshotPath: f.home,
			MetaDir:      f.metaDir,
			RootfsVol:    "tpl-" + id + "-rootfs",
			RootfsKind:   cowKindSnapshot,
			MetadataVol:  f.metaWork,
			MetadataKind: cowKindVolume,
			Backend:      cow.BackendS3,
		}
		if opts.withMemory {
			entry.MemoryVol = f.memWork
			entry.MemoryKind = cowKindVolume
		}
		if snapKind == SnapshotKindPause {
			entry.Kind = CatalogKindPauseSnapshot
		}
		if opts.catalogEntry != nil {
			opts.catalogEntry(f, entry)
		}
		require.NoError(t, WriteSnapshotCatalogFor(cow.BackendS3, entry))
	}
	return f
}

func TestFinalizePacksMetadataWithoutMounting(t *testing.T) {
	f := newS3FinalizeFixture(t, "snap-pack-1", s3FinalizeOpts{
		withMemory: true,
		setupEngine: func(_ *s3FinalizeFixture, e *fakeCowEngine) {
			e.volumeInfos[S3MetadataBaseVolumeName] = &cubecow.Volume{SizeBytes: 8 << 20}
		},
	})

	mountCalls := 0
	mountS3MetadataDevice = func(string, string) error {
		mountCalls++
		return errors.New("write path must never mount metadata disk")
	}
	// Route through real packS3MetadataDeviceImpl to inspect mkfs.ext4 argv.
	packS3MetadataDevice = packS3MetadataDeviceImpl
	packCalls := 0
	var gotArgv []string
	var catalogAtPack SnapshotCatalogEntry
	execS3MetadataPack = func(argv []string, _ time.Duration) (string, string, error) {
		packCalls++
		gotArgv = append([]string(nil), argv...)
		body, err := os.ReadFile(filepath.Join(f.metaDir, snapshotCatalogFileName))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &catalogAtPack))
		return "", "", nil
	}

	res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err)
	require.NoError(t, res.CleanupErr)
	require.Zero(t, mountCalls, "finalize must not call mount")
	require.Equal(t, 1, packCalls)
	require.Equal(t, []string{"mkfs.ext4", "-F", "-O", "^has_journal", "-b", "4096", "-d", f.metaDir, f.engine.createVolumePath}, gotArgv)
	require.Equal(t, f.metaSnap, catalogAtPack.MetadataVol)
	require.Equal(t, cowKindSnapshot, catalogAtPack.MetadataKind)
	require.Equal(t, f.memSnap, catalogAtPack.MemoryVol)
	require.Equal(t, cowKindSnapshot, catalogAtPack.MemoryKind)

	// Host MetaDir stays as an empty directory after sealing.
	entries, err := os.ReadDir(f.metaDir)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.Contains(t, f.engine.createSnapshots, [2]string{f.metaWork, f.metaSnap})
	require.Contains(t, f.engine.deletedVolumes, f.metaWork)
	require.Contains(t, f.engine.deactivatedVolumes, f.metaWork)
	require.Contains(t, f.engine.deactivatedVolumes, f.metaSnap)
}

func TestFinalizePackFailureKeepsPlainPackage(t *testing.T) {
	f := newS3FinalizeFixture(t, "snap-pack-fail", s3FinalizeOpts{withMemory: true})
	packS3MetadataDevice = func(string, string) error {
		return errors.New("mkfs.ext4 ENOSPC")
	}

	_, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
	require.ErrorContains(t, err, "mkfs.ext4 ENOSPC")
	require.FileExists(t, filepath.Join(f.metaDir, "snapshot", "config.json"))
	require.FileExists(t, filepath.Join(f.metaDir, snapshotCatalogFileName))
	require.NotContains(t, f.engine.createSnapshots, [2]string{f.metaWork, f.metaSnap})

	// Retrying Finalize after the failure recovers cleanly using the work vol.
	packS3MetadataDevice = func(dir, dev string) error {
		require.Equal(t, f.metaDir, dir)
		require.NotEmpty(t, dev)
		return nil
	}
	res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err)
	require.NoError(t, res.CleanupErr)
	require.Contains(t, f.engine.createSnapshots, [2]string{f.metaWork, f.metaSnap})
	entries, err := os.ReadDir(f.metaDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestFinalizePostSealCleanupErrorsDoNotFailSeal(t *testing.T) {
	f := newS3FinalizeFixture(t, "snap-cleanup-err", s3FinalizeOpts{
		withMemory: true,
		setupEngine: func(f *s3FinalizeFixture, e *fakeCowEngine) {
			e.deactivateErr = errors.New("deactivate busy")
			e.deleteErrByName = map[string]error{f.metaWork: errors.New("delete work busy")}
		},
	})
	testS3MetadataKV = &memS3MetadataKV{setErr: errors.New("kv write failed")}

	res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err, "post-seal cleanup errors must not fail sealing")
	require.ErrorContains(t, res.CleanupErr, "deactivate busy")
	require.ErrorContains(t, res.CleanupErr, "delete work busy")
	require.ErrorContains(t, res.CleanupErr, "kv write failed")
	require.Contains(t, f.engine.createSnapshots, [2]string{f.memWork, f.memSnap})
	require.Contains(t, f.engine.createSnapshots, [2]string{f.metaWork, f.metaSnap})
	require.True(t, res.Sealed, "post-seal cleanup issues must not unset Sealed")
}

func TestFinalizeCatalogPersistFailureKeepsPackage(t *testing.T) {
	f := newS3FinalizeFixture(t, "snap-catalog-fail", s3FinalizeOpts{withMemory: true})

	// A directory at catalog.json makes the rewrite's rename fail.
	catalogPath := filepath.Join(f.metaDir, snapshotCatalogFileName)
	require.NoError(t, os.Remove(catalogPath))
	require.NoError(t, os.Mkdir(catalogPath, 0o755))

	res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err, "a catalog write failure must not fail finalize")
	require.False(t, res.Sealed, "package must stay unsealed without a persisted catalog")
	require.ErrorContains(t, res.CleanupErr, "persist s3 package catalog")
	require.Empty(t, f.engine.createSnapshots, "nothing may be sealed without a persisted catalog")
	require.NotContains(t, f.engine.deletedVolumes, f.metaWork, "work volume must survive an unsealed package")
	require.NotContains(t, f.engine.deletedVolumes, f.memWork, "memory work volume must survive an unsealed package")
	require.FileExists(t, filepath.Join(f.metaDir, "metadata.json"))
	require.DirExists(t, filepath.Join(f.metaDir, "snapshot"))
}

func TestFinalizeValidatesContentsAndSupportsFsOnly(t *testing.T) {
	t.Run("rejects incomplete full dir without rewriting catalog or packing", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-invalid-pack", s3FinalizeOpts{
			withMemory:     true,
			skipValidFiles: true,
		})
		writeValidS3PackageMetadata(t, f.metaDir, false, false)

		packCalled := false
		packS3MetadataDevice = func(string, string) error {
			packCalled = true
			return nil
		}

		_, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.ErrorContains(t, err, "state.json")
		require.False(t, packCalled)
		entry, catErr := GetLocalSnapshotFor(context.Background(), cow.BackendS3, f.id)
		require.NoError(t, catErr)
		require.Equal(t, f.metaWork, entry.MetadataVol)
		require.Equal(t, f.memWork, entry.MemoryVol)
	})

	t.Run("pause package requires sandbox_spec.json", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-pause-nospec", s3FinalizeOpts{
			kind:          SnapshotKindPause,
			withMemory:    true,
			withPauseSpec: false,
		})
		_, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.ErrorContains(t, err, "sandbox_spec.json")
	})

	t.Run("fs-only snapshot without state.json packs cleanly", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-fsonly", s3FinalizeOpts{withMemory: false})
		res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.NoError(t, err)
		require.NoError(t, res.CleanupErr)
		require.Contains(t, f.engine.createSnapshots, [2]string{f.metaWork, f.metaSnap})
	})
}

func TestFinalizeIdempotentRetryAndFallback(t *testing.T) {
	t.Run("skips pack when metaSnap already exists even if metaDir was cleared", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-meta-exists", s3FinalizeOpts{
			skipValidFiles: true,
			setupEngine: func(f *s3FinalizeFixture, e *fakeCowEngine) {
				e.volumeInfos[f.memSnap] = &cubecow.Volume{SizeBytes: 64 << 20}
				e.volumeInfos[f.metaSnap] = &cubecow.Volume{SizeBytes: 8 << 20}
			},
			catalogEntry: func(f *s3FinalizeFixture, entry *SnapshotCatalogEntry) {
				entry.MemoryVol, entry.MemoryKind = f.memSnap, cowKindSnapshot
				entry.MetadataVol, entry.MetadataKind = f.metaSnap, cowKindSnapshot
			},
		})
		require.NoError(t, resetDir(f.metaDir))

		packCalled := false
		packS3MetadataDevice = func(string, string) error {
			packCalled = true
			return errors.New("pack must be skipped when metaSnap already exists")
		}

		res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.NoError(t, err)
		require.NoError(t, res.CleanupErr)
		require.False(t, packCalled)
		require.Empty(t, f.engine.createSnapshots)
		require.NotContains(t, f.engine.deletedVolumes, f.memWork)
		require.Contains(t, f.engine.deactivatedVolumes, f.memSnap)
		// A retry must reclaim the leftover metadata work volume.
		require.Contains(t, f.engine.deletedVolumes, f.metaWork)
	})

	t.Run("trims -snap when catalog rewrote memory_vol before seal", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-mem-retry", s3FinalizeOpts{
			withMemory:     true,
			skipValidFiles: true,
			setupEngine: func(f *s3FinalizeFixture, e *fakeCowEngine) {
				e.volumeInfos[f.metaSnap] = &cubecow.Volume{SizeBytes: 8 << 20}
			},
			catalogEntry: func(f *s3FinalizeFixture, entry *SnapshotCatalogEntry) {
				entry.MemoryVol, entry.MemoryKind = f.memSnap, cowKindSnapshot
				entry.MetadataVol, entry.MetadataKind = f.metaSnap, cowKindSnapshot
			},
		})

		res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.NoError(t, err)
		require.NoError(t, res.CleanupErr)
		require.Contains(t, f.engine.createSnapshots, [2]string{f.memWork, f.memSnap})
		require.Contains(t, f.engine.deletedVolumes, f.memWork)
	})

	t.Run("does not reset host metaDir when metadata points at base", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-base-meta", s3FinalizeOpts{
			catalogEntry: func(_ *s3FinalizeFixture, entry *SnapshotCatalogEntry) {
				entry.MetadataVol = S3MetadataBaseVolumeName
				entry.MetadataKind = cowKindVolume
			},
		})

		res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.NoError(t, err)
		require.NoError(t, res.CleanupErr)
		require.FileExists(t, filepath.Join(f.metaDir, "metadata.json"), "host metadata must remain when no metadata disk was sealed")
	})
}

func TestFinalizeUnmountsLegacyMountBeforePack(t *testing.T) {
	t.Run("unmounts stale mount and packs", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-legacy-mount", s3FinalizeOpts{})
		require.NoError(t, mountS3MetadataDevice("/dev/nvme1n1", f.metaDir))
		require.True(t, s3MetadataIsMounted(f.metaDir))

		res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.NoError(t, err)
		require.NoError(t, res.CleanupErr)
		require.False(t, s3MetadataIsMounted(f.metaDir))
	})

	t.Run("unmount error aborts before pack", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-legacy-umount-fail", s3FinalizeOpts{})
		require.NoError(t, mountS3MetadataDevice("/dev/nvme1n1", f.metaDir))
		unmountS3MetadataPath = func(string) error { return errors.New("umount EBUSY") }
		packCalled := false
		packS3MetadataDevice = func(string, string) error {
			packCalled = true
			return nil
		}

		_, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.ErrorContains(t, err, "umount EBUSY")
		require.False(t, packCalled)
	})
}

func TestFinalizeRejectsUnsafeOrSymlinkMetaDir(t *testing.T) {
	t.Run("outside s3 storage root", func(t *testing.T) {
		outside := t.TempDir()
		writeValidS3PackageMetadata(t, outside, false, false)
		f := newS3FinalizeFixture(t, "snap-outside-root", s3FinalizeOpts{
			skipValidFiles: true,
			catalogEntry: func(_ *s3FinalizeFixture, entry *SnapshotCatalogEntry) {
				entry.SnapshotPath = filepath.Dir(outside)
				entry.MetaDir = outside
			},
		})
		_, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.ErrorContains(t, err, "not under base")
		require.FileExists(t, filepath.Join(outside, "metadata.json"))
	})

	t.Run("s3 root, intermediate symlink, trailing symlink, and resetDir bounds", func(t *testing.T) {
		stubS3MetadataMounts(t)
		useTestCowStorageAt(t, &fakeCowEngine{})
		s3Root := BackendStorageRoot(cow.BackendS3)
		require.NoError(t, os.MkdirAll(s3Root, 0o755))

		_, err := validateS3MetadataDir(s3Root)
		require.ErrorContains(t, err, "refuse to use s3 storage root")

		outside := t.TempDir()
		writeValidS3PackageMetadata(t, outside, false, false)
		normalRoot := filepath.Join(s3Root, SnapshotKindNormal)
		require.NoError(t, os.MkdirAll(normalRoot, 0o755))

		// Intermediate symlink component inside S3 root.
		linkPkg := filepath.Join(normalRoot, "symlink-pkg")
		require.NoError(t, os.Symlink(outside, linkPkg))
		_, err = validateS3MetadataDir(filepath.Join(linkPkg, SnapshotMetadataDir))
		require.ErrorContains(t, err, "symlink")

		// Trailing symlink metaDir rejected end-to-end by Finalize without touching target.
		id := "snap-symlink-dir"
		home := SnapshotHome(cow.BackendS3, SnapshotKindNormal, id)
		require.NoError(t, os.MkdirAll(home, 0o755))
		metaLink := filepath.Join(home, SnapshotMetadataDir)
		require.NoError(t, os.Symlink(outside, metaLink))
		t.Cleanup(func() { DeleteSnapshotCatalogFor(cow.BackendS3, id) })
		require.NoError(t, WriteSnapshotCatalogFor(cow.BackendS3, &SnapshotCatalogEntry{
			SnapshotID:   id,
			SnapshotPath: home,
			MetaDir:      metaLink,
			RootfsVol:    "tpl-" + id + "-rootfs",
			RootfsKind:   cowKindSnapshot,
			Backend:      cow.BackendS3,
		}))
		_, err = FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, id)
		require.ErrorContains(t, err, "symlink")
		require.FileExists(t, filepath.Join(outside, "metadata.json"))

		// resetDir edge cases: empty/blank/missing under S3 root are no-ops; outside root is rejected.
		require.NoError(t, resetDir(""))
		require.NoError(t, resetDir("   "))
		require.NoError(t, resetDir(filepath.Join(normalRoot, "does-not-exist", SnapshotMetadataDir)))
		require.ErrorContains(t, resetDir(filepath.Join(outside, "does-not-exist")), "unsafe s3 metadata dir")
	})

	t.Run("symlink entry inside metaDir rejected before pack", func(t *testing.T) {
		f := newS3FinalizeFixture(t, "snap-symlink-file", s3FinalizeOpts{})
		secret := filepath.Join(t.TempDir(), "secret.txt")
		require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
		require.NoError(t, os.Symlink(secret, filepath.Join(f.metaDir, "leak")))

		packCalled := false
		packS3MetadataDevice = func(string, string) error {
			packCalled = true
			return nil
		}
		_, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, f.id)
		require.ErrorContains(t, err, "symlink")
		require.False(t, packCalled)
	})
}

func TestPackagePipelineFacadeAndCatalogAutoFill(t *testing.T) {
	f := newS3FinalizeFixture(t, "tpl-facade-autofill", s3FinalizeOpts{skipCatalog: true})
	t.Cleanup(func() {
		DeleteSnapshotCatalogFor(cow.BackendXFS, f.id)
		DeleteSnapshotCatalogFor(cow.BackendJFS, f.id)
	})

	// Non-S3 backends are no-ops across the facade and leave MetadataVol/Kind empty.
	for _, nonS3 := range []string{cow.BackendXFS, cow.BackendJFS} {
		require.NoError(t, ValidateStagedPackage(nonS3, t.TempDir(), true))
		res, err := FinalizePublishedPackage(context.Background(), nonS3, f.id)
		require.NoError(t, err)
		require.NoError(t, res.CleanupErr)
		require.NoError(t, DiscardSealedPackage(context.Background(), nonS3, f.id))
		require.NoError(t, ReleasePackageMetadata(context.Background(), nonS3, f.id))

		home := filepath.Join(t.TempDir(), nonS3, f.id)
		require.NoError(t, WriteSnapshotCatalogFor(nonS3, &SnapshotCatalogEntry{
			SnapshotID:   f.id,
			SnapshotPath: home,
			MetaDir:      filepath.Join(home, SnapshotMetadataDir),
			Backend:      nonS3,
		}))
		entry, err := GetLocalSnapshotFor(context.Background(), nonS3, f.id)
		require.NoError(t, err)
		require.Empty(t, entry.MetadataVol)
		require.Empty(t, entry.MetadataKind)
	}

	// S3: ValidateStagedPackage checks memory-aware files; WriteSnapshotCatalogFor
	// auto-fills MetadataVol/Kind when empty and preserves explicit -snap entries.
	require.NoError(t, ValidateStagedPackage(cow.BackendS3, f.metaDir, false))
	require.ErrorContains(t, ValidateStagedPackage(cow.BackendS3, f.metaDir, true), "state.json")

	require.NoError(t, WriteSnapshotCatalogFor(cow.BackendS3, &SnapshotCatalogEntry{
		SnapshotID:   f.id,
		SnapshotPath: f.home,
		MetaDir:      f.metaDir,
		RootfsVol:    "tpl-" + f.id + "-rootfs",
		RootfsKind:   cowKindSnapshot,
		Backend:      cow.BackendS3,
	}))
	entry, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err)
	require.Equal(t, f.metaWork, entry.MetadataVol)
	require.Equal(t, cowKindVolume, entry.MetadataKind)

	res, err := FinalizePublishedPackage(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err)
	require.NoError(t, res.CleanupErr)
	sealed, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err)
	require.Equal(t, f.metaSnap, sealed.MetadataVol)
	require.Equal(t, cowKindSnapshot, sealed.MetadataKind)

	require.NoError(t, WriteSnapshotCatalogFor(cow.BackendS3, sealed))
	rewritten, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, f.id)
	require.NoError(t, err)
	require.Equal(t, f.metaSnap, rewritten.MetadataVol)

	require.NoError(t, DiscardSealedPackage(context.Background(), cow.BackendS3, f.id))
	require.Contains(t, f.engine.deletedSnapshots, f.metaSnap)
	require.NoError(t, ReleasePackageMetadata(context.Background(), cow.BackendS3, f.id))
	require.Contains(t, f.engine.deletedVolumes, f.metaWork)
}
