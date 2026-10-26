// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package refcount implements a persistent reference-count store for
// plugin_volume mounts.
//
// A single host-level plugin volume can be concurrently attached to multiple
// sandbox instances.  The ref-count tracks how many live sandboxes are using
// each (namespace, volumeID) pair and is persisted to bbolt so that a
// cubelet restart does not lose the count.
//
// # Key design
//
//   - Bucket:  "volume_refcounts"
//   - Key:     "<namespace>\x00<volumeID>"   (null byte separator)
//   - Value:   JSON-encoded RefCountRecord
//
// volumeID is the CubeMaster VolumeRecord identifier. It is the correct identifier
// for cross-sandbox deduplication: multiple sandboxes may mount the same
// physical volume under different Volume.name strings, but they all share
// the same volumeID.
//
// # RefCount semantics
//
// The count exposed to plugins follows a "before/after" model:
//
//   - Mount: plugin receives the count BEFORE this sandbox is added.
//     0 → first attach; plugin should perform host-level setup.
//   - Unmount: plugin receives the count AFTER this sandbox is removed.
//     0 → last detach; plugin should tear down host-level resources.
//
// This makes 0 the consistent "boundary" sentinel in both directions.
//
// # Thread safety
//
// Exported methods rely on bbolt's transaction serialisation (one writer,
// concurrent readers).  No outer Go mutex is needed.
//
// # Recovery after restart
//
// Call RecoverRefCounts with all StorageInfo records loaded from the sandbox
// DB on startup to reconcile any mismatch between persisted ref-counts and
// the actual set of live sandboxes.  Sandboxes that are no longer present
// (host reboot, crash, …) are removed from every record; records that reach
// zero are deleted.
package refcount

import (
	"encoding/json"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
)

const (
	// BucketName is the bbolt bucket used to store all ref-count records.
	BucketName = "volume_refcounts"

	// keySep separates namespace from volumeID inside the bbolt key.
	keySep = "\x00"
)

// RefCountRecord is the value stored per volume.
type RefCountRecord struct {
	// Count is the number of live sandbox references.
	Count int64 `json:"count"`

	// SandboxIDs is the set of sandbox IDs currently holding this volume.
	// Used during recovery to detect and remove stale entries.
	SandboxIDs map[string]struct{} `json:"sandbox_ids"`

	// Driver is stored for informational / recovery purposes.
	Driver string `json:"driver"`
}

// Store is a persistent reference-count store backed by a bbolt database.
// Obtain one via New; share a single instance across the whole cubelet process.
type Store struct {
	db *bolt.DB
}

// New opens (or creates) the bbolt bucket inside the given *bolt.DB.
// The caller owns the DB lifetime; close the DB externally on shutdown.
func New(db *bolt.DB) (*Store, error) {
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(BucketName))
		return err
	}); err != nil {
		return nil, fmt.Errorf("refcount: create bucket: %w", err)
	}
	return &Store{db: db}, nil
}

// key encodes the (namespace, volumeID) pair as a bbolt key.
// volumeID is the CubeMaster VolumeRecord identifier, not the Volume.name.
func key(namespace, volumeID string) []byte {
	return []byte(namespace + keySep + volumeID)
}

func getRecord(b *bolt.Bucket, k []byte) (RefCountRecord, error) {
	var rec RefCountRecord
	v := b.Get(k)
	if v == nil {
		rec.SandboxIDs = make(map[string]struct{})
		return rec, nil
	}
	if err := json.Unmarshal(v, &rec); err != nil {
		return rec, fmt.Errorf("refcount: unmarshal %q: %w", k, err)
	}
	if rec.SandboxIDs == nil {
		rec.SandboxIDs = make(map[string]struct{})
	}
	return rec, nil
}

func putRecord(b *bolt.Bucket, k []byte, rec RefCountRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("refcount: marshal: %w", err)
	}
	return b.Put(k, data)
}

// AcquireResult is returned by Acquire.
type AcquireResult struct {
	// Before is the ref-count BEFORE this sandbox was added.
	// 0 means this is the first sandbox to attach the volume.
	Before int64
	// After is the ref-count AFTER this sandbox was added (always ≥ 1).
	After int64
	// AlreadyHeld is true when sandboxID was already in the set (idempotent call).
	AlreadyHeld bool
}

// Acquire registers sandboxID as a holder of (namespace, volumeID).
// It is idempotent: calling Acquire again for the same sandboxID returns
// AlreadyHeld=true and the unchanged counts.
//
// The caller should pass Before to the plugin as MountRequest.RefCount so
// the plugin can detect first-attach (Before == 0).
func (s *Store) Acquire(namespace, volumeID, sandboxID, driver string) (AcquireResult, error) {

	var res AcquireResult
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		k := key(namespace, volumeID)
		rec, err := getRecord(b, k)
		if err != nil {
			return err
		}

		res.Before = rec.Count

		// Idempotent: already registered.
		if _, already := rec.SandboxIDs[sandboxID]; already {
			res.After = rec.Count
			res.AlreadyHeld = true
			return nil
		}

		rec.SandboxIDs[sandboxID] = struct{}{}
		rec.Count = int64(len(rec.SandboxIDs))
		rec.Driver = driver
		res.After = rec.Count
		return putRecord(b, k, rec)
	})
	return res, err
}

