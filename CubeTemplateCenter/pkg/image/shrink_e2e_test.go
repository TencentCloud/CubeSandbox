// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCreateExt4ImageShrinkAndAlign(t *testing.T) {
	requireExt4Tools(t)
	t.Setenv("CUBEMASTER_EXT4_FIXED_OVERHEAD_MIB", "256")
	t.Setenv("CUBEMASTER_EXT4_OVERHEAD_PERCENT", "10")
	tmpDir := t.TempDir()
	rootfsDir := filepath.Join(tmpDir, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfsDir, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "etc", "test.txt"), []byte("hello cubesandbox"), 0o644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	ext4Path := filepath.Join(tmpDir, "output.ext4")
	if err := createExt4Image(context.Background(), rootfsDir, ext4Path); err != nil {
		t.Fatalf("createExt4Image failed: %v", err)
	}
	assertCompactExt4(t, ext4Path, 256*1024*1024)
}

func requireExt4Tools(t *testing.T) {
	t.Helper()
	for _, cmd := range []string{"mkfs.ext4", "resize2fs", "e2fsck", "truncate"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("command %s not found in PATH, skipping", cmd)
		}
	}
}

func TestCreateExt4ImageFileHeavyRootfs(t *testing.T) {
	requireExt4Tools(t)
	t.Setenv("CUBEMASTER_EXT4_FIXED_OVERHEAD_MIB", "256")
	t.Setenv("CUBEMASTER_EXT4_OVERHEAD_PERCENT", "10")
	tmpDir := t.TempDir()
	rootfsDir := filepath.Join(tmpDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	// A 512 MiB scratch image has only 32,768 inodes with the default
	// mke2fs configuration, despite having ample space for these empty files.
	var batchDir string
	for i := 0; i < 40000; i++ {
		if i%1000 == 0 {
			batchDir = filepath.Join(rootfsDir, fmt.Sprintf("batch-%02d", i/1000))
			if err := os.Mkdir(batchDir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(batchDir, fmt.Sprintf("file-%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ext4Path := filepath.Join(tmpDir, "output.ext4")
	if err := createExt4Image(context.Background(), rootfsDir, ext4Path); err != nil {
		t.Fatalf("createExt4Image failed: %v", err)
	}
	assertCompactExt4(t, ext4Path, 1024*1024*1024)
}

func assertCompactExt4(t *testing.T, ext4Path string, sizeLimit int64) {
	t.Helper()
	fi, err := os.Stat(ext4Path)
	if err != nil {
		t.Fatalf("stat output.ext4 failed: %v", err)
	}

	const twoMiB = int64(2 * 1024 * 1024)
	if fi.Size()%twoMiB != 0 {
		t.Fatalf("ext4 size %d is not 2MiB aligned (remainder %d)", fi.Size(), fi.Size()%twoMiB)
	}

	// Each fixture must shrink below its limit without sacrificing filesystem integrity.
	if fi.Size() >= sizeLimit {
		t.Fatalf("ext4 size %d was not shrunk below %d bytes", fi.Size(), sizeLimit)
	}

	f, err := os.Open(ext4Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var blockSizeLog [4]byte
	// s_log_block_size is a little-endian uint32 at offset 24 in the superblock.
	if _, err := f.ReadAt(blockSizeLog[:], 1024+24); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(blockSizeLog[:]); got != 2 {
		t.Fatalf("ext4 block size exponent = %d, want 2 (4096-byte blocks)", got)
	}
	t.Logf("Compact ext4 generated: %.1f MiB, 2 MiB aligned", float64(fi.Size())/float64(1024*1024))

	cmd := exec.Command("e2fsck", "-fn", ext4Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("e2fsck failed on generated image: %v, output: %s", err, string(out))
	}
}

func TestExt4ChecksExitCodes(t *testing.T) {
	for _, exitCode := range []int{0, 1, 2, 4, 8} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			binDir := t.TempDir()
			t.Setenv("PATH", binDir)
			installFakeCommand(t, binDir, "e2fsck", fmt.Sprintf("exit %d", exitCode))
			err := runE2fsck(context.Background(), "unused.ext4")
			if (err == nil) != (exitCode <= 2) {
				t.Fatalf("repair exit %d: unexpected error %v", exitCode, err)
			}
			err = verifyExt4Image(context.Background(), "unused.ext4")
			if (err == nil) != (exitCode == 0) {
				t.Fatalf("read-only verification exit %d: unexpected error %v", exitCode, err)
			}
		})
	}
}

func TestExt4BuildRejectsPostShrinkInconsistency(t *testing.T) {
	for _, path := range []string{"directory", "streaming"} {
		t.Run(path, func(t *testing.T) {
			binDir, tracePath, ext4Path := setupStreamingFakes(t)
			installFakeCommand(t, binDir, "losetup", `case "$1" in --find) echo /dev/loop9 ;; esac`)
			installFakeCommand(t, binDir, "truncate", `echo "truncate $*" >> "$FAKE_TRACE"
printf data > "$3"`)
			installFakeCommand(t, binDir, "e2fsck", `echo "e2fsck $*" >> "$FAKE_TRACE"
case "$1" in -fn) exit 4 ;; esac`)
			var err error
			if path == "directory" {
				ext4Path = filepath.Join(t.TempDir(), "output.ext4")
				err = createExt4Image(context.Background(), t.TempDir(), ext4Path)
			} else {
				err = runStreamingBuild(t, ext4Path)
			}
			if err == nil || !strings.Contains(err.Error(), "post-truncate e2fsck failed") {
				t.Fatalf("expected final verification failure, got %v", err)
			}
			lines := traceLines(t, tracePath)
			if traceIndex(lines, "mkfs.ext4 -F -b 4096") < 0 {
				t.Fatalf("mkfs must explicitly use 4KiB blocks: %v", lines)
			}
		})
	}
}
