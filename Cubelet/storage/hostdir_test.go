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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
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

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = defaultRunHostDirCommand(ctx, shell, "-c", script)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if elapsed > 2*time.Second {
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

	deadline := time.Now().Add(time.Second)
	id := utils.ProcessIdentity{Pid: pid}
	for id.Status() != utils.LivenessGone {
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still alive after timeout", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestValidateHostDirSourceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "hostdir-0"},
		{name: "sdk-cold-start-host"},
		{name: "..", wantErr: true},
		{name: ".", wantErr: true},
		{name: "", wantErr: true},
		{name: "team/member", wantErr: true},
		{name: `team\member`, wantErr: true},
		{name: "/abs", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateHostDirSourceName(tt.name)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestRemoveEmptyHostDirTreeRefusesNonDirectoryEntries(t *testing.T) {
	base := t.TempDir()
	sandboxDir := filepath.Join(base, "sb-nonempty")
	volDir := filepath.Join(sandboxDir, "rw", "vol-0")
	require.NoError(t, os.MkdirAll(volDir, 0755))

	userFile := filepath.Join(volDir, "user-data.txt")
	require.NoError(t, os.WriteFile(userFile, []byte("precious"), 0644))

	err := removeEmptyHostDirTree(sandboxDir)
	require.Error(t, err)
	require.ErrorContains(t, err, "unexpected non-directory entry")
	require.FileExists(t, userFile)
}

func TestCleanupHostDirVolumesUnmountsBindAndPreservesSourceData(t *testing.T) {
	utils.SkipUnlessRootWithSysAdmin(t)

	tmp := t.TempDir()
	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = filepath.Join(tmp, "hostdir")
	require.NoError(t, os.MkdirAll(hostDirBasePath, 0755))
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	srcDir := filepath.Join(tmp, "user-source")
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "sub"), 0755))
	srcFile := filepath.Join(srcDir, "sub", "important.txt")
	require.NoError(t, os.WriteFile(srcFile, []byte("do-not-delete"), 0644))

	sandboxID := "bind-mount-sb"
	bindDest := filepath.Join(hostDirBasePath, sandboxID, "rw", "vol 0")
	require.NoError(t, os.MkdirAll(bindDest, 0755))

	require.NoError(t, unix.Mount(srcDir, bindDest, "", unix.MS_BIND|unix.MS_REC, ""))
	t.Cleanup(func() {
		_ = unix.Unmount(bindDest, unix.MNT_DETACH)
	})

	// Stack a second bind mount on the same target to verify stacked-mount cleanup.
	require.NoError(t, unix.Mount(srcDir, bindDest, "", unix.MS_BIND, ""))
	t.Cleanup(func() {
		_ = unix.Unmount(bindDest, unix.MNT_DETACH)
	})

	s := &local{}
	info := &StorageInfo{
		SandboxID: sandboxID,
		HostDirBackendInfos: map[string]*HostDirBackendInfo{
			"vol/vol 0": {
				VolumeName: "vol",
				ShareDir:   filepath.Dir(bindDest),
				BindPath:   bindDest,
			},
		},
	}

	require.NoError(t, s.cleanupHostDirVolumes(context.Background(), info))
	require.NoDirExists(t, filepath.Join(hostDirBasePath, sandboxID))
	require.FileExists(t, srcFile)

	// Calling cleanup a second time must be idempotent.
	require.NoError(t, s.cleanupHostDirVolumes(context.Background(), info))
}

func TestRemoveEmptyHostDirTreeRefusesActiveMountPoint(t *testing.T) {
	utils.SkipUnlessRootWithSysAdmin(t)

	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "source")
	require.NoError(t, os.MkdirAll(srcDir, 0755))
	srcFile := filepath.Join(srcDir, "keep.txt")
	require.NoError(t, os.WriteFile(srcFile, []byte("keep"), 0644))

	sandboxDir := filepath.Join(tmp, "hostdir", "sb-active-mnt")
	bindDest := filepath.Join(sandboxDir, "rw", "vol")
	require.NoError(t, os.MkdirAll(bindDest, 0755))

	require.NoError(t, unix.Mount(srcDir, bindDest, "", unix.MS_BIND, ""))
	t.Cleanup(func() {
		_ = unix.Unmount(bindDest, unix.MNT_DETACH)
	})

	// Even if removeEmptyHostDirTree is called while a mount is still active,
	// it must refuse to traverse/delete the mount and leave source data intact.
	err := removeEmptyHostDirTree(sandboxDir)
	require.Error(t, err)
	require.FileExists(t, srcFile)
}
