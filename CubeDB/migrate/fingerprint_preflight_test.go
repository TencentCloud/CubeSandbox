// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// fakeFingerprintStore serves canned fingerprint state so the preflight
// decision can be tested without a database (the other tests in this package
// need dockertest).
type fakeFingerprintStore struct {
	applied map[int64]bool
	stored  map[int64]storedFingerprint
}

func (s *fakeFingerprintStore) EnsureTable(context.Context, *sql.DB) error { return nil }

func (s *fakeFingerprintStore) LoadStored(context.Context, *sql.DB) (map[int64]storedFingerprint, error) {
	return s.stored, nil
}

func (s *fakeFingerprintStore) CurrentlyApplied(context.Context, *sql.DB) (map[int64]bool, error) {
	return s.applied, nil
}

func (s *fakeFingerprintStore) RecordOne(context.Context, *sql.DB, fileFingerprint) error {
	return nil
}

func fp(version int64, source, sum string) fileFingerprint {
	return fileFingerprint{version: version, source: source, sum: sum}
}

// An applied version whose file this binary does not carry must NOT be fatal.
//
// This is the shape produced by a database that was migrated by another build
// line, or by a newer image sharing the same database. Every component that
// migrates CubeDB (CubeMaster, CubeTemplateCenter) would otherwise refuse to
// start over state that cannot affect it.
func TestPreflightFingerprintsAbsentFileIsNotFatal(t *testing.T) {
	t.Setenv(skipFingerprintEnv, "")
	store := &fakeFingerprintStore{
		applied: map[int64]bool{1: true, 20260731120000: true, 20260805170000: true},
		stored: map[int64]storedFingerprint{
			1:              {sum: "aaa", source: "0001_baseline.sql"},
			20260731120000: {sum: "bbb", source: "20260731120000_soft_delete_purge_indexes.sql"},
			20260805170000: {sum: "ccc", source: "20260805170000_node_operation.sql"},
		},
	}
	fsFP := map[int64]fileFingerprint{
		1: fp(1, "0001_baseline.sql", "aaa"),
	}

	if err := preflightFingerprints(context.Background(), nil, fsFP, store); err != nil {
		t.Fatalf("an applied version missing from the tree must not fail startup, got: %v", err)
	}
}

// Content drift stays fatal: this is the case the check exists for.
func TestPreflightFingerprintsContentDriftIsFatal(t *testing.T) {
	t.Setenv(skipFingerprintEnv, "")
	store := &fakeFingerprintStore{
		applied: map[int64]bool{2: true},
		stored: map[int64]storedFingerprint{
			2: {sum: "recorded-sum", source: "0002_head.sql"},
		},
	}
	fsFP := map[int64]fileFingerprint{
		2: fp(2, "0002_head.sql", "on-disk-sum"),
	}

	err := preflightFingerprints(context.Background(), nil, fsFP, store)
	if err == nil {
		t.Fatal("edited migration content must fail loudly")
	}
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("expected ErrFingerprintMismatch, got: %v", err)
	}
	if !strings.Contains(err.Error(), "content changed") {
		t.Fatalf("error should name the drift, got: %v", err)
	}
	// The absent-file wording must no longer appear in a fatal error.
	if strings.Contains(err.Error(), "missing from the migrations tree") {
		t.Fatalf("absent files must not be reported as fatal, got: %v", err)
	}
}

// An absent file and real drift at the same time: the drift alone decides, and
// the absent version must not be listed as a reason.
func TestPreflightFingerprintsDriftReportedWithoutAbsentNoise(t *testing.T) {
	t.Setenv(skipFingerprintEnv, "")
	store := &fakeFingerprintStore{
		applied: map[int64]bool{2: true, 20260810120000: true},
		stored: map[int64]storedFingerprint{
			2:              {sum: "recorded", source: "0002_head.sql"},
			20260810120000: {sum: "zzz", source: "20260810120000_host_facts.sql"},
		},
	}
	fsFP := map[int64]fileFingerprint{
		2: fp(2, "0002_head.sql", "different"),
	}

	err := preflightFingerprints(context.Background(), nil, fsFP, store)
	if err == nil {
		t.Fatal("expected the drift to fail")
	}
	if strings.Contains(err.Error(), "host_facts") {
		t.Fatalf("absent version must not appear in the fatal error, got: %v", err)
	}
}

// A stored fingerprint for a version that is no longer applied (rolled back) is
// ignored, absent or not.
func TestPreflightFingerprintsIgnoresUnappliedVersions(t *testing.T) {
	t.Setenv(skipFingerprintEnv, "")
	store := &fakeFingerprintStore{
		applied: map[int64]bool{},
		stored: map[int64]storedFingerprint{
			3: {sum: "recorded", source: "0003_rolled_back.sql"},
		},
	}
	if err := preflightFingerprints(context.Background(), nil, map[int64]fileFingerprint{}, store); err != nil {
		t.Fatalf("unapplied stored rows must be ignored, got: %v", err)
	}
}

// The escape hatch still short-circuits everything, including real drift.
func TestPreflightFingerprintsSkipEnvBypassesDrift(t *testing.T) {
	t.Setenv(skipFingerprintEnv, "1")
	store := &fakeFingerprintStore{
		applied: map[int64]bool{2: true},
		stored: map[int64]storedFingerprint{
			2: {sum: "recorded", source: "0002_head.sql"},
		},
	}
	fsFP := map[int64]fileFingerprint{2: fp(2, "0002_head.sql", "different")}

	if err := preflightFingerprints(context.Background(), nil, fsFP, store); err != nil {
		t.Fatalf("skip env must bypass the check, got: %v", err)
	}
}

// No stored fingerprints at all (fresh database, or the layer never ran) is not
// an error.
func TestPreflightFingerprintsEmptyStoreIsFine(t *testing.T) {
	t.Setenv(skipFingerprintEnv, "")
	store := &fakeFingerprintStore{
		applied: map[int64]bool{1: true},
		stored:  map[int64]storedFingerprint{},
	}
	if err := preflightFingerprints(context.Background(), nil, map[int64]fileFingerprint{}, store); err != nil {
		t.Fatalf("an empty fingerprint table must not fail, got: %v", err)
	}
}
