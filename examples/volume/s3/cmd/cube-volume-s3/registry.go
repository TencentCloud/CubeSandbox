// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/s3api"
)

type claimsStore interface {
	ReadClaims(context.Context, string) ([]byte, string, error)
	WriteClaims(context.Context, string, []byte, string) error
	PrefixHasData(context.Context, string) (bool, error)
}

// A registration records whether THIS call added ownership. Existing ownership
// must survive a failed create; callers never infer that from success alone.
type registration struct {
	Prefix, VolumeID string
	CreatedThisCall  bool
}

// A deletionLease binds a deletion attempt to its target and random token.
// It is runtime-only; persisted state and registry JSON formats are unchanged.
type deletionLease struct {
	Prefix, VolumeID, ID string
}

const claimsAttempts = 8
const deleteLease = 30 * time.Minute

var now = time.Now // replaced by deterministic lease tests
var errRegistryContended = fmt.Errorf("volume claims registry was contended %d times", claimsAttempts)

func readRegistry(ctx context.Context, store claimsStore) (binding.Claims, string, error) {
	data, etag, err := store.ReadClaims(ctx, binding.ClaimsKey)
	if err != nil {
		return binding.Claims{}, "", err
	}
	registry, err := binding.DecodeClaims(data)
	return registry, etag, err
}

// updateRegistry is the only metadata CAS loop. change returns whether a write
// is needed and must not create/delete data objects. Operations with a nonce
// may confirm their own uncertain PUT; cleanup operations retain their existing
// error behavior by passing no confirmation predicate.
func updateRegistry(ctx context.Context, store claimsStore, change func(*binding.Claims) (bool, error), confirmed func(binding.Claims) bool) error {
	for attempt := 0; attempt < claimsAttempts; attempt++ {
		registry, etag, err := readRegistry(ctx, store)
		if err != nil {
			return err
		}
		changed, err := change(&registry)
		if err != nil || !changed {
			return err
		}
		encoded, err := binding.EncodeClaims(registry)
		if err != nil {
			return fmt.Errorf("encode volume claims: %w", err)
		}
		err = store.WriteClaims(ctx, binding.ClaimsKey, encoded, etag)
		if err == nil {
			return nil
		}
		// A successful PUT can lose its response; an SDK replay can end in 412.
		if confirmed != nil {
			if current, _, readErr := readRegistry(ctx, store); readErr == nil && confirmed(current) {
				return nil
			}
		}
		if !s3api.IsPreconditionFailed(err) {
			return err
		}
	}
	return errRegistryContended
}

func operationID(kind string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate %s id: %w", kind, err)
	}
	return hex.EncodeToString(token[:]), nil
}

func claimPrefix(ctx context.Context, store claimsStore, bucket, prefix, volumeID string) (registration, error) {
	result := registration{Prefix: prefix, VolumeID: volumeID}
	createID := ""
	err := updateRegistry(ctx, store, func(registry *binding.Claims) (bool, error) {
		result.CreatedThisCall = false
		if conflict, ok := binding.FindConflict(registry.Claims, prefix, volumeID); ok {
			return false, fmt.Errorf("prefix %q is already assigned to volume %q", prefix, conflict.VolumeID)
		}
		if claim, ok := binding.FindClaim(registry.Claims, prefix); ok && claim.VolumeID == volumeID {
			result.CreatedThisCall = createID != "" && claim.CreateID == createID && claim.Bucket == bucket
			return false, nil
		}
		if createID == "" {
			var err error
			createID, err = operationID("claim")
			if err != nil {
				return false, err
			}
		}
		registry.Claims = append(registry.Claims, binding.Claim{
			VolumeID: volumeID, Bucket: bucket, Prefix: prefix,
			CreatedAt: time.Now().UTC().Format(time.RFC3339), CreateID: createID,
		})
		result.CreatedThisCall = true
		return true, nil
	}, func(current binding.Claims) bool {
		claim, ok := binding.FindClaim(current.Claims, prefix)
		return ok && claim.VolumeID == volumeID && claim.Bucket == bucket && claim.CreateID == createID
	})
	if err == errRegistryContended {
		err = fmt.Errorf("prefix %q ownership was contended %d times; retry the create", prefix, claimsAttempts)
	}
	if err != nil {
		return registration{}, fmt.Errorf("create: %w", err)
	}
	return result, nil
}

