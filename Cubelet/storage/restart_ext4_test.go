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
	"testing"
	"time"

	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestExt4RepairAccepted(t *testing.T) {
	for _, code := range []int{1, 2, 3} {
		if !ext4RepairAccepted(code) {
			t.Fatalf("exit %d should trigger another check", code)
		}
	}
	for _, code := range []int{-1, 0, 4, 5, 8, 9, 12, 16} {
		if ext4RepairAccepted(code) {
			t.Fatalf("exit %d should not recheck", code)
		}
	}
	for _, code := range []int{4, 5, 6, 7, 12} {
		if !ext4RepairUnrecoverable(code) {
			t.Fatalf("exit %d leaves errors on the disk", code)
		}
	}
	for _, code := range []int{-1, 0, 1, 2, 3, 8, 16} {
		if ext4RepairUnrecoverable(code) {
			t.Fatalf("exit %d stays retryable", code)
		}
	}
}

func TestRetainedRootfsTargets(t *testing.T) {
	cow := &BackendFileInfo{VolumeName: "sb-1-rootfs-gen0", FilePath: "/dev/mapper/sb-1-rootfs-gen0"}
	other := &BackendFileInfo{VolumeName: "sb-2-rootfs-gen0", FilePath: "/dev/mapper/sb-2-rootfs-gen0"}
	plain := &BackendFileInfo{Type: "ext4", Medium: cubebox.StorageMedium_StorageMediumDefault, FilePath: "/dev/disk/by-id/pool"}
	pathOnly := &BackendFileInfo{FilePath: "/data/volumes/sb-1-rootfs-gen0"}
	memory := &BackendFileInfo{VolumeName: "memory", FilePath: "/dev/mapper/tpl-memory"}

	got, err := retainedRootfsTargets("1", map[string]*BackendFileInfo{
		"cow": cow, "other": other, "plain": plain, "path": pathOnly, "mem": memory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != cow {
		t.Fatalf("cubecow candidate = %+v", got)
	}

	got, err = retainedRootfsTargets("9", map[string]*BackendFileInfo{"plain": plain, "path": pathOnly, "mem": memory})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != plain {
		t.Fatalf("default ext4 = %+v", got)
	}

	_, err = retainedRootfsTargets("1", map[string]*BackendFileInfo{
		"a": cow,
		"b": {VolumeName: "sb-1-rootfs-gen1", FilePath: "/dev/mapper/other"},
	})
	if err == nil {
		t.Fatal("multiple cubecow candidates")
	}
}

func TestHasExt4Superblock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	buf := make([]byte, 0x438+2)
	buf[0x438] = 0x53
	buf[0x439] = 0xEF
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := hasExt4Superblock(path)
	if err != nil || !ok {
		t.Fatalf("magic ok=%v err=%v", ok, err)
	}
	buf[0x438] = 0
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = hasExt4Superblock(path)
	if err != nil || ok {
		t.Fatalf("non-ext4 ok=%v err=%v", ok, err)
	}
}

func TestRepairExt4AfterCrashRepeatsUntilClean(t *testing.T) {
	path := writeFakeExt4(t)
	var got [][]string
	parentDeadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()

	restore := stubExt4Fsck(t, func(passCtx context.Context, argv []string) (string, string, error) {
		got = append(got, append([]string(nil), argv...))
		dl, ok := passCtx.Deadline()
		if !ok || dl.After(parentDeadline) {
			t.Fatalf("pass deadline = %v ok=%v, parent = %v", dl, ok, parentDeadline)
		}
		switch len(got) {
		case 1:
			return "pass 1 corrected", "", exitStatus(1)
		case 2:
			return "clean", "", nil
		default:
			t.Fatalf("unexpected pass %d", len(got))
			return "", "", nil
		}
	})
	defer restore()

	if err := repairExt4AfterCrash(ctx, path); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("passes = %d, want 2", len(got))
	}
	for _, argv := range got {
		if len(argv) != 3 || argv[0] != "e2fsck" || argv[1] != "-fy" || argv[2] != path {
			t.Fatalf("argv = %v", argv)
		}
	}
}

func TestRepairExt4AfterCrashThirdPassCleansExit2(t *testing.T) {
	path := writeFakeExt4(t)
	codes := []int{2, 1, 0}
	n := 0
	restore := stubExt4Fsck(t, func(ctx context.Context, argv []string) (string, string, error) {
		code := codes[n]
		n++
		if code == 0 {
			return "clean", "", nil
		}
		return fmt.Sprintf("exit %d", code), "", exitStatus(code)
	})
	defer restore()

	if err := repairExt4AfterCrash(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("passes = %d, want 3", n)
	}
}

func TestRepairExt4AfterCrashExit3Rechecks(t *testing.T) {
	path := writeFakeExt4(t)
	codes := []int{3, 0}
	n := 0
	restore := stubExt4Fsck(t, func(ctx context.Context, argv []string) (string, string, error) {
		code := codes[n]
		n++
		if code == 0 {
			return "clean", "", nil
		}
		return "corrected and reboot", "", exitStatus(code)
	})
	defer restore()

	if err := repairExt4AfterCrash(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("passes = %d, want 2", n)
	}
}

