// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package logcleanup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanOnlyRemovesExpiredAndSafeLogs(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"expired", "recent", "active", "unmarked"} {
		dir := filepath.Join(root, id)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("log"), 0600); err != nil {
			t.Fatal(err)
		}
		if id != "unmarked" {
			if err := Mark(root, id, "default"); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now()
	for _, id := range []string{"expired", "active"} {
		old := now.Add(-2 * time.Hour)
		if err := os.Chtimes(filepath.Join(root, id, markerName), old, old); err != nil {
			t.Fatal(err)
		}
	}
	guard := func(_ context.Context, id string, remove func() error) error {
		if id == "active" {
			return nil
		}
		return remove()
	}
	if err := Scan(context.Background(), root, time.Hour, now, guard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "expired")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired directory should be removed, got %v", err)
	}
	for _, id := range []string{"recent", "active", "unmarked"} {
		if _, err := os.Stat(filepath.Join(root, id, "stdout")); err != nil {
			t.Fatalf("%s log should remain: %v", id, err)
		}
	}
}

func TestScanRechecksMarkerAfterAcquiringGuard(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Mark(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, markerName), old, old); err != nil {
		t.Fatal(err)
	}
	guard := func(_ context.Context, id string, remove func() error) error {
		if err := Mark(root, id, "default"); err != nil {
			return err
		}
		return remove()
	}
	if err := Scan(context.Background(), root, time.Hour, now, guard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("refreshed logs should remain: %v", err)
	}
}

func TestMarkRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", ".", "../outside", "a/b"} {
		if err := Mark(root, id, "default"); err == nil {
			t.Fatalf("Mark accepted %q", id)
		}
	}
}

func TestScanKeepsLogsWhenSafetyCheckFails(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Mark(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(2 * time.Hour)
	guard := func(context.Context, string, func() error) error {
		return errors.New("runtime state unknown")
	}
	if err := Scan(context.Background(), root, time.Hour, now, guard); err == nil {
		t.Fatal("expected safety check error")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("logs must remain when runtime state is unknown: %v", err)
	}
}

func TestScanDoesNotFollowSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "stdout"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sandbox")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := Mark(root, "sandbox", "default"); err == nil {
		t.Fatal("Mark accepted a symlink as a log directory")
	}
	if err := Scan(context.Background(), root, time.Hour, time.Now().Add(2*time.Hour),
		func(_ context.Context, _ string, remove func() error) error { return remove() }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "stdout")); err != nil {
		t.Fatalf("outside data must remain: %v", err)
	}
}

func TestMarkerNamespaceAndLegacyMarker(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Mark(root, "sandbox", "nondefault"); err != nil {
		t.Fatal(err)
	}
	ns, err := Namespace(root, "sandbox")
	if err != nil || ns != "nondefault" {
		t.Fatalf("namespace=%q error=%v", ns, err)
	}
	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Namespace(root, "sandbox"); err == nil {
		t.Fatal("legacy empty marker must fail closed")
	}
	if err := Mark(root, "sandbox", "../invalid"); err == nil {
		t.Fatal("invalid namespace accepted")
	}
}

func TestNamespaceDoesNotFollowMarkerSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("default"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, markerName)); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := Namespace(root, "sandbox"); err == nil {
		t.Fatal("marker symlink accepted")
	}
}
