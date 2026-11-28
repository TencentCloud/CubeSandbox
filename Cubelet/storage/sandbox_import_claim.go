// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// A cross-node restore brings the sandbox's metadata disk onto this node at
// the Create entry, before any sandbox record exists. Destroy only releases
// disks of sandboxes it knows about, so a Create rejected between the import
// and the record (a package without sandbox_spec.json, a failed parameter
// check, a crash) used to leave the disk mounted under the sandbox's name
// with no owner: it leaks the disk and the node keeps a stray local snapshot.
//
// The import is therefore claimed durably before anything is imported. The
// claim ends when the Create returns: either the sandbox record now owns the
// disks and destroy will release them, or nothing does and the claim
// releases them itself. A claim that outlives its process is reclaimed later.

const sandboxImportBucket = "cross-node-imports/v1"

type sandboxImportRecord struct {
	SandboxID string    `json:"sandbox_id"`
	Backend   string    `json:"backend"`
	ClaimedAt time.Time `json:"claimed_at"`
}

type sandboxImportKV interface {
	ReadAll() (map[string][]byte, error)
	Set(key string, value []byte) error
	Delete(key string) error
}

var (
	sandboxImportMu sync.Mutex
	// In-process imports, by sandbox id. A durable claim whose id is here
	// belongs to a Create still running in this process.
	sandboxImportsInFlight = map[string]int{}
	testSandboxImportKV    sandboxImportKV

	releaseSandboxImportFn = releaseSandboxImportDisks
)

// SandboxImportClaim is one Create's hold on the disks it imports for a
// sandbox that has no record on this node yet.
type SandboxImportClaim struct {
	SandboxID string
	Backend   string
	once      sync.Once
}

// ClaimSandboxImport records that this node is about to import disks for
// sandboxID. It must be called before the first import.
func ClaimSandboxImport(sandboxID, backend string) (*SandboxImportClaim, error) {
	id := strings.TrimSpace(sandboxID)
	if err := pathutil.ValidateSafeID(id); err != nil {
		return nil, fmt.Errorf("claim cross-node import: %w", err)
	}
	backend, err := cow.NormalizeBackend(backend)
	if err != nil || !CrossNodeImportBackend(backend) {
		return nil, fmt.Errorf("claim cross-node import of %s: unsupported backend %q", id, backend)
	}
	sandboxImportMu.Lock()
	defer sandboxImportMu.Unlock()
	body, err := json.Marshal(sandboxImportRecord{SandboxID: id, Backend: backend, ClaimedAt: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	if err := sandboxImportStore().Set(id, body); err != nil {
		return nil, fmt.Errorf("persist cross-node import claim for %s: %w", id, err)
	}
	sandboxImportsInFlight[id]++
	return &SandboxImportClaim{SandboxID: id, Backend: backend}, nil
}

// Finish ends the claim when its Create returns. owned reports whether a
// sandbox record on this node now holds sandboxID; if it does, its destroy
// releases the disks. Otherwise nothing will, and they are released here.
func (c *SandboxImportClaim) Finish(ctx context.Context, owned bool) (err error) {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		sandboxImportMu.Lock()
		defer sandboxImportMu.Unlock()
		if n := sandboxImportsInFlight[c.SandboxID]; n > 1 {
			// Another Create for the same sandbox is still running here and
			// may be about to own these disks; it finishes the claim.
			sandboxImportsInFlight[c.SandboxID] = n - 1
			return
		}
		delete(sandboxImportsInFlight, c.SandboxID)
		if !owned {
			if err = releaseSandboxImportFn(ctx, c.Backend, c.SandboxID); err != nil {
				// Keep the durable claim: the periodic reclaim retries.
				return
			}
		}
		err = sandboxImportStore().Delete(c.SandboxID)
	})
	return err
}

// SandboxImportOwnership answers, for a sandbox id, whether anything on this
// node still owns disks named after it.
type SandboxImportOwnership func(sandboxID string) (bool, error)

// ReclaimAbandonedSandboxImports releases the disks of cross-node imports
// whose Create ended without a sandbox taking them over: durable claims left
// by an earlier process, and, for the S3 backend, metadata disks imported
// before claims existed. Anything owned, in flight, or not shaped exactly
// like an import is left alone.
func ReclaimAbandonedSandboxImports(ctx context.Context, owned SandboxImportOwnership, minAge time.Duration) ([]string, error) {
	if owned == nil {
		return nil, errors.New("reclaim cross-node imports: no ownership source")
	}
	candidates, err := abandonedImportCandidates(ctx, minAge)
	if err != nil {
		return nil, err
	}
	var reclaimed []string
	var errs error
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return reclaimed, errors.Join(errs, err)
		}
		done, err := reclaimOne(ctx, c, owned)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("reclaim cross-node import %s: %w", c.SandboxID, err))
			continue
		}
		if done {
			reclaimed = append(reclaimed, c.SandboxID)
		}
	}
	return reclaimed, errs
}

