// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/s3api"
)

func TestClaimPrefixRecoversCommittedWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport error", errors.New("connection reset after commit")},
		{"final precondition failure", s3api.ErrPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			seedClaims(t, store, binding.Claim{VolumeID: "outside-volume", Prefix: "outside/"})
			before := store.writes
			store.commitThenErr = tc.err
			claimed, err := claimPrefix(context.Background(), store, "user-bucket", "workspaces/repro/", "repro-volume")
			if err != nil || !claimed.CreatedThisCall {
				t.Fatalf("claimed=%v err=%v, want recovery of this call's committed claim", claimed, err)
			}
			if store.writes != before+1 {
				t.Fatalf("writes=%d, want one committed write", store.writes-before)
			}
			ownedRegistration := claimed
			// A new invocation must distinguish an existing claim from its own
			// uncertain write, so later failures do not clean up old ownership.
			claimed, err = claimPrefix(context.Background(), store, "user-bucket", "workspaces/repro/", "repro-volume")
			if err != nil || claimed.CreatedThisCall || store.writes != before+1 {
				t.Fatalf("retry claimed=%v err=%v writes=%d, want unchanged pre-existing ownership", claimed, err, store.writes-before)
			}
			if err := cancelRegistration(context.Background(), store, ownedRegistration); err != nil {
				t.Fatal(err)
			}
			claims := registryOf(t, store).Claims
			if len(claims) != 1 || !binding.HasClaim(claims, "outside/", "outside-volume") {
				t.Fatalf("claims=%+v, want only unrelated ownership after cleanup", claims)
			}
		})
	}
}

func TestClaimPrefixRecoveryDoesNotAdoptOtherOwnership(t *testing.T) {
	for _, kind := range []string{"different token", "different volume", "different prefix", "different bucket"} {
		t.Run(kind, func(t *testing.T) {
			store := newFakeStore()
			failure := errors.New("uncertain registration write")
			wrapped := interceptedClaimsStore{claimsStore: store}
			wrapped.write = func(_ context.Context, _ string, data []byte, _ string) error {
				registry, err := binding.DecodeClaims(data)
				if err != nil {
					t.Fatal(err)
				}
				claim := registry.Claims[0]
				if len(claim.CreateID) != 32 {
					t.Fatalf("CreateID=%q, want a random 32-hex token", claim.CreateID)
				}
				switch kind {
				case "different token":
					claim.CreateID = "another-creation"
				case "different volume":
					claim.VolumeID = "another-volume"
				case "different prefix":
					claim.Prefix = "another/"
				case "different bucket":
					claim.Bucket = "another-bucket"
				}
				seedClaims(t, store, claim)
				return failure
			}
			claimed, err := claimPrefix(context.Background(), wrapped, "user-bucket", "workspaces/repro/", "repro-volume")
			if claimed.CreatedThisCall || !errors.Is(err, failure) {
				t.Fatalf("claimed=%v err=%v, want original failure without adopting other ownership", claimed, err)
			}
			if len(registryOf(t, store).Claims) != 1 || store.writes != 1 {
				t.Fatal("recovery modified the other claim")
			}
		})
	}
}

func TestCreatePrefixFailureCleansOnlyRecoveredClaim(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "recovered new claim"
		if preexisting {
			name = "pre-existing claim"
		}
		t.Run(name, func(t *testing.T) {
			store := &fakeCreateStore{fakeStore: newFakeStore(), exists: true, createErr: errors.New("prefix write denied")}
			initial := []binding.Claim{{VolumeID: "outside-volume", Prefix: "outside/"}}
			if preexisting {
				initial = append(initial, binding.Claim{VolumeID: "repro-volume", Prefix: "workspaces/repro/"})
			}
			seedClaims(t, store.fakeStore, initial...)
			before := store.writes
			if !preexisting {
				store.commitThenErr = s3api.ErrPreconditionFailed
			}
			err := createDynamicPrefix(context.Background(), store, "user-bucket", "workspaces/repro/", "repro-volume")
			if err == nil || !strings.Contains(err.Error(), "prefix write denied") || store.prefixWrites != 1 {
				t.Fatalf("err=%v prefixWrites=%d, want the later prefix failure", err, store.prefixWrites)
			}
			claims := registryOf(t, store.fakeStore).Claims
			wantWrites := 2
			if preexisting {
				wantWrites = 0
			}
			if binding.HasClaim(claims, "workspaces/repro/", "repro-volume") != preexisting ||
				!binding.HasClaim(claims, "outside/", "outside-volume") || store.writes-before != wantWrites {
				t.Fatalf("claims=%+v writes=%d, want only the newly recovered claim released", claims, store.writes-before)
			}
		})
	}
}
