// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
)

func TestArtifactStoreDirOf(t *testing.T) {
	got := artifactStoreDirOf("/var/lib/cubemaster/storage/rfs-abc/rfs-abc.ext4")
	if want := "/var/lib/cubemaster/storage"; got != want {
		t.Fatalf("artifactStoreDirOf = %q, want %q", got, want)
	}
}

func TestOrphanLoopCandidates(t *testing.T) {
	const store = "/var/lib/cubemaster/storage"

	cases := []struct {
		name     string
		list     string
		storeDir string
		want     []string
	}{
		{
			name:     "empty output",
			list:     "",
			storeDir: store,
		},
		{
			name:     "deleted backing file is a candidate",
			list:     "/dev/loop3 /var/lib/cubemaster/storage/rfs-a/rfs-a.ext4 (deleted)\n",
			storeDir: store,
			want:     []string{"/dev/loop3"},
		},
		{
			name:     "existing backing file under the store is left alone (may be a live build)",
			list:     "/dev/loop1 /var/lib/cubemaster/storage/rfs-b/rfs-b.ext4\n",
			storeDir: store,
		},
		{
			name:     "devices outside the store are never touched",
			list:     "/dev/loop0 /var/lib/snapd/snaps/core.snap\n/dev/loop2 /srv/other/disk.img (deleted)\n",
			storeDir: store,
		},
		{
			name:     "the store dir itself must not match as a prefix of a sibling",
			list:     "/dev/loop4 /var/lib/cubemaster/storage-backup/rfs-c/rfs-c.ext4 (deleted)\n",
			storeDir: store,
		},
		{
			name:     "blank and malformed lines are skipped",
			list:     "\n   \n/dev/loop5\n/dev/loop6 /var/lib/cubemaster/storage/rfs-d/rfs-d.ext4 (deleted)\n",
			storeDir: store,
			want:     []string{"/dev/loop6"},
		},
		{
			name:     "a root store dir is refused rather than detaching everything",
			list:     "/dev/loop0 /any/file.img (deleted)\n",
			storeDir: "/",
		},
		{
			name:     "trailing slash on the store dir behaves the same",
			list:     "/dev/loop7 /var/lib/cubemaster/storage/rfs-e/rfs-e.ext4 (deleted)\n",
			storeDir: store + "/",
			want:     []string{"/dev/loop7"},
		},
		{
			name:     "a backing path containing spaces is still matched",
			list:     "/dev/loop8 /var/lib/cubemaster/storage/rfs f/rfs f.ext4 (deleted)\n",
			storeDir: store,
			want:     []string{"/dev/loop8"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orphanLoopCandidates(tc.list, tc.storeDir)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("orphanLoopCandidates = %v, want %v", got, tc.want)
			}
		})
	}
}

// setupStreamingFakes puts fake truncate/mkfs.ext4/mount/umount/resize2fs on
// PATH (each appending its argv to a trace file) and lets the streaming build
// believe loop mounts are usable. The caller installs its own `losetup` fake,
// which is where every interesting behaviour lives.
func setupStreamingFakes(t *testing.T) (binDir, tracePath, ext4Path string) {
	t.Helper()
	binDir = t.TempDir()
	tracePath = filepath.Join(binDir, "trace.log")
	store := filepath.Join(t.TempDir(), "storage")
	ext4Path = filepath.Join(store, "rfs-a", "rfs-a.ext4")

	t.Setenv("PATH", binDir)
	t.Setenv("FAKE_TRACE", tracePath)
	t.Setenv("FAKE_STORE", store)
	t.Setenv("FAKE_STATE", filepath.Join(binDir, "state"))

	for _, name := range []string{"truncate", "mkfs.ext4", "mount", "umount", "resize2fs"} {
		installFakeCommand(t, binDir, name, `echo "`+name+` $*" >> "$FAKE_TRACE"`)
	}

	patches := gomonkey.NewPatches()
	patches.ApplyFuncReturn(canUseLoopMount, true)
	patches.ApplyFuncReturn(StreamRegistryToDir, nil)
	t.Cleanup(patches.Reset)
	return binDir, tracePath, ext4Path
}