// ReleaseResult is returned by Release.
type ReleaseResult struct {
	// After is the ref-count AFTER this sandbox was removed (≥ 0).
	// 0 means this was the last sandbox; host-level cleanup is appropriate.
	After int64
	// NotHeld is true when sandboxID was not in the set (idempotent call).
	NotHeld bool
}

// Release removes sandboxID from the holder set of (namespace, volumeID).
// It is idempotent: calling Release for an unknown sandboxID returns
// NotHeld=true and the unchanged count.
//
// When After reaches 0 the record is deleted from the store.
// The caller should pass After to the plugin as UnmountRequest.RefCount so
// the plugin can detect last-detach (After == 0).
func (s *Store) Release(namespace, volumeID, sandboxID string) (ReleaseResult, error) {

	var res ReleaseResult
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		k := key(namespace, volumeID)
		rec, err := getRecord(b, k)
		if err != nil {
			return err
		}

		// Idempotent: not a current holder.
		if _, present := rec.SandboxIDs[sandboxID]; !present {
			res.After = rec.Count
			res.NotHeld = true
			return nil
		}

		delete(rec.SandboxIDs, sandboxID)
		rec.Count = int64(len(rec.SandboxIDs))
		res.After = rec.Count

		if rec.Count == 0 {
			return b.Delete(k)
		}
		return putRecord(b, k, rec)
	})
	return res, err
}

// Get returns the current ref-count for (namespace, volumeID).
// Returns 0 if no record exists (not an error).
func (s *Store) Get(namespace, volumeID string) (int64, error) {

	var count int64
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		k := key(namespace, volumeID)
		rec, err := getRecord(b, k)
		if err != nil {
			return err
		}
		count = rec.Count
		return nil
	})
	return count, err
}

// GetRecord returns the full RefCountRecord for (namespace, volumeID).
// ok is false if the record does not exist.
func (s *Store) GetRecord(namespace, volumeID string) (RefCountRecord, bool, error) {

	var rec RefCountRecord
	var exists bool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		k := key(namespace, volumeID)
		v := b.Get(k)
		if v == nil {
			return nil
		}
		exists = true
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("refcount: unmarshal: %w", err)
		}
		if rec.SandboxIDs == nil {
			rec.SandboxIDs = make(map[string]struct{})
		}
		return nil
	})
	return rec, exists, err
}

// SandboxRecord describes a volume held by a sandbox.
type SandboxRecord struct {
	Namespace string
	VolumeID  string
	RefCount  int64
}

// ListForSandbox returns all (namespace, volumeID) pairs where sandboxID
// appears in the SandboxIDs set.  Used during recovery.
func (s *Store) ListForSandbox(sandboxID string) ([]SandboxRecord, error) {

	var result []SandboxRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var rec RefCountRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if rec.SandboxIDs == nil {
				return nil
			}
			if _, ok := rec.SandboxIDs[sandboxID]; ok {
				parts := strings.SplitN(string(k), keySep, 2)
				ns, volID := "", string(k)
				if len(parts) == 2 {
					ns, volID = parts[0], parts[1]
				}
				result = append(result, SandboxRecord{
					Namespace: ns,
					VolumeID:  volID,
					RefCount:  rec.Count,
				})
			}
			return nil
		})
	})
	return result, err
}

// RecoverResult is a summary of what RecoverRefCounts did.
type RecoverResult struct {
	RecordsScanned   int
	StaleRefsRemoved int
	RecordsDeleted   int
}

// RecoverRefCounts reconciles the persisted ref-count store against the set of
// actually-live sandbox IDs.  Call this once on cubelet startup, after loading
// all StorageInfo records from the sandbox DB.
//
// Any sandboxID that appears in the store but is absent from liveSandboxIDs is
// treated as dead and removed.  Records that reach 0 refs are deleted.
func (s *Store) RecoverRefCounts(liveSandboxIDs map[string]struct{}) (RecoverResult, error) {

	var res RecoverResult

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		if b == nil {
			return nil
		}

		type kv struct {
			k []byte
			r RefCountRecord
		}
		var all []kv
		if err := b.ForEach(func(k, v []byte) error {
			var rec RefCountRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if rec.SandboxIDs == nil {
				rec.SandboxIDs = make(map[string]struct{})
			}
			kc := make([]byte, len(k))
			copy(kc, k)
			all = append(all, kv{k: kc, r: rec})
			return nil
		}); err != nil {
			return err
		}

		res.RecordsScanned = len(all)

		for _, item := range all {
			rec := item.r
			changed := false
			for sid := range rec.SandboxIDs {
				if _, live := liveSandboxIDs[sid]; !live {
					delete(rec.SandboxIDs, sid)
					res.StaleRefsRemoved++
					changed = true
				}
			}
			if !changed {
				continue
			}
			rec.Count = int64(len(rec.SandboxIDs))
			if rec.Count == 0 {
				if err := b.Delete(item.k); err != nil {
					return err
				}
				res.RecordsDeleted++
			} else {
				if err := putRecord(b, item.k, rec); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return res, err
}

// AllRecords returns all records in the store.  Primarily for diagnostics.
func (s *Store) AllRecords() (map[string]RefCountRecord, error) {

	result := make(map[string]RefCountRecord)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var rec RefCountRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if rec.SandboxIDs == nil {
				rec.SandboxIDs = make(map[string]struct{})
			}
			result[string(k)] = rec
			return nil
		})
	})
	return result, err
}