func reclaimOne(ctx context.Context, c sandboxImportRecord, owned SandboxImportOwnership) (bool, error) {
	sandboxImportMu.Lock()
	defer sandboxImportMu.Unlock()
	if sandboxImportsInFlight[c.SandboxID] > 0 {
		return false, nil
	}
	isOwned, err := owned(c.SandboxID)
	if err != nil {
		return false, err
	}
	if isOwned {
		// The sandbox took the disks over; only the claim was left behind.
		return false, sandboxImportStore().Delete(c.SandboxID)
	}
	if err := releaseSandboxImportFn(ctx, c.Backend, c.SandboxID); err != nil {
		return false, err
	}
	return true, sandboxImportStore().Delete(c.SandboxID)
}

func abandonedImportCandidates(ctx context.Context, minAge time.Duration) ([]sandboxImportRecord, error) {
	all, err := sandboxImportStore().ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read cross-node import claims: %w", err)
	}
	seen := map[string]bool{}
	var out []sandboxImportRecord
	for key, raw := range all {
		var rec sandboxImportRecord
		if err := json.Unmarshal(raw, &rec); err != nil || rec.SandboxID != key || pathutil.ValidateSafeID(key) != nil {
			return nil, fmt.Errorf("cross-node import claim %q is unreadable", key)
		}
		seen[key] = true
		out = append(out, rec)
	}
	legacy, err := legacyS3ImportCandidates(ctx, minAge)
	if err != nil {
		return nil, err
	}
	for _, id := range legacy {
		if !seen[id] {
			out = append(out, sandboxImportRecord{SandboxID: id, Backend: cow.BackendS3})
		}
	}
	return out, nil
}

// legacyS3ImportCandidates finds S3 metadata disks imported for a sandbox
// before claims were recorded. Such a disk is a private RW metadata volume
// named after the id, mounted at the sandbox's own metadata home, with no
// sealed package snapshot behind it: a template or runtime snapshot package
// always has one, and a sandbox's private copy is excluded by the ownership
// check. The home must also be older than minAge so a Create still cloning
// metadata for a new sandbox is never mistaken for one.
func legacyS3ImportCandidates(ctx context.Context, minAge time.Duration) ([]string, error) {
	store, err := requireS3Cow()
	if err != nil || store == nil {
		// No S3 store on this node, nothing imported through it.
		return nil, nil
	}
	s3MetadataMu.Lock()
	state, err := loadS3MetadataState()
	s3MetadataMu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []string
	for id, rec := range state.Derived {
		if pathutil.ValidateSafeID(id) != nil || strings.HasPrefix(id, "tpl-") {
			continue
		}
		home := SnapshotHome(cow.BackendS3, SnapshotKindNormal, id)
		if home == "" || rec.VolName != S3MetadataVolumeName(id) || rec.MountPath != SnapshotMetaDir(cow.BackendS3, SnapshotKindNormal, id) {
			continue
		}
		fi, err := os.Stat(home)
		if err != nil || !fi.IsDir() || time.Since(fi.ModTime()) < minAge {
			continue
		}
		sealed, err := cowObjectPresent(store.GetVolumeInfo(ctx, S3MetadataSnapshotName(id)))
		if err != nil || sealed {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// releaseSandboxImportDisks releases what the Create entry imports: the
// metadata disk and the home it is mounted in. Disks imported later in the
// create workflow are named after the sandbox too, but the workflow only
// reaches them with a storage record, and destroy owns those.
func releaseSandboxImportDisks(ctx context.Context, backend, sandboxID string) error {
	if backend == cow.BackendS3 {
		if err := ReleaseS3MetadataVolume(ctx, backend, sandboxID); err != nil {
			// Never delete files under a disk that may still be mounted.
			return err
		}
	}
	home := SnapshotHome(backend, SnapshotKindNormal, sandboxID)
	if home == "" {
		return nil
	}
	if err := os.RemoveAll(home); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove import home %s: %w", home, err)
	}
	return nil
}

type dbSandboxImportKV struct{}

func (dbSandboxImportKV) ReadAll() (map[string][]byte, error) {
	if localStorage == nil || localStorage.db == nil {
		return nil, errors.New("storage plugin is not initialized")
	}
	all, err := localStorage.db.ReadAll(sandboxImportBucket)
	if errors.Is(err, utils.ErrorBucketNotFound) {
		return map[string][]byte{}, nil
	}
	return all, err
}

func (dbSandboxImportKV) Set(key string, value []byte) error {
	if localStorage == nil || localStorage.db == nil {
		return errors.New("storage plugin is not initialized")
	}
	return localStorage.db.Set(sandboxImportBucket, key, value)
}

func (dbSandboxImportKV) Delete(key string) error {
	if localStorage == nil || localStorage.db == nil {
		return errors.New("storage plugin is not initialized")
	}
	err := localStorage.db.Delete(sandboxImportBucket, key)
	if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
		return nil
	}
	return err
}

func sandboxImportStore() sandboxImportKV {
	if testSandboxImportKV != nil {
		return testSandboxImportKV
	}
	return dbSandboxImportKV{}
}
