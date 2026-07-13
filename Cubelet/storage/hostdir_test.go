// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBindHostDirRW(t *testing.T) {
	orig := runHostDirCommand
	t.Cleanup(func() {
		runHostDirCommand = orig
	})

	var calls [][]string
	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		row := append([]string{name}, args...)
		calls = append(calls, row)
		return nil
	}

	if err := bindHostDir(context.Background(), "/host/path", "/bind/dest", false); err != nil {
		t.Fatalf("bindHostDir returned error: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(calls))
	}
	got := strings.Join(calls[0], " ")
	want := "/usr/bin/mount --rbind /host/path /bind/dest"
	if got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}
}

func TestBindHostDirReadOnlyAddsRemount(t *testing.T) {
	orig := runHostDirCommand
	t.Cleanup(func() {
		runHostDirCommand = orig
	})

	var calls [][]string
	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		row := append([]string{name}, args...)
		calls = append(calls, row)
		return nil
	}

	if err := bindHostDir(context.Background(), "/host/path", "/bind/dest", true); err != nil {
		t.Fatalf("bindHostDir returned error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(calls))
	}

	gotBind := strings.Join(calls[0], " ")
	wantBind := "/usr/bin/mount --rbind /host/path /bind/dest"
	if gotBind != wantBind {
		t.Fatalf("unexpected bind command: got %q want %q", gotBind, wantBind)
	}

	gotRemount := strings.Join(calls[1], " ")
	wantRemount := "/usr/bin/mount -o remount,bind,ro /bind/dest"
	if gotRemount != wantRemount {
		t.Fatalf("unexpected remount command: got %q want %q", gotRemount, wantRemount)
	}
}

func TestBindHostDirTimeout(t *testing.T) {
	orig := runHostDirCommand
	t.Cleanup(func() {
		runHostDirCommand = orig
	})

	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		return context.DeadlineExceeded
	}

	err := bindHostDir(context.Background(), "/host/path", "/bind/dest", false)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout message, got %v", err)
	}
}

func TestBindHostDirCanceled(t *testing.T) {
	orig := runHostDirCommand
	t.Cleanup(func() {
		runHostDirCommand = orig
	})

	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		return context.Canceled
	}

	err := bindHostDir(context.Background(), "/host/path", "/bind/dest", false)
	if err == nil {
		t.Fatal("expected canceled error, got nil")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled message, got %v", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("did not expect timeout message, got %v", err)
	}
}

func TestBindHostDirMountError(t *testing.T) {
	orig := runHostDirCommand
	t.Cleanup(func() {
		runHostDirCommand = orig
	})

	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		return errors.New("mount failed")
	}

	err := bindHostDir(context.Background(), "/host/path", "/bind/dest", false)
	if err == nil {
		t.Fatal("expected mount error, got nil")
	}
	if !strings.Contains(err.Error(), "mount failed") {
		t.Fatalf("expected wrapped mount error, got %v", err)
	}
}

func TestBindHostDirReadOnlyRemountCanceled(t *testing.T) {
	orig := runHostDirCommand
	t.Cleanup(func() {
		runHostDirCommand = orig
	})

	callCount := 0
	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		callCount++
		if callCount == 1 {
			return nil
		}
		return context.Canceled
	}

	err := bindHostDir(context.Background(), "/host/path", "/bind/dest", true)
	if err == nil {
		t.Fatal("expected canceled remount error, got nil")
	}
	if !strings.Contains(err.Error(), "remount ro /bind/dest canceled") {
		t.Fatalf("expected remount canceled message, got %v", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("did not expect timeout message, got %v", err)
	}
}

func TestDefaultRunHostDirCommandTimeoutKillsProcessGroup(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}

	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	script := fmt.Sprintf("sleep 10 & child=$!; printf '%%s' \"$child\" > %q; wait \"$child\"", pidFile)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = defaultRunHostDirCommand(ctx, shell, "-c", script)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("expected timeout to return promptly, took %s", elapsed)
	}

	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid file: %v", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", strings.TrimSpace(string(rawPID)), err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		killErr := syscall.Kill(pid, 0)
		if killErr != nil {
			if errors.Is(killErr, syscall.ESRCH) {
				return
			}
			t.Fatalf("probe child pid %d: %v", pid, killErr)
		}

		if time.Now().After(deadline) {
			t.Fatalf("child process %d still alive after timeout", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
