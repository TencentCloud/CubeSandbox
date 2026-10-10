// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRootDefault is where the kernel exposes the process table on a normal
// host. tapTunHolders takes it as a parameter so tests can point it at a
// fake tree instead.
const procRootDefault = "/proc"

// tapTunHolders returns the pids that currently hold a /dev/net/tun queue fd
// attached to tapName, read straight from the kernel under procRoot. selfPid
// is never included: this runtime retains its own queue fd for every pooled
// TAP (see poolTapFD), and counting it would make every TAP permanently
// unassignable.
//
// This asks the kernel rather than trusting cubelet's own bookkeeping on
// purpose: a leaked holder is exactly the case where that bookkeeping is
// missing or wrong. A TUN device accepts multiple queues, so a foreign
// process holding one never stops this runtime from also opening the same
// tap — which is why verifyTapReusableFD's "can I reopen it" probe cannot see
// a leak like this at all. The tun driver writes the currently attached
// device name into each fd's fdinfo, so the mapping read here is
// authoritative.
//
// Processes that vanish mid-scan, and fds this process may not read, are
// skipped rather than failing the scan: a dying neighbour must not wedge an
// unrelated tap in Cleaning forever.
func tapTunHolders(procRoot, tapName string, selfPid int) ([]int, error) {
	if tapName == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}

	var holders []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == selfPid {
			continue // not a process directory, or this runtime itself
		}
		fdDir := filepath.Join(procRoot, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // process exited mid-scan; a partial answer beats failing
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || target != tunDevicePath {
				continue
			}
			fdinfoPath := filepath.Join(procRoot, entry.Name(), "fdinfo", fd.Name())
			if readTunIface(fdinfoPath) == tapName {
				holders = append(holders, pid)
				break
			}
		}
	}
	return holders, nil
}

// readTunIface returns the tap device a tun queue fd is attached to, from the
// "iff:" line the tun driver adds to fdinfo. An fd that has been opened but
// not yet attached, or that disappeared mid-scan, holds no device.
func readTunIface(fdinfoPath string) string {
	b, err := os.ReadFile(fdinfoPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "iff:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
