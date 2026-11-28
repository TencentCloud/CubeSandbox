//go:build linux

package cubemntnsinit_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildHelper(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cube-mntns-init")
	cmd := exec.Command("gcc", "-O2", "-Wall", "-Wextra", "-Werror", "-o", bin, filepath.Join("csrc", "main.c"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compile cube-mntns-init failed: %v\n%s", err, out)
	}
	return bin
}

func TestPassthroughWhenNotBigPod(t *testing.T) {
	bin := buildHelper(t)

	for _, val := range []string{"", "false", "0", "f"} {
		cmd := exec.Command(bin, "--", "/bin/sh", "-c", "printf ok-passthrough")
		cmd.Env = append(os.Environ(), "CUBELET_DEPLOYED_IN_BIG_POD="+val)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("val=%q: unexpected error: %v\n%s", val, err, out)
		}
		if got := string(out); got != "ok-passthrough" {
			t.Fatalf("val=%q: got %q, want %q", val, got, "ok-passthrough")
		}
	}
}

func TestValidationErrors(t *testing.T) {
	bin := buildHelper(t)

	cases := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{
			name: "missing command",
			args: []string{"--"},
			want: "usage:",
		},
		{
			name: "invalid bool env rejected",
			args: []string{"--", "/bin/true"},
			env:  []string{"CUBELET_DEPLOYED_IN_BIG_POD=maybe"},
			want: "invalid boolean CUBELET_DEPLOYED_IN_BIG_POD",
		},
		{
			name: "empty paths in bigpod",
			args: []string{"--", "/bin/true"},
			env:  []string{"CUBELET_DEPLOYED_IN_BIG_POD=true", "CUBELET_SHARED_MNT_PATHS="},
			want: "CUBELET_SHARED_MNT_PATHS is empty",
		},
		{
			name: "relative path rejected",
			args: []string{"--paths", "relative/path", "--", "/bin/true"},
			env:  []string{"CUBELET_DEPLOYED_IN_BIG_POD=true"},
			want: "invalid shared mount path",
		},
		{
			name: "root path rejected",
			args: []string{"--paths", "/", "--", "/bin/true"},
			env:  []string{"CUBELET_DEPLOYED_IN_BIG_POD=true"},
			want: "invalid shared mount path",
		},
		{
			name: "dotdot path rejected",
			args: []string{"--paths", "/data/../etc", "--", "/bin/true"},
			env:  []string{"CUBELET_DEPLOYED_IN_BIG_POD=true"},
			want: "contains '..'",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			cmd.Env = append(os.Environ(), tc.env...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected failure, got success: %s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("output %q does not contain %q", string(out), tc.want)
			}
		})
	}
}

