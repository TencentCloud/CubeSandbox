// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// procRoot is where fileHolders looks for processes. Tests point it at a
// directory that shows only their own process.
var procRoot = "/proc"

// fileHolders returns the pids of processes that have path open or mapped.
// Files are matched by device and inode, so a holder that reached the file
// through a bind mount or another path still counts, and so does one that
// mapped the file and closed its descriptor.
//
// Only "does not exist" errors are skipped: the process exited, or the
// descriptor was closed, during the scan. Any other error, such as a process
// whose /proc entries cannot be read, is returned, and the caller must then
// treat the file as in use.
func fileHolders(root, path string) ([]int, error) {
	target, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	st, ok := target.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat %s: no device and inode", path)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	dev := uint64(st.Dev) //nolint:unconvert // Dev is uint32 on some arches
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		held, err := holdsFD(dir, target)
		if err == nil && !held {
			held, err = mapsFile(dir, unix.Major(dev), unix.Minor(dev), st.Ino)
		}
		if err != nil {
			return nil, fmt.Errorf("pid %d: %w", pid, err)
		}
		if held {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func holdsFD(procDir string, target os.FileInfo) (bool, error) {
	fdDir := filepath.Join(procDir, "fd")
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false, skipGone(err)
	}
	for _, fd := range fds {
		info, err := os.Stat(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			if err = skipGone(err); err != nil {
				return false, err
			}
			continue
		}
		if os.SameFile(info, target) {
			return true, nil
		}
	}
	return false, nil
}

// mapsFile reports whether a line of <procDir>/maps maps the file with this
// device and inode. The line format is "addr perms offset major:minor inode path",
// with major and minor in hex.
func mapsFile(procDir string, major, minor uint32, ino uint64) (bool, error) {
	f, err := os.Open(filepath.Join(procDir, "maps"))
	if err != nil {
		return false, skipGone(err)
	}
	defer f.Close()
	want := strconv.FormatUint(ino, 10)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[4] != want {
			continue
		}
		maj, min, found := strings.Cut(fields[3], ":")
		if !found {
			continue
		}
		gotMaj, err1 := strconv.ParseUint(maj, 16, 32)
		gotMin, err2 := strconv.ParseUint(min, 16, 32)
		if err1 == nil && err2 == nil && uint32(gotMaj) == major && uint32(gotMin) == minor {
			return true, nil
		}
	}
	return false, skipGone(scanner.Err())
}

// skipGone drops the error of an entry that vanished during the scan: a
// process that exited (ENOENT, or ESRCH while reading its files) or a
// descriptor closed after it was listed.
func skipGone(err error) error {
	if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
