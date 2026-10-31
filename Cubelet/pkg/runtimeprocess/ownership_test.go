package runtimeprocess

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func fakeRuntime(t *testing.T, root, pid, exe string, args ...string) string {
	t.Helper()
	base := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(base, "fdinfo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(base, "exe")); err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, arg := range args {
		body = append(body, []byte(arg)...)
		body = append(body, 0)
	}
	if err := os.WriteFile(filepath.Join(base, "cmdline"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestTAPOwnersRejectsForeignRuntimeEvenWithDuplicatedFD(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ pid, exe, tap string }{
		{"31", "/bin/containerd-shim-cube-rs", "192.168.0.140"},
		{"32", "/bin/cube-hypervisor", "192.168.0.140"},
		{"33", "/bin/containerd-shim-cube-rs", "192.168.0.14"},
		{"34", "/bin/containerd-shim-cube-rs", "192.168.0.140"},
		{"35", "/bin/custom-runtime", "192.168.0.140"},
	} {
		base := fakeRuntime(t, root, tc.pid, tc.exe)
		for _, fd := range []string{"74", "75"} {
			if err := os.WriteFile(filepath.Join(base, "fdinfo", fd), []byte("pos:\t0\niff:\t"+tc.tap+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := TAPOwners(context.Background(), root, "192.168.0.140", 34)
	if err != nil || !reflect.DeepEqual(got, []int{31, 32, 35}) {
		t.Fatalf("owners=%v err=%v", got, err)
	}
}

func TestOwnershipScanFailsClosed(t *testing.T) {
	if _, err := TAPOwners(context.Background(), filepath.Join(t.TempDir(), "missing"), "tap", 0); err == nil {
		t.Fatal("missing procfs must not mean no live shim")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	fakeRuntime(t, root, "42", "/bin/containerd-shim-cube-rs")
	if _, err := TAPOwners(ctx, root, "tap", 0); err == nil {
		t.Fatal("cancelled ownership check must not report an unused TAP")
	}
}

func TestOwnershipScanRejectsPartialDescriptorInspection(t *testing.T) {
	root := t.TempDir()
	base := fakeRuntime(t, root, "42", "/bin/custom-runtime")
	// A directory in place of fdinfo deterministically causes a read error,
	// including when this fixture is run as root.
	if err := os.Mkdir(filepath.Join(base, "fdinfo", "74"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := TAPOwners(context.Background(), root, "tap", 0); err == nil {
		t.Fatal("partial process inspection must not certify a TAP as unused")
	}
}

func TestDiagnosticScanCanReturnPartialResults(t *testing.T) {
	root := t.TempDir()
	base := fakeRuntime(t, root, "42", "/bin/custom-runtime")
	if err := os.Mkdir(filepath.Join(base, "fdinfo", "73"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "fdinfo", "74"), []byte("iff:\ttap0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ScanTAPDescriptors(context.Background(), root, 0, false)
	if err != nil || len(got) != 1 || got[0].Interface != "tap0" {
		t.Fatalf("descriptors=%v err=%v", got, err)
	}
}

func BenchmarkTAPOwnersManyDescriptors(b *testing.B) {
	root := b.TempDir()
	for pid := 1; pid <= 100; pid++ {
		dir := filepath.Join(root, strconv.Itoa(pid), "fdinfo")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		for fd := 0; fd < 100; fd++ {
			body := "pos:\t0\n"
			if fd == 99 {
				body += "iff:\ttap0\n"
			}
			if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(fd)), []byte(body), 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		owners, err := TAPOwners(context.Background(), root, "tap0", 0)
		if err != nil || len(owners) != 100 {
			b.Fatalf("owners=%d err=%v", len(owners), err)
		}
	}
}