// TestEndToEndMountPropagation exercises the real Linux mount namespace stack:
//  1. Pod1 mounts a sandbox tmpfs under d1/sb1 -> invisible to Host mntns H.
//  2. Pod1 exits -> A_node anchor keeps d1/sb1 alive.
//  3. Pod2 starts with a distinct container rootfs + newly added d3 path:
//     - keeps its own rootfs marker (not Pod1's),
//     - reads Pod1's d1/sb1 payload and cleanly unmounts it,
//     - receives d3 via sync_existing_anchor (upgrade compatibility).
//  4. Host mounts d2/host_vol while Pod3 is running -> live H -> C propagation.
func TestEndToEndMountPropagation(t *testing.T) {
	if os.Geteuid() != 0 {
		if os.Getenv("CUBELET_REQUIRE_MOUNT_TESTS") == "1" {
			t.Fatal("CUBELET_REQUIRE_MOUNT_TESTS=1 requires root")
		}
		t.Skip("requires root (CAP_SYS_ADMIN)")
	}

	bin := buildHelper(t)
	work := t.TempDir()
	hostData := filepath.Join(work, "host")
	d1 := filepath.Join(hostData, "d1")
	d2 := filepath.Join(hostData, "d2")
	d3 := filepath.Join(hostData, "d3")
	pinDir := filepath.Join(work, "pin")
	pinFile := filepath.Join(pinDir, "anchor")
	for _, p := range []string{d1, d2, d3, pinDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	// Start a dedicated "host mntns H" holder process so the test never mutates
	// the test runner's own mount namespace.
	readyFile := filepath.Join(work, "host_ready")
	triggerFile := filepath.Join(work, "host_trigger")
	hostDoneFile := filepath.Join(work, "host_mounted")
	stopFile := filepath.Join(work, "host_stop")

	hostScript := fmt.Sprintf(`
set -euo pipefail
mount --make-rprivate /
mount -t tmpfs -o size=8m tmpfs %q
mkdir -p %q %q %q %q
mount --bind %q %q && mount --make-shared %q
mount --bind %q %q && mount --make-shared %q
mount --bind %q %q && mount --make-shared %q
touch %q
while [ ! -f %q ]; do
  if [ -f %q ] && [ ! -f %q ]; then
    mkdir -p %q/host_vol
    mount -t tmpfs -o size=2m tmpfs %q/host_vol
    printf from-host > %q/host_vol/tag
    touch %q
  fi
  sleep 0.05
done
`, hostData, d1, d2, d3, pinDir,
		d1, d1, d1,
		d2, d2, d2,
		d3, d3, d3,
		readyFile,
		stopFile,
		triggerFile, hostDoneFile,
		d2, d2, d2, hostDoneFile)

	hostHolder := exec.Command("unshare", "--mount", "/bin/bash", "-c", hostScript)
	var hostOut bytes.Buffer
	hostHolder.Stdout = &hostOut
	hostHolder.Stderr = &hostOut
	if err := hostHolder.Start(); err != nil {
		t.Fatalf("start host holder: %v", err)
	}
	defer func() {
		_ = os.WriteFile(stopFile, []byte("1"), 0o600)
		_ = hostHolder.Wait()
	}()

	waitForFile(t, readyFile, 5*time.Second, &hostOut)
	hostMntns := fmt.Sprintf("/proc/%d/ns/mnt", hostHolder.Process.Pid)

	runPod := func(marker, paths, body string) string {
		t.Helper()
		// Each simulated container runs in its own unshared mntns with a private
		// /tmp marker mount so we can prove pivot_root preserves each Pod's rootfs.
		markerDir := filepath.Join(work, "rootfs-marker")
		_ = os.MkdirAll(markerDir, 0o755)
		podScript := fmt.Sprintf(`
set -euo pipefail
mount --make-rprivate /
mount -t tmpfs -o size=2m tmpfs %q
printf %q > %q/id
exec env CUBELET_DEPLOYED_IN_BIG_POD=true %q \
  --host-mntns %q --pin-dir %q --pin-file %q --paths %q -- \
  /bin/bash -ec "$POD_BODY"
`, markerDir, marker, markerDir, bin, hostMntns, pinDir, pinFile, paths)

		cmd := exec.Command("unshare", "--mount", "/bin/bash", "-c", podScript)
		cmd.Env = append(os.Environ(), "POD_BODY="+body)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pod(%s) failed: %v\noutput:\n%s\nhost holder:\n%s",
				marker, err, out, hostOut.String())
		}
		return strings.TrimSpace(string(out))
	}

	markerFile := filepath.Join(work, "rootfs-marker", "id")

	// 1. Pod1 creates a sandbox mount under d1/sb1.
	got1 := runPod("pod1-rootfs", d1+":"+d2, fmt.Sprintf(`
mkdir -p %q/sb1
mount -t tmpfs -o size=2m tmpfs %q/sb1
printf from-pod1 > %q/sb1/data
printf "%%s|%%s" "$(cat %q)" "$(cat %q/sb1/data)"
`, d1, d1, d1, markerFile, d1))
	if got1 != "pod1-rootfs|from-pod1" {
		t.Fatalf("pod1 output = %q, want pod1-rootfs|from-pod1", got1)
	}

	// Verify Host H mountinfo NEVER sees d1/sb1 (systemd isolation!).
	hostMI, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", hostHolder.Process.Pid))
	if err != nil {
		t.Fatalf("read host mountinfo: %v", err)
	}
	if strings.Contains(string(hostMI), filepath.Join(d1, "sb1")) {
		t.Fatalf("host mountinfo leaked sandbox mount %s:\n%s", filepath.Join(d1, "sb1"), hostMI)
	}

	// 2. Pod1 has exited. Pod2 starts with its own rootfs + adds d3 (upgrade path):
	//    must preserve pod2-rootfs, read Pod1's surviving d1/sb1, unmount it, and see d3.
	got2 := runPod("pod2-rootfs", d1+":"+d2+":"+d3, fmt.Sprintf(`
id="$(cat %q)"
data="$(cat %q/sb1/data)"
umount %q/sb1
mkdir -p %q/probe
mount -t tmpfs -o size=2m tmpfs %q/probe
printf ok-d3 > %q/probe/v
d3v="$(cat %q/probe/v)"
umount %q/probe
printf "%%s|%%s|%%s" "$id" "$data" "$d3v"
`, markerFile, d1, d1, d3, d3, d3, d3, d3))
	if got2 != "pod2-rootfs|from-pod1|ok-d3" {
		t.Fatalf("pod2 output = %q, want pod2-rootfs|from-pod1|ok-d3", got2)
	}

	// 3. Pod3 runs while Host H mounts d2/host_vol -> live H -> C propagation.
	pod3Ready := filepath.Join(work, "pod3_ready")
	pod3Script := fmt.Sprintf(`
set -euo pipefail
mount --make-rprivate /
exec env CUBELET_DEPLOYED_IN_BIG_POD=true %q \
  --host-mntns %q --pin-dir %q --pin-file %q --paths %q -- \
  /bin/bash -ec '
    touch %q
    for _ in $(seq 1 100); do
      if [ -f %q/host_vol/tag ]; then
        cat %q/host_vol/tag
        exit 0
      fi
      sleep 0.05
    done
    echo "timeout waiting for host propagation" >&2
    exit 1
  '
`, bin, hostMntns, pinDir, pinFile, d1+":"+d2+":"+d3, pod3Ready, d2, d2)

	pod3 := exec.Command("unshare", "--mount", "/bin/bash", "-c", pod3Script)
	var pod3Out bytes.Buffer
	pod3.Stdout = &pod3Out
	pod3.Stderr = &pod3Out
	if err := pod3.Start(); err != nil {
		t.Fatalf("start pod3: %v", err)
	}

	waitForFile(t, pod3Ready, 5*time.Second, &pod3Out)
	if err := os.WriteFile(triggerFile, []byte("1"), 0o600); err != nil {
		t.Fatalf("trigger host mount: %v", err)
	}
	if err := pod3.Wait(); err != nil {
		t.Fatalf("pod3 failed: %v\noutput:\n%s", err, pod3Out.String())
	}
	if got := strings.TrimSpace(pod3Out.String()); got != "from-host" {
		t.Fatalf("pod3 output = %q, want from-host", got)
	}

	// Verify Host H still has zero leaks from d3/probe or d1/sb1.
	hostMI, err = os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", hostHolder.Process.Pid))
	if err != nil {
		t.Fatalf("read host mountinfo after pod3: %v", err)
	}
	if strings.Contains(string(hostMI), "sb1") || strings.Contains(string(hostMI), "probe") {
		t.Fatalf("host mountinfo leaked container mounts:\n%s", hostMI)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration, logBuf *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (output: %s)", path, logBuf.String())
}
