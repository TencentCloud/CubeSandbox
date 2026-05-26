// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build cgo && cubecow_integration

package cubecow

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func testConfigPath(t *testing.T) string {
	t.Helper()
	cfg := os.Getenv("CUBECOW_TEST_CONFIG")
	if cfg == "" {
		t.Skip("CUBECOW_TEST_CONFIG is not set")
	}
	return cfg
}

func testConfigJSON(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(testConfigPath(t))
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	return string(raw)
}

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := InitWithoutLogging(testConfigPath(t))
	if err != nil {
		t.Fatalf("InitWithoutLogging failed: %v", err)
	}
	return engine
}

func TestInitWithoutLoggingFromJSON(t *testing.T) {
	engine, err := InitWithoutLoggingFromJSON(testConfigJSON(t))
	if err != nil {
		t.Fatalf("InitWithoutLoggingFromJSON failed: %v", err)
	}
	engine.Close()
}

func TestFullLifecycle(t *testing.T) {
	engine := newTestEngine(t)
	defer engine.Close()

	baseName := fmt.Sprintf("sdk-it-%d", time.Now().UnixNano())
	volumeName := baseName + "-vol"
	snapshotName := baseName + "-snap"

	devicePath, err := engine.CreateVolume(volumeName, 1<<30)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if devicePath == "" {
		t.Fatal("CreateVolume returned empty device path")
	}
	defer func() { _ = engine.DeleteVolume(volumeName) }()

	info, err := engine.GetVolumeInfo(volumeName)
	if err != nil {
		t.Fatalf("GetVolumeInfo failed: %v", err)
	}
	if info.DevicePath == "" {
		t.Fatal("GetVolumeInfo returned empty device path")
	}

	oldSize, newSize, err := engine.ResizeVolume(volumeName, info.SizeBytes+(256<<20))
	if err != nil {
		t.Fatalf("ResizeVolume failed: %v", err)
	}
	if newSize <= oldSize {
		t.Fatalf("ResizeVolume did not expand: old=%d new=%d", oldSize, newSize)
	}

	snapshotDev, err := engine.CreateSnapshot(volumeName, snapshotName, true)
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}
	if snapshotDev == "" {
		t.Fatal("CreateSnapshot returned empty device path")
	}
	defer func() { _ = engine.DeleteSnapshot(snapshotName) }()

	snapshots, err := engine.ListSnapshots(volumeName, 0, "")
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}
	if len(snapshots.Snapshots) == 0 {
		t.Fatal("ListSnapshots returned no snapshots")
	}

	metrics, err := engine.GetMetrics()
	if err != nil {
		t.Fatalf("GetMetrics failed: %v", err)
	}
	for _, key := range []string{"volume_count", "snapshot_count", "total_bytes", "used_bytes"} {
		if _, ok := metrics[key]; !ok {
			t.Fatalf("GetMetrics missing key %q", key)
		}
	}

	if err := engine.DeleteSnapshot(snapshotName); err != nil {
		t.Fatalf("DeleteSnapshot failed: %v", err)
	}
	if err := engine.DeleteVolume(volumeName); err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}
}

func TestErrorInjectionNotFound(t *testing.T) {
	engine := newTestEngine(t)
	defer engine.Close()

	err := engine.DeleteVolume(fmt.Sprintf("sdk-missing-%d", time.Now().UnixNano()))
	if err == nil {
		t.Fatal("expected DeleteVolume to fail for missing volume")
	}
	cerr, ok := err.(*CowError)
	if !ok {
		t.Fatalf("expected CowError, got %T", err)
	}
	if cerr.Code != SemNotFound {
		t.Fatalf("unexpected code: %v", cerr.Code)
	}
}

func TestErrorInjectionAlreadyExists(t *testing.T) {
	engine := newTestEngine(t)
	defer engine.Close()

	name := fmt.Sprintf("sdk-dup-%d", time.Now().UnixNano())
	if _, err := engine.CreateVolume(name, 1<<30); err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	defer func() { _ = engine.DeleteVolume(name) }()

	_, err := engine.CreateVolume(name, 1<<30)
	if err == nil {
		t.Fatal("expected second CreateVolume to fail")
	}
	cerr, ok := err.(*CowError)
	if !ok {
		t.Fatalf("expected CowError, got %T", err)
	}
	if cerr.Code != SemAlreadyExists {
		t.Fatalf("unexpected code: %v", cerr.Code)
	}
}

func TestConcurrent(t *testing.T) {
	engine := newTestEngine(t)
	defer engine.Close()

	var (
		wg    sync.WaitGroup
		errCh = make(chan error, 10)
	)

	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("sdk-concurrent-%d-%d", time.Now().UnixNano(), i)
			if _, err := engine.CreateVolume(name, 1<<29); err != nil {
				errCh <- fmt.Errorf("CreateVolume(%s): %w", name, err)
				return
			}
			defer func() { _ = engine.DeleteVolume(name) }()
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}