func runStreamingBuild(t *testing.T, ext4Path string) error {
	t.Helper()
	source := &PreparedSource{LocalRef: "local/img:latest", ExportMode: ExportModeNative}
	return createExt4ImageStreaming(context.Background(), source, t.TempDir(), ext4Path, 1024, nil)
}

func traceLines(t *testing.T, tracePath string) []string {
	t.Helper()
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read command trace: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func traceIndex(lines []string, prefix string) int {
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return i
		}
	}
	return -1
}

func traceCount(lines []string, prefix string) int {
	n := 0
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestStreamingUnmountsBeforeDetach guards the actual leak: detaching a device
// that is still mounted fails EBUSY and pins it for the lifetime of the host.
func TestStreamingUnmountsBeforeDetach(t *testing.T) {
	binDir, tracePath, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in --find) echo /dev/loop9 ;; esac`)

	if err := runStreamingBuild(t, ext4Path); err != nil {
		t.Fatalf("streaming build failed: %v", err)
	}

	lines := traceLines(t, tracePath)
	umountAt := traceIndex(lines, "umount ")
	detachAt := traceIndex(lines, "losetup --detach")
	if umountAt < 0 || detachAt < 0 {
		t.Fatalf("expected both umount and detach in trace, got %v", lines)
	}
	if umountAt > detachAt {
		t.Fatalf("detach ran before umount (would fail EBUSY and leak the device): %v", lines)
	}
}

// TestStreamingRetriesDetachAfterBusy covers the lazy-umount window, where the
// mount is released asynchronously and the first detaches legitimately fail.
func TestStreamingRetriesDetachAfterBusy(t *testing.T) {
	binDir, tracePath, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in
--find) echo /dev/loop9 ;;
--detach)
	if [ -f "$FAKE_STATE.2" ]; then exit 0; fi
	if [ -f "$FAKE_STATE.1" ]; then > "$FAKE_STATE.2"; else > "$FAKE_STATE.1"; fi
	echo "device or resource busy" >&2; exit 1 ;;
esac`)

	if err := runStreamingBuild(t, ext4Path); err != nil {
		t.Fatalf("streaming build failed: %v", err)
	}

	if got := traceCount(traceLines(t, tracePath), "losetup --detach"); got != 3 {
		t.Fatalf("detach attempts = %d, want 3 (retry until it succeeds)", got)
	}
}

// TestStreamingSkipsDetachRetriesWhenMountStuck: once even the lazy umount
// failed the mount is still there, so every detach is guaranteed to fail —
// spending the retry budget only delays the build.
func TestStreamingSkipsDetachRetriesWhenMountStuck(t *testing.T) {
	binDir, tracePath, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "umount", `echo "umount $*" >> "$FAKE_TRACE"
echo "target is busy" >&2; exit 1`)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in
--find) echo /dev/loop9 ;;
--detach) echo "device or resource busy" >&2; exit 1 ;;
esac`)

	if err := runStreamingBuild(t, ext4Path); err != nil {
		t.Fatalf("streaming build failed: %v", err)
	}

	lines := traceLines(t, tracePath)
	if got := traceCount(lines, "umount -l"); got != 1 {
		t.Fatalf("lazy umount attempts = %d, want 1", got)
	}
	if got := traceCount(lines, "losetup --detach"); got != 1 {
		t.Fatalf("detach attempts = %d, want 1 (mount is stuck, retrying is pointless)", got)
	}
}

// TestStreamingDetachesAfterLazyUmount is the case the detach retry budget
// exists for: the plain umount fails, the lazy one succeeds, and the device is
// released once the kernel has dropped the mount.
func TestStreamingDetachesAfterLazyUmount(t *testing.T) {
	binDir, tracePath, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "umount", `echo "umount $*" >> "$FAKE_TRACE"
case "$1" in -l) exit 0 ;; *) echo "target is busy" >&2; exit 1 ;; esac`)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in
--find) echo /dev/loop9 ;;
--detach)
	if [ -f "$FAKE_STATE" ]; then exit 0; fi
	> "$FAKE_STATE"; echo "device or resource busy" >&2; exit 1 ;;
esac`)

	if err := runStreamingBuild(t, ext4Path); err != nil {
		t.Fatalf("streaming build failed: %v", err)
	}

	lines := traceLines(t, tracePath)
	if got := traceCount(lines, "umount -l"); got != 1 {
		t.Fatalf("lazy umount attempts = %d, want 1", got)
	}
	if got := traceCount(lines, "losetup --detach"); got != 2 {
		t.Fatalf("detach attempts = %d, want 2 (retried once the lazy umount landed)", got)
	}
}