func TestRepairExt4AfterCrashStopsOnUnrecoverable(t *testing.T) {
	for _, code := range []int{4, 12} {
		t.Run(fmt.Sprintf("exit_%d", code), func(t *testing.T) {
			path := writeFakeExt4(t)
			n := 0
			restore := stubExt4Fsck(t, func(ctx context.Context, argv []string) (string, string, error) {
				n++
				return "errors left", "operational", exitStatus(code)
			})
			defer restore()

			err := repairExt4AfterCrash(context.Background(), path)
			if !errors.Is(err, ErrExt4RepairUnrecoverable) {
				t.Fatalf("err = %v", err)
			}
			if n != 1 {
				t.Fatalf("passes = %d, want 1", n)
			}
		})
	}
}

func TestRepairExt4AfterCrashExit8StaysRetryable(t *testing.T) {
	path := writeFakeExt4(t)
	n := 0
	restore := stubExt4Fsck(t, func(ctx context.Context, argv []string) (string, string, error) {
		n++
		return "", "device busy", exitStatus(8)
	})
	defer restore()

	err := repairExt4AfterCrash(context.Background(), path)
	if err == nil || errors.Is(err, ErrExt4RepairUnrecoverable) {
		t.Fatalf("err = %v, want retryable error", err)
	}
	if n != 1 {
		t.Fatalf("passes = %d, want 1", n)
	}
}

func TestRepairExt4AfterCrashNonExitErrorStaysRetryable(t *testing.T) {
	path := writeFakeExt4(t)
	n := 0
	restore := stubExt4Fsck(t, func(ctx context.Context, argv []string) (string, string, error) {
		n++
		return "", "", errors.New("e2fsck not found")
	})
	defer restore()

	err := repairExt4AfterCrash(context.Background(), path)
	if err == nil || errors.Is(err, ErrExt4RepairUnrecoverable) {
		t.Fatalf("err = %v, want retryable error", err)
	}
	if n != 1 {
		t.Fatalf("passes = %d, want 1", n)
	}
}

func TestRepairExt4AfterCrashStopsWhenContextCanceled(t *testing.T) {
	path := writeFakeExt4(t)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	restore := stubExt4Fsck(t, func(passCtx context.Context, argv []string) (string, string, error) {
		n++
		cancel()
		return "corrected", "", exitStatus(1)
	})
	defer restore()

	err := repairExt4AfterCrash(ctx, path)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrExt4RepairUnrecoverable) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n != 1 {
		t.Fatalf("passes = %d, want 1", n)
	}
}

func TestRepairExt4AfterCrashGivesUpWhenNeverClean(t *testing.T) {
	path := writeFakeExt4(t)
	n := 0
	restore := stubExt4Fsck(t, func(ctx context.Context, argv []string) (string, string, error) {
		n++
		return "still dirty", "", exitStatus(1)
	})
	defer restore()

	err := repairExt4AfterCrash(context.Background(), path)
	if !errors.Is(err, ErrExt4RepairUnrecoverable) {
		t.Fatalf("err = %v", err)
	}
	if n != ext4RepairMaxPasses {
		t.Fatalf("passes = %d, want %d", n, ext4RepairMaxPasses)
	}
}

func TestRepairExt4AfterCrashSkipsNonExt4(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	buf := make([]byte, 0x438+2)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := stubExt4Fsck(t, func(ctx context.Context, argv []string) (string, string, error) {
		t.Fatal("e2fsck ran on a non-ext4 disk")
		return "", "", nil
	})
	defer restore()
	if err := repairExt4AfterCrash(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func writeFakeExt4(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	buf := make([]byte, 0x438+2)
	buf[0x438] = 0x53
	buf[0x439] = 0xEF
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubExt4Fsck(t *testing.T, fn func(context.Context, []string) (string, string, error)) func() {
	t.Helper()
	prev := runExt4Fsck
	runExt4Fsck = fn
	return func() { runExt4Fsck = prev }
}

func exitStatus(code int) error {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	err := cmd.Run()
	if err == nil {
		panic(fmt.Sprintf("exit %d returned nil", code))
	}
	return err
}

func TestRepairExt4AfterCrashRealCleanImage(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip(err.Error())
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Skip(err.Error())
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.img")
	if out, err := exec.Command("dd", "if=/dev/zero", "of="+path, "bs=1M", "count=8").CombinedOutput(); err != nil {
		t.Fatalf("dd: %v %s", err, out)
	}
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", path).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v %s", err, out)
	}
	if err := repairExt4AfterCrash(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	again := exec.Command("e2fsck", "-fn", path)
	out, err := again.CombinedOutput()
	if err != nil {
		t.Fatalf("disk not clean after repair: %v %s", err, out)
	}
}
