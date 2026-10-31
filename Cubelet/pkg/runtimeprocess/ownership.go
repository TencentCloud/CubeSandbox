// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package runtimeprocess checks physical runtime ownership independently of
// containerd metadata, which can disappear before a failed Create is reaped.
package runtimeprocess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// TAPDescriptor identifies an open TUN descriptor and its attached interface.
type TAPDescriptor struct {
	PID       int
	FD        string
	Interface string
}

// ReadTAPInterface reads the interface name reported by the TUN driver.
func ReadTAPInterface(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok && name == "iff" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", nil
}

// ScanTAPDescriptors shares procfs discovery between runtime safety checks and
// diagnostics. Strict mode returns attached descriptors and rejects incomplete
// inspection; diagnostics also include unattached TUN fds and may be partial. excludePID omits the caller's own retained fds.
func ScanTAPDescriptors(ctx context.Context, procRoot string, excludePID int, strict bool) ([]TAPDescriptor, error) {
	var found []TAPDescriptor
	err := walkProcesses(ctx, procRoot, func(pid int, base string) error {
		if pid == excludePID {
			return nil
		}
		fds, err := os.ReadDir(filepath.Join(base, "fdinfo"))
		if os.IsNotExist(err) || (err != nil && !strict) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, fd := range fds {
			if err := ctx.Err(); err != nil {
				return err
			}
			iface, err := ReadTAPInterface(filepath.Join(base, "fdinfo", fd.Name()))
			if os.IsNotExist(err) {
				continue // fd closed during the scan
			}
			if err != nil && strict {
				return err
			}
			// fdinfo also finds TUN devices opened through a bind mount.
			if iface != "" {
				found = append(found, TAPDescriptor{PID: pid, FD: fd.Name(), Interface: iface})
			} else if !strict {
				target, _ := os.Readlink(filepath.Join(base, "fd", fd.Name()))
				if target == "/dev/net/tun" {
					found = append(found, TAPDescriptor{PID: pid, FD: fd.Name()})
				}
			}
		}
		return nil
	})
	return found, err
}

// TAPOwners excludes Cubelet's own pooled descriptors and inspects all other
// processes, including renamed runtimes and helpers that received a handoff.
// Multiple duplicated descriptors in one process count as one owner.
func TAPOwners(ctx context.Context, procRoot, tapName string, selfPID int) ([]int, error) {
	descriptors, err := ScanTAPDescriptors(ctx, procRoot, selfPID, true)
	var found []int
	for _, fd := range descriptors {
		if fd.Interface == tapName {
			found = append(found, fd.PID)
		}
	}
	slices.Sort(found)
	return slices.Compact(found), err
}

func walkProcesses(ctx context.Context, procRoot string, visit func(int, string) error) error {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return fmt.Errorf("scan runtime processes: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		base := filepath.Join(procRoot, entry.Name())
		if err := visit(pid, base); err != nil {
			return fmt.Errorf("inspect runtime pid %d: %w", pid, err)
		}
	}
	return nil
}
