package cubevs

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH l7mark ../src/l7_mark_test.bpf.c -- -I../vmlinux/$GOARCH

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
)

// loadL7MarkProgram loads the l7_mark test object (only its .rodata and the
// test program), applies the given L7 mark overrides to the const volatile
// globals exactly as rewriteConstants would, and returns the loaded program.
func loadL7MarkProgram(t *testing.T, httpMark, httpsMark, mask uint32) *ebpf.Program {
	t.Helper()
	spec, err := loadL7mark()
	if err != nil {
		t.Fatalf("load l7 mark test spec: %v", err)
	}
	for name, mapSpec := range spec.Maps {
		if name != ".rodata" {
			delete(spec.Maps, name)
		} else {
			mapSpec.Pinning = ebpf.PinNone
		}
	}
	setVar := func(name string, value uint32) {
		v := spec.Variables[name]
		if v == nil {
			t.Fatalf("variable %s missing from l7 mark test object", name)
		}
		if err := v.Set(value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	setVar(globalNameCubeL7MarkHTTP, httpMark)
	setVar(globalNameCubeL7MarkHTTPS, httpsMark)
	setVar(globalNameCubeL7MarkMask, mask)

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF l7 mark test unavailable: %v", err)
		}
		t.Fatalf("load l7 mark test collection: %v", err)
	}
	t.Cleanup(coll.Close)
	prog := coll.Programs["test_l7_mark"]
	if prog == nil {
		t.Fatal("test_l7_mark program missing")
	}
	return prog
}

func runL7MarkCase(t *testing.T, prog *ebpf.Program, inMark uint32) uint32 {
	t.Helper()
	data := make([]byte, 16) // pad past the 4-byte case; tiny packets are rejected
	binary.LittleEndian.PutUint32(data[0:4], inMark)
	ret, out, err := prog.Test(data)
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF l7 mark test-run unavailable: %v", err)
		}
		t.Fatalf("run l7 mark test: %v", err)
	}
	if ret != 0 {
		t.Fatalf("test_l7_mark returned %d, want TC_ACT_OK", ret)
	}
	return binary.LittleEndian.Uint32(out[0:4])
}

// TestL7MarkStampedMatchesConfiguredValue proves that an overridden
// cube_l7_mark_http reaches the running dataplane and is the value stamped
// onto skb->mark (masked), exactly as mvmtap.bpf.c does for L7 traffic.
func TestL7MarkStampedMatchesConfiguredValue(t *testing.T) {
	const httpMark = uint32(0xABCD0000)
	const mask = uint32(0xFFFF0000)
	prog := loadL7MarkProgram(t, httpMark, defaultL7MarkHTTPS, mask)

	// Low (user) bits of the incoming mark are preserved; the configured HTTP
	// mark is OR'd into the cube-owned bits.
	const inMark = uint32(0x00001234)
	want := (inMark &^ mask) | httpMark
	if got := runL7MarkCase(t, prog, inMark); got != want {
		t.Fatalf("stamped mark=%#x, want %#x (configured http mark %#x OR'd over low bits %#x)",
			got, want, httpMark, inMark&^mask)
	}
}

// TestResolveL7Marks covers the defaults-and-validation helper used by
// rewriteConstants: zero fields get shipped defaults, overrides are honored,
// and invalid combinations (http==https, bits outside the mask) are rejected.
func TestResolveL7Marks(t *testing.T) {
	h, s, m, err := resolveL7Marks(Params{})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if h != defaultL7MarkHTTP || s != defaultL7MarkHTTPS || m != defaultL7MarkMask {
		t.Fatalf("defaults: got http=%#x https=%#x mask=%#x", h, s, m)
	}

	h, s, m, err = resolveL7Marks(Params{L7MarkHTTP: 0xAB010000, L7MarkHTTPS: 0xAB020000})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if h != 0xAB010000 || s != 0xAB020000 || m != defaultL7MarkMask {
		t.Fatalf("override: got http=%#x https=%#x mask=%#x", h, s, m)
	}

	if _, _, _, err := resolveL7Marks(Params{L7MarkHTTP: 0xAB010000, L7MarkHTTPS: 0xAB010000}); err == nil {
		t.Fatal("http==https was not rejected")
	}
	if _, _, _, err := resolveL7Marks(Params{L7MarkHTTP: 0xAB010001}); err == nil {
		t.Fatal("mark with bits outside the mask was not rejected")
	}
}