func cancelRegistration(ctx context.Context, store claimsStore, registered registration) error {
	if !registered.CreatedThisCall {
		return nil
	}
	return updateRegistry(ctx, store, func(registry *binding.Claims) (bool, error) {
		claim, ok := binding.FindClaim(registry.Claims, registered.Prefix)
		if !ok {
			return false, nil
		}
		if claim.VolumeID != registered.VolumeID {
			return false, fmt.Errorf("prefix %q is now claimed by volume %q; leaving that claim in place", registered.Prefix, claim.VolumeID)
		}
		registry.Claims = binding.WithoutClaim(registry.Claims, registered.Prefix, registered.VolumeID)
		return true, nil
	}, nil)
}

// A nil lease with no error means the prefix is already empty and unclaimed.
func beginDestroy(ctx context.Context, store claimsStore, prefix, volumeID string) (*deletionLease, error) {
	var lease *deletionLease
	err := updateRegistry(ctx, store, func(registry *binding.Claims) (bool, error) {
		lease = nil
		if nested, ok := binding.FindNested(registry.Claims, prefix); ok {
			return false, fmt.Errorf("refusing to delete prefix %q: it contains volume %q", prefix, nested.VolumeID)
		}
		claim, ok := binding.FindClaim(registry.Claims, prefix)
		if !ok {
			hasData, err := store.PrefixHasData(ctx, prefix)
			if err != nil {
				return false, err
			}
			if !hasData {
				return false, nil
			}
			return false, fmt.Errorf("refusing to delete prefix %q: no ownership claim records it; the data may belong to another volume", prefix)
		}
		if claim.VolumeID != volumeID {
			return false, fmt.Errorf("refusing to delete prefix %q: it belongs to volume %q", prefix, claim.VolumeID)
		}
		if !deleteLeaseExpired(claim.DeletingAt) {
			return false, fmt.Errorf("destroy of prefix %q is already in progress; retry after the lease expires", prefix)
		}
		id, err := operationID("deletion lease")
		if err != nil {
			return false, err
		}
		lease = &deletionLease{Prefix: prefix, VolumeID: volumeID, ID: id}
		registry.Claims = binding.SetClaimDeleting(registry.Claims, prefix, volumeID, now().UTC().Format(time.RFC3339), id)
		return true, nil
	}, func(current binding.Claims) bool {
		claim, ok := binding.FindClaim(current.Claims, prefix)
		return lease != nil && ok && claim.VolumeID == volumeID && claim.LeaseID == lease.ID
	})
	if err == errRegistryContended {
		err = fmt.Errorf("prefix %q ownership was contended %d times; retry the destroy", prefix, claimsAttempts)
	}
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func finishDestroy(ctx context.Context, store claimsStore, lease deletionLease) error {
	return updateRegistry(ctx, store, func(registry *binding.Claims) (bool, error) {
		claim, ok := binding.FindClaim(registry.Claims, lease.Prefix)
		if !ok {
			return false, nil
		}
		if claim.VolumeID != lease.VolumeID {
			return false, fmt.Errorf("prefix %q is now claimed by volume %q; leaving that claim in place", lease.Prefix, claim.VolumeID)
		}
		// Preserve the existing takeover/cleared-lease semantics.
		if claim.LeaseID != "" && claim.LeaseID != lease.ID {
			return false, fmt.Errorf("deletion lease for prefix %q was taken over by another destroy; leaving the claim in place", lease.Prefix)
		}
		registry.Claims = binding.WithoutClaim(registry.Claims, lease.Prefix, lease.VolumeID)
		return true, nil
	}, nil)
}

func clearDeleteLease(ctx context.Context, store claimsStore, lease deletionLease) error {
	return updateRegistry(ctx, store, func(registry *binding.Claims) (bool, error) {
		claim, ok := binding.FindClaim(registry.Claims, lease.Prefix)
		if !ok || claim.VolumeID != lease.VolumeID || claim.LeaseID != lease.ID {
			return false, nil
		}
		registry.Claims = binding.SetClaimDeleting(registry.Claims, lease.Prefix, lease.VolumeID, "", "")
		return true, nil
	}, nil)
}

func deleteLeaseExpired(at string) bool {
	if at == "" {
		return true
	}
	started, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return true
	}
	return now().Sub(started) > deleteLease
}
