// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// cmdTimeout is the per-command timeout for utils.ExecV calls in this
// package (cp, truncate, e2fsck, resize2fs, mkfs.ext4, ...). The
// default of 3s is fine for the small pre-formatted images on the
// pool fast path; the live `mkfs + reflink-copy + e2fsck + resize2fs`
// slow path on multi-GiB images can need longer. Override via the
// storage plugin config `cmd_timeout` (see Config.CmdTimeout); set
// during plugin initialization.
var cmdTimeout = 3 * time.Second

const diskSizeOverheadInBytes = 1024 * 1024 * 100

const otherSizeOverheadInBytesAbove1Gi = 1024 * 1024 * 1024

const diskSizeExtendInBytes = 1024 * 1024 * 5

func newExt4BaseRaw(filePath, uuid string, size int64) error {
	ok, _ := utils.FileExistAndValid(filePath)
	if ok {
		return nil
	}

	_ = os.RemoveAll(path.Clean(filePath))
	tmpTarget := path.Join(path.Dir(filePath), "tmpmount")
	defer func() {
		_ = os.RemoveAll(path.Clean(tmpTarget))
	}()
	size = size + diskSizeOverheadInBytes
	cmds := [][]string{
		{"mkdir", "-p", tmpTarget},
		{"touch", filePath},
		{"truncate", "-s", fmt.Sprintf("%d", size), filePath},
		{"mkfs.ext4", "-O", "^has_journal", "-U", uuid, filePath},
		{"mount", filePath, tmpTarget},
		{"mkdir", "-p", path.Join(tmpTarget, emptyDirInnerSourcePath)},
		{"mkdir", "-p", path.Join(tmpTarget, "containerd")},
		{"umount", tmpTarget},
	}
	for _, cmd := range cmds {
		if _, stderr, err := utils.ExecV(cmd, cmdTimeout); err != nil {
			return fmt.Errorf("newBaseRaw failed:%s", stderr)
		}
	}
	ok, err := utils.FileExistAndValid(filePath)
	if !ok {
		return fmt.Errorf("newBaseRaw failed:%s", err)
	}
	return nil
}

func initExt4BlockDevice(devicePath string) (err error) {
	mountDir, err := os.MkdirTemp("", "cube-cubecow-mount-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(mountDir)
	}()

	cmds := [][]string{
		{"mkfs.ext4", "-F", "-O", "^has_journal", devicePath},
		{"mount", devicePath, mountDir},
		{"mkdir", "-p", path.Join(mountDir, emptyDirInnerSourcePath)},
		{"mkdir", "-p", path.Join(mountDir, "containerd")},
		{"umount", mountDir},
	}

	for _, cmd := range cmds {
		if _, stderr, execErr := utils.ExecV(cmd, cmdTimeout); execErr != nil {
			err = fmt.Errorf("initExt4BlockDevice failed:%s", stderr)
			if cmd[0] != "umount" {
				_, _, _ = utils.ExecV([]string{"umount", mountDir}, cmdTimeout)
			}
			return err
		}
	}
	return nil
}

func newExt4RawByCopy(baseFormatFile, targetFile string, size int64) (err error) {
	cmds := [][]string{
		{"cp", baseFormatFile, targetFile},
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(targetFile)
		} else {
			ok, _ := utils.FileExistAndValid(targetFile)
			if !ok {
				err = fmt.Errorf("newExt4RawByCopy failed:%s", err)
				_ = os.RemoveAll(targetFile)
				return
			}
		}
	}()

	if size != 0 {
		size = size + otherSizeOverheadInBytesAbove1Gi
		cmds = append(cmds, []string{"truncate", "-s", fmt.Sprintf("%d", size), targetFile})
		cmds = append(cmds, []string{"e2fsck", "-fy", targetFile})
		cmds = append(cmds, []string{"resize2fs", targetFile})
	}
	for _, cmd := range cmds {
		var stderr, stdout string
		if stdout, stderr, err = utils.ExecV(cmd, cmdTimeout); err != nil {
			return fmt.Errorf("newExt4RawByCopy failed:%s, %v", stderr, stdout)
		}
	}
	return nil
}

func newExt4RawByReflinkCopy(baseFormatFile, targetFile string, size int64) (err error) {
	cmds := [][]string{
		{"cp", "--reflink=always", baseFormatFile, targetFile},
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(targetFile)
		} else {
			ok, _ := utils.FileExistAndValid(targetFile)
			if !ok {
				err = fmt.Errorf("newExt4RawByReflinkCopy failed:%s", err)
				_ = os.RemoveAll(targetFile)
				return
			}
		}
	}()

	if size != 0 {
		size = size + otherSizeOverheadInBytesAbove1Gi
		cmds = append(cmds, []string{"truncate", "-s", fmt.Sprintf("%d", size), targetFile})
		cmds = append(cmds, []string{"e2fsck", "-fy", targetFile})
		cmds = append(cmds, []string{"resize2fs", targetFile})
	}
	started := time.Now()
	for i, cmd := range cmds {
		var stderr string
		if _, stderr, err = utils.ExecV(cmd, cmdTimeout); err != nil {
			return fmt.Errorf("newExt4RawByReflinkCopy failed:%s%s",
				stderr,
				describeStorageFailure(cmds, i, targetFile, baseFormatFile, started))
		}
	}
	return nil
}

// describeStorageFailure builds a single-line diagnostic suffix for
// failures in the ext4-create command chain. Returned string starts
// with " [" so callers append it directly after the existing
// "<fnname> failed:<stderr>" prefix and the resulting message stays
// on one line.
//
// Format:
//
//	[step=N/M cmd="<argv>" elapsed=<dur> target=<stat> base=<stat> free=<bytes>B]
//
// Best-effort: stat / statfs errors are reported inline ("missing",
// "stat err=<msg>") instead of failing the diagnostic itself; the
// caller already has a real error to return.
func describeStorageFailure(cmds [][]string, idx int, target, base string, started time.Time) string {
	var b strings.Builder
	b.WriteString(" [step=")
	fmt.Fprintf(&b, "%d/%d", idx+1, len(cmds))
	fmt.Fprintf(&b, " cmd=%q", strings.Join(cmds[idx], " "))
	fmt.Fprintf(&b, " elapsed=%s", time.Since(started).Truncate(time.Millisecond))
	fmt.Fprintf(&b, " target=%s", describeFile(target))
	fmt.Fprintf(&b, " base=%s", describeFile(base))
	fmt.Fprintf(&b, " free=%s", describeFreeBytes(path.Dir(target)))
	b.WriteString("]")
	return b.String()
}

func describeFile(p string) string {
	if p == "" {
		return "<empty>"
	}
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return fmt.Sprintf("stat err=%v", err)
	}
	return fmt.Sprintf("size=%d", fi.Size())
}

func describeFreeBytes(dir string) string {
	var buf unix.Statfs_t
	if err := unix.Statfs(dir, &buf); err != nil {
		return fmt.Sprintf("statfs err=%v", err)
	}
	return fmt.Sprintf("%dB", buf.Bavail*uint64(buf.Bsize))
}

func newExt4BaseRawWithReplace(filePath, uuid string, size int64, replace bool) error {
	if replace {
		newFile := filePath + ".new"
		err := newExt4BaseRaw(newFile, uuid, size)
		if err != nil {
			return err
		}
		err = os.Rename(newFile, filePath)
		if err != nil {
			return err
		}
		return nil
	} else {
		return newExt4BaseRaw(filePath, uuid, size)
	}
}
