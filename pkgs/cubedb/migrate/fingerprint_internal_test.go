// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

// TestAppliedResults_PartialError proves we recover the successfully-applied
// migrations from goose's PartialError (where Up returns nil results), so a
// partial/interrupted run still fingerprints what actually applied.
func TestAppliedResults_PartialError(t *testing.T) {
	mkResult := func(v int64) *goose.MigrationResult {
		return &goose.MigrationResult{Source: &goose.Source{Version: v}}
	}

	t.Run("success returns results", func(t *testing.T) {
		results := []*goose.MigrationResult{mkResult(1), mkResult(2)}
		got := appliedResults(results, nil)
		if len(got) != 2 {
			t.Fatalf("expected 2 applied, got %d", len(got))
		}
	})

	t.Run("partial error returns Applied", func(t *testing.T) {
		applied := []*goose.MigrationResult{mkResult(1)}
		pErr := &goose.PartialError{
			Applied: applied,
			Failed:  mkResult(2),
			Err:     fmt.Errorf("boom"),
		}
		got := appliedResults(nil, pErr)
		if len(got) != 1 || got[0].Source.Version != 1 {
			t.Fatalf("expected version 1 from PartialError.Applied, got %+v", got)
		}
	})

	t.Run("wrapped partial error is unwrapped", func(t *testing.T) {
		applied := []*goose.MigrationResult{mkResult(7)}
		pErr := &goose.PartialError{Applied: applied, Failed: mkResult(8), Err: fmt.Errorf("x")}
		wrapped := fmt.Errorf("migrate: %w", pErr)
		got := appliedResults(nil, wrapped)
		if len(got) != 1 || got[0].Source.Version != 7 {
			t.Fatalf("expected version 7 from wrapped PartialError, got %+v", got)
		}
	})

	t.Run("non-partial error returns results as-is", func(t *testing.T) {
		results := []*goose.MigrationResult{mkResult(3)}
		got := appliedResults(results, fmt.Errorf("some other error"))
		if len(got) != 1 || got[0].Source.Version != 3 {
			t.Fatalf("expected version 3, got %+v", got)
		}
	})
}

// TestCollectFSFingerprints_RealMigrations verifies each embedded migration set
// is fingerprintable and deterministic (no docker required).
func TestCollectFSFingerprints_RealMigrations(t *testing.T) {
	for _, dialect := range []string{"mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			spec, ok := dialectSpecs[dialect]
			if !ok {
				t.Fatalf("missing dialectSpecs[%q]", dialect)
			}
			subFS, err := fs.Sub(spec.rootFS, spec.subdir)
			if err != nil {
				t.Fatalf("fs.Sub: %v", err)
			}

			fp, err := collectFSFingerprints(subFS)
			if err != nil {
				t.Fatalf("collectFSFingerprints: %v", err)
			}
			if len(fp) == 0 {
				t.Fatal("expected at least one migration fingerprint")
			}
			// The frozen baseline must be present and parsed as version 1.
			if _, ok := fp[1]; !ok {
				t.Errorf("missing fingerprint for baseline version 1")
			}
			for v, f := range fp {
				if f.version != v {
					t.Errorf("version key %d != fingerprint.version %d", v, f.version)
				}
				if len(f.sum) != 64 {
					t.Errorf("version %d: sha256 hex must be 64 chars, got %d", v, len(f.sum))
				}
			}

			// Deterministic: a second pass yields identical sums.
			again, err := collectFSFingerprints(subFS)
			if err != nil {
				t.Fatalf("collectFSFingerprints (2nd): %v", err)
			}
			for v, f := range fp {
				if again[v].sum != f.sum {
					t.Errorf("version %d sum not deterministic: %s vs %s", v, f.sum, again[v].sum)
				}
			}
		})
	}
}

// TestCollectFSFingerprints_DuplicateVersion ensures two files sharing one
// integer version are rejected (mirrors goose's duplicate-version guard).
func TestCollectFSFingerprints_DuplicateVersion(t *testing.T) {
	mem := fstest.MapFS{
		"0007_alpha.sql": {Data: []byte("-- a")},
		"0007_beta.sql":  {Data: []byte("-- b")},
	}
	if _, err := collectFSFingerprints(mem); err == nil {
		t.Fatal("expected duplicate-version error, got nil")
	}
}

// TestCollectFSFingerprints_IgnoresNonMigrations confirms README/non-versioned
// files are skipped rather than failing collection.
func TestCollectFSFingerprints_IgnoresNonMigrations(t *testing.T) {
	mem := fstest.MapFS{
		"README.md":            {Data: []byte("# docs")},
		"helpers.txt":          {Data: []byte("noise")},
		"20260622143000_x.sql": {Data: []byte("-- x")},
	}
	fp, err := collectFSFingerprints(mem)
	if err != nil {
		t.Fatalf("collectFSFingerprints: %v", err)
	}
	if len(fp) != 1 {
		t.Fatalf("expected exactly 1 fingerprint, got %d", len(fp))
	}
	if _, ok := fp[20260622143000]; !ok {
		t.Errorf("expected timestamp version 20260622143000 to be collected")
	}
}

// TestLogUnprotectedVersions covers both branches of logUnprotectedVersions.
func TestLogUnprotectedVersions(t *testing.T) {
	t.Run("all protected, no output", func(t *testing.T) {
		var buf bytes.Buffer
		oldWriter := log.Writer()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(oldWriter) })

		applied := map[int64]bool{1: true, 2: true}
		stored := map[int64]storedFingerprint{
			1: {sum: "aa", source: "1.sql"},
			2: {sum: "bb", source: "2.sql"},
		}
		logUnprotectedVersions(applied, stored)
		if buf.Len() > 0 {
			t.Errorf("expected no output, got: %s", buf.String())
		}
	})

	t.Run("unprotected versions present, logs warning", func(t *testing.T) {
		var buf bytes.Buffer
		oldWriter := log.Writer()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(oldWriter) })

		applied := map[int64]bool{1: true, 2: true, 3: true}
		stored := map[int64]storedFingerprint{
			1: {sum: "aa", source: "1.sql"},
		}
		logUnprotectedVersions(applied, stored)
		out := buf.String()
		if !strings.Contains(out, "NOT content-checked") {
			t.Errorf("expected warning about unprotected versions, got: %s", out)
		}
		if !strings.Contains(out, "2") || !strings.Contains(out, "3") {
			t.Errorf("expected versions 2 and 3 to be listed, got: %s", out)
		}
	})

	t.Run("all versions unprotected, logs warning", func(t *testing.T) {
		var buf bytes.Buffer
		oldWriter := log.Writer()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(oldWriter) })

		applied := map[int64]bool{1: true, 2: true, 3: true}
		stored := map[int64]storedFingerprint{} // nothing fingerprinted
		logUnprotectedVersions(applied, stored)
		out := buf.String()
		if !strings.Contains(out, "NOT content-checked") {
			t.Errorf("expected warning about unprotected versions, got: %s", out)
		}
	})
}