// TestStreamingDetachesWhenMountFails guards the removal of the explicit detach
// on the mount-failure path: it now happens only through the deferred cleanup.
func TestStreamingDetachesWhenMountFails(t *testing.T) {
	binDir, tracePath, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "mount", `echo "mount $*" >> "$FAKE_TRACE"
echo "bad option" >&2; exit 32`)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in --find) echo /dev/loop9 ;; esac`)

	if err := runStreamingBuild(t, ext4Path); err == nil {
		t.Fatal("expected the build to fail when the mount fails")
	}

	lines := traceLines(t, tracePath)
	if traceIndex(lines, "losetup --detach -- /dev/loop9") < 0 {
		t.Fatalf("allocated device must be detached even though it was never mounted: %v", lines)
	}
	if traceIndex(lines, "umount ") >= 0 {
		t.Fatalf("nothing was mounted, so no umount should be attempted: %v", lines)
	}
}

// TestStreamingReclaimsOrphansOnAllocationFailure: exhaustion caused by earlier
// leaks must self-heal instead of permanently degrading to the phase-1 build.
func TestStreamingReclaimsOrphansOnAllocationFailure(t *testing.T) {
	binDir, tracePath, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in
--find)
	if [ -f "$FAKE_STATE" ]; then echo /dev/loop9; else > "$FAKE_STATE"; echo "could not find any free loop device" >&2; exit 1; fi ;;
--list) echo "/dev/loop3 $FAKE_STORE/rfs-gone/rfs-gone.ext4 (deleted)" ;;
esac`)

	if err := runStreamingBuild(t, ext4Path); err != nil {
		t.Fatalf("streaming build failed instead of reclaiming and retrying: %v", err)
	}

	lines := traceLines(t, tracePath)
	if got := traceCount(lines, "losetup --find"); got != 2 {
		t.Fatalf("losetup --find attempts = %d, want 2 (retry after reclaim)", got)
	}
	if traceIndex(lines, "losetup --detach -- /dev/loop3") < 0 {
		t.Fatalf("expected the orphaned device to be detached, got %v", lines)
	}
}

// TestStreamingAllocationFailureWithoutOrphans: with nothing to reclaim the
// original allocation error must be returned unchanged (and no retry spent), so
// causes other than exhaustion still surface as themselves.
func TestStreamingAllocationFailureWithoutOrphans(t *testing.T) {
	binDir, tracePath, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in
--find) echo "could not find any free loop device" >&2; exit 1 ;;
--list) ;;
esac`)

	err := runStreamingBuild(t, ext4Path)
	if err == nil {
		t.Fatal("expected the build to fail when no loop device can be allocated")
	}
	if strings.Contains(err.Error(), "retry") {
		t.Fatalf("nothing was reclaimed, so no retry should be reported: %v", err)
	}

	if got := traceCount(traceLines(t, tracePath), "losetup --find"); got != 1 {
		t.Fatalf("losetup --find attempts = %d, want 1 (nothing to reclaim)", got)
	}
}

// TestStreamingReportsBothAllocationFailures: when the retry after a reclaim
// fails too, both the original and the retry error must be visible — otherwise
// the reclaim hides the real reason the fast path was lost.
func TestStreamingReportsBothAllocationFailures(t *testing.T) {
	binDir, _, ext4Path := setupStreamingFakes(t)
	installFakeCommand(t, binDir, "losetup", `echo "losetup $*" >> "$FAKE_TRACE"
case "$1" in
--find) echo "could not find any free loop device" >&2; exit 1 ;;
--list) echo "/dev/loop3 $FAKE_STORE/rfs-gone/rfs-gone.ext4 (deleted)" ;;
esac`)

	err := runStreamingBuild(t, ext4Path)
	if err == nil {
		t.Fatal("expected the build to fail when the allocation fails even after reclaiming")
	}
	if !strings.Contains(err.Error(), "could not find any free loop device") ||
		!strings.Contains(err.Error(), "retry after reclaiming 1 device(s)") {
		t.Fatalf("error must report both the original and the retry failure, got %v", err)
	}
}
