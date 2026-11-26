// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// procRoot is where fileHolders looks for processes.
const procRoot = "/proc"

// fileHolders returns the pids of processes that have path open or mapped.
// Files are matched by device and inode, so a holder that reached the file
// through a bind mount or another path still counts, and so does one that
// mapped the file and closed its descriptor.
//
// A process that exits during the scan is skipped. Any error reading the file
// or root itself is returned, and the caller must then treat the file as in
// use.
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
		if holdsFD(dir, target) || mapsFile(dir, unix.Major(dev), unix.Minor(dev), st.Ino) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func holdsFD(procDir string, target os.FileInfo) bool {
	fdDir := filepath.Join(procDir, "fd")
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		info, err := os.Stat(filepath.Join(fdDir, fd.Name()))
		if err == nil && os.SameFile(info, target) {
			return true
		}
	}
	return false
}

// mapsFile reports whether a line of <procDir>/maps maps the file with this
// device and inode. The line format is "addr perms offset major:minor inode path",
// with major and minor in hex.
func mapsFile(procDir string, major, minor uint32, ino uint64) bool {
	f, err := os.Open(filepath.Join(procDir, "maps"))
	if err != nil {
		return false
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
			return true
		}
	}
	return false
}
