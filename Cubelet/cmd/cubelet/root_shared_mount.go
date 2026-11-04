// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	stdlog "log"
	"os"
	"strings"

	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
)

func ensureRootSharedMount(rootDir string) error {
	return ensureRootSharedMountWithLookup(rootDir, lookupExactMount, unix.Mount)
}

func ensureRootSharedMountWithLookup(rootDir string, lookup func(string) (*mountinfo.Info, error), mountRoot func(string, string, string, uintptr, string) error) error {

	if _, err := os.Stat(rootDir); os.IsNotExist(err) {
		return nil
	}

	// mountinfo reports canonical paths. Looking up an alias misses an existing
	// shared bind and can multiply peer mounts on every restart.
	mountInfo, err := lookup(rootDir)
	if err != nil {
		return fmt.Errorf("failed to get mount info for %s: %w", rootDir, err)
	}

	if mountInfo == nil {

		if err := mountRoot(rootDir, rootDir, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("failed to bind mount %s: %w", rootDir, err)
		}

		if err := mountRoot("", rootDir, "", unix.MS_SHARED, ""); err != nil {
			return fmt.Errorf("failed to make %s shared: %w", rootDir, err)
		}
		stdlog.Printf("set %s as shared mount", rootDir)
		return nil
	}

	for _, opt := range strings.Fields(mountInfo.Optional) {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		}
	}

	if err := mountRoot("", rootDir, "", unix.MS_SHARED, ""); err != nil {
		return fmt.Errorf("failed to make %s shared: %w", rootDir, err)
	}
	stdlog.Printf("set %s as shared mount", rootDir)
	return nil
}
