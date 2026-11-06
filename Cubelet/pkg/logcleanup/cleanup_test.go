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

func TestPreparePreservesEarlierLogsAndResetsDeadline(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stdout", "stderr"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old-"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Mark(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old deadline remains: %v", err)
	}
	if ns, err := Namespace(root, "sandbox"); err != nil || ns != "default" {
		t.Fatalf("pending namespace %q: %v", ns, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, historyDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("history: %v, %v", entries, err)
	}
	for _, name := range []string{"stdout", "stderr"} {
		data, err := os.ReadFile(filepath.Join(dir, historyDir, entries[0].Name(), name))
		if err != nil || string(data) != "old-"+name {
			t.Fatalf("history %s = %q: %v", name, data, err)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("current %s still exists: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "stdout")); err != nil || string(data) != "new" {
		t.Fatalf("new output: %q, %v", data, err)
	}
}

func TestPrepareKeepsHistoryAccessibleLikeTheSandboxDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Mark(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, historyDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("history: %v, %v", entries, err)
	}
	history, err := os.Stat(filepath.Join(dir, historyDir))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := os.Stat(filepath.Join(dir, historyDir, entries[0].Name()))
	if err != nil || generation.Mode().Perm() != history.Mode().Perm() {
		t.Fatalf("history generation permissions differ from history directory: %v", err)
	}
}

func TestPreparePreservesUnmarkedLegacyLogsOutsideScan(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("legacy evidence"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, legacyDirName, "sandbox"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("legacy history: %v, %v", entries, err)
	}
	legacy := filepath.Join(root, legacyDirName, "sandbox", entries[0].Name(), "logs", "stdout")
	if data, err := os.ReadFile(legacy); err != nil || string(data) != "legacy evidence" {
		t.Fatalf("legacy output: %q, %v", data, err)
	}
	if err := Mark(root, "sandbox", "default"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	marker := filepath.Join(root, "sandbox", markerName)
	if err := os.Chtimes(marker, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Scan(context.Background(), root, time.Hour, now, func(_ context.Context, _ string, remove func() error) error { return remove() }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new generation was not removed: %v", err)
	}
	if data, err := os.ReadFile(legacy); err != nil || string(data) != "legacy evidence" {
		t.Fatalf("legacy evidence changed: %q, %v", data, err)
	}
}

func TestScanStartsRetentionForPendingLogsOnlyAfterGuard(t *testing.T) {
	root := t.TempDir()
	if err := Prepare(root, "pending", "default"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "legacy"), 0700); err != nil {
		t.Fatal(err)
	}
	busy := true
	guard := func(_ context.Context, id string, remove func() error) error {
		if busy {
			return nil
		}
		return remove()
	}
	now := time.Now()
	if err := Scan(context.Background(), root, time.Hour, now, guard); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "pending", markerName)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active sandbox was marked: %v", err)
	}
	busy = false
	if err := Scan(context.Background(), root, time.Hour, now, guard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("pending logs not marked after exit: %v", err)
	}
	if err := os.Chtimes(marker, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Scan(context.Background(), root, time.Hour, now, guard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired pending logs not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "legacy")); err != nil {
		t.Fatalf("legacy unmarked logs changed: %v", err)
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
	for _, id := range []string{"", ".", legacyDirName, "../outside", "a/b"} {
		if err := Mark(root, id, "default"); err == nil {
			t.Fatalf("Mark accepted %q", id)
		}
	}
}

func TestMarkReportsMissingDirectory(t *testing.T) {
	err := Mark(t.TempDir(), "sandbox", "default")
	if !errors.Is(err, ErrLogDirNotFound) {
		t.Fatalf("expected missing log directory, got %v", err)
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
