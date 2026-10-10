// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeTunFd writes one /proc/<pid>/fd/<fd> symlink plus its fdinfo, the same
// shape the kernel exposes for an open /dev/net/tun queue.
func fakeTunFd(t *testing.T, procRoot, pid, fd, target, fdinfo string) {
	t.Helper()
	fdDir := filepath.Join(procRoot, pid, "fd")
	fdinfoDir := filepath.Join(procRoot, pid, "fdinfo")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fdinfoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(fdDir, fd)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fdinfoDir, fd), []byte(fdinfo), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTapTunHoldersReportsOnlyTheNamedTap(t *testing.T) {
	procRoot := t.TempDir()
	// A leaked process holding two queues on the tap under test.
	fakeTunFd(t, procRoot, "100", "7", tunDevicePath, "pos:\t0\nflags:\t02\niff:\ttap0\n")
	fakeTunFd(t, procRoot, "100", "8", tunDevicePath, "pos:\t0\nflags:\t02\niff:\ttap0\n")
	// A different process holding an unrelated tap must not be reported.
	fakeTunFd(t, procRoot, "200", "3", tunDevicePath, "iff:\ttap1\n")

	holders, err := tapTunHolders(procRoot, "tap0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0] != 100 {
		t.Fatalf("holders = %v, want [100]", holders)
	}
}

func TestTapTunHoldersIgnoresNonTunFdsAndOtherInterfaces(t *testing.T) {
	procRoot := t.TempDir()
	fakeTunFd(t, procRoot, "100", "4", "/dev/null", "pos:\t0\n")
	fakeTunFd(t, procRoot, "200", "3", tunDevicePath, "iff:\ttap1\n")

	holders, err := tapTunHolders(procRoot, "tap0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 0 {
		t.Fatalf("holders = %v, want none", holders)
	}
}

func TestTapTunHoldersExcludesSelf(t *testing.T) {
	procRoot := t.TempDir()
	fakeTunFd(t, procRoot, "100", "7", tunDevicePath, "iff:\ttap0\n")

	holders, err := tapTunHolders(procRoot, "tap0", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 0 {
		t.Fatalf("holders = %v, want none: this runtime retains its own pooled queue fd on every ready tap", holders)
	}
}

func TestTapTunHoldersSkipsUnreadableProcesses(t *testing.T) {
	procRoot := t.TempDir()
	fakeTunFd(t, procRoot, "100", "7", tunDevicePath, "iff:\ttap0\n")
	// A process directory with no fd dir, as happens when a process exits
	// mid-scan. A partial answer beats failing the whole check.
	if err := os.MkdirAll(filepath.Join(procRoot, "101"), 0o755); err != nil {
		t.Fatal(err)
	}

	holders, err := tapTunHolders(procRoot, "tap0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0] != 100 {
		t.Fatalf("holders = %v, want [100]", holders)
	}
}

func TestTapTunHoldersEmptyTapNameNeverMatches(t *testing.T) {
	procRoot := t.TempDir()
	fakeTunFd(t, procRoot, "100", "7", tunDevicePath, "iff:\ttap0\n")
	// An unattached queue fd: opened but never bound to a device, so fdinfo
	// carries no iff: line. Must not be confused with a name-less match.
	fakeTunFd(t, procRoot, "100", "8", tunDevicePath, "pos:\t0\nflags:\t02\n")

	holders, err := tapTunHolders(procRoot, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 0 {
		t.Fatalf("holders = %v, want none for an empty tap name", holders)
	}
}
