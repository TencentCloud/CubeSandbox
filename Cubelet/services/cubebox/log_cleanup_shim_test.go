// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDelayedCleanupShimLauncher(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "shim 'quoted'")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s' \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	paths := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := delayedCleanupShimPath(root, shim, "")
			paths <- p
			errs <- err
		}()
	}
	wg.Wait()
	close(paths)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	launcher := ""
	for path := range paths {
		if launcher != "" && launcher != path {
			t.Fatal("launcher path changed between calls")
		}
		launcher = path
	}
	for _, action := range []string{"start", "delete"} {
		out, err := exec.Command(launcher, action).CombinedOutput()
		if err != nil || string(out) != action {
			t.Fatalf("%s: output=%q error=%v", action, out, err)
		}
	}
	if err := os.Remove(launcher); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "different-shim"), launcher); err != nil {
		t.Fatal(err)
	}
	if _, err := delayedCleanupShimPath(root, shim, ""); err == nil || !strings.Contains(err.Error(), "invalid shim launcher") {
		t.Fatalf("unexpected error for modified launcher: %v", err)
	}
	if _, err := delayedCleanupShimPath(root, "relative-shim", ""); err == nil {
		t.Fatal("accepted a relative shim path")
	}
}
