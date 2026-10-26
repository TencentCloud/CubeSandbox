package cubevs

import (
	"errors"
	"os"
	"testing"

	"github.com/cilium/ebpf"
)

func TestPopulateDNSTailCallsBindsQueryPipelinePrograms(t *testing.T) {
	spec := &ebpf.CollectionSpec{
		Maps: map[string]*ebpf.MapSpec{
			mapNameDNSTailCalls: {
				Name:       mapNameDNSTailCalls,
				Type:       ebpf.ProgramArray,
				KeySize:    4,
				ValueSize:  4,
				MaxEntries: 16,
			},
		},
		Programs: map[string]*ebpf.ProgramSpec{
			programNameDNSParseChunk: {},
			programNameDNSRevChunk:   {},
			programNameDNSFinish:     {},
		},
	}

	if err := populateDNSTailCalls(spec); err != nil {
		t.Fatalf("populateDNSTailCalls error=%v", err)
	}

	contents := spec.Maps[mapNameDNSTailCalls].Contents
	want := []ebpf.MapKV{
		{Key: dnsTailCallParse, Value: programNameDNSParseChunk},
		{Key: dnsTailCallReverse, Value: programNameDNSRevChunk},
		{Key: dnsTailCallFinish, Value: programNameDNSFinish},
	}
	if len(contents) != len(want) {
		t.Fatalf("contents length=%d, want %d: %#v", len(contents), len(want), contents)
	}
	for i := range want {
		if contents[i].Key != want[i].Key || contents[i].Value != want[i].Value {
			t.Fatalf("contents[%d]=%#v, want %#v", i, contents[i], want[i])
		}
	}
}

func TestPopulateDNSTailCallsBindsOnlyResponsePrograms(t *testing.T) {
	// nodenic owns the response handler and response finish programs;
	// populate should register only those slots and leave the
	// query-pipeline slots for the mvmtap load.
	spec := &ebpf.CollectionSpec{
		Maps: map[string]*ebpf.MapSpec{
			mapNameDNSTailCalls: {
				Name:       mapNameDNSTailCalls,
				Type:       ebpf.ProgramArray,
				KeySize:    4,
				ValueSize:  4,
				MaxEntries: 16,
			},
		},
		Programs: map[string]*ebpf.ProgramSpec{
			programNameDNSHandleResponse: {},
			programNameDNSResponseFinish: {},
		},
	}

	if err := populateDNSTailCalls(spec); err != nil {
		t.Fatalf("populateDNSTailCalls error=%v", err)
	}

	contents := spec.Maps[mapNameDNSTailCalls].Contents
	want := []ebpf.MapKV{
		{Key: dnsTailCallResponse, Value: programNameDNSHandleResponse},
		{Key: dnsTailCallResponseFinish, Value: programNameDNSResponseFinish},
	}
	if len(contents) != len(want) {
		t.Fatalf("contents length=%d, want %d: %#v", len(contents), len(want), contents)
	}
	for i := range want {
		if contents[i].Key != want[i].Key || contents[i].Value != want[i].Value {
			t.Fatalf("contents[%d]=%#v, want %#v", i, contents[i], want[i])
		}
	}
}

func TestPopulateDNSTailCallsEmptyWhenObjectDoesNotOwnDNSPrograms(t *testing.T) {
	// localgw owns none of the DNS tail-called programs; the jump table must
	// load cleanly even though shared map.h includes it in every spec.
	spec := &ebpf.CollectionSpec{
		Maps: map[string]*ebpf.MapSpec{
			mapNameDNSTailCalls: {
				Name:       mapNameDNSTailCalls,
				Type:       ebpf.ProgramArray,
				KeySize:    4,
				ValueSize:  4,
				MaxEntries: 16,
				Contents:   []ebpf.MapKV{{Key: uint32(9), Value: "stale"}},
			},
		},
		Programs: map[string]*ebpf.ProgramSpec{},
	}

	if err := populateDNSTailCalls(spec); err != nil {
		t.Fatalf("populateDNSTailCalls error=%v", err)
	}

	contents := spec.Maps[mapNameDNSTailCalls].Contents
	if len(contents) != 0 {
		t.Fatalf("contents=%#v, want empty", contents)
	}
}

// TestPersistentPolicyGenerationRecoversHalfPinned covers the F1 availability
// regression: a boot that died between pinning allow_out_v3 and dns_allow_v2
// leaves a half-pinned generation. persistentPolicyGenerationExists must treat
// it as an incomplete generation — remove the orphan pin and report "no
// generation" so Init rebuilds a consistent pair — rather than returning an
// error that permanently bricks startup (Init's recovery defer is only
// registered on the generationExists==false path, which an early error return
// skips).
func TestPersistentPolicyGenerationRecoversHalfPinned(t *testing.T) {
	cases := []struct {
		name      string
		pinAllow  bool // pin allow_out_v3
		pinDNS    bool // pin dns_allow_v2
		wantExist bool
	}{
		{"neither pinned (fresh)", false, false, false},
		{"both pinned (complete)", true, true, true},
		{"only allow_out_v3 pinned (orphan)", true, false, false},
		{"only dns_allow_v2 pinned (orphan)", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mountBpffs(t)
			if tc.pinAllow {
				m := newAllowOutV3OuterMap(t)
				if err := m.Pin(pinPath(MapNameAllowOutV3)); err != nil {
					t.Fatalf("pin %s: %v", MapNameAllowOutV3, err)
				}
			}
			if tc.pinDNS {
				m := newDNSAllowOuterMap(t)
				if err := m.Pin(pinPath(MapNameDNSAllowV2)); err != nil {
					t.Fatalf("pin %s: %v", MapNameDNSAllowV2, err)
				}
			}

			exists, err := persistentPolicyGenerationExists()
			if err != nil {
				t.Fatalf("returned error (would brick startup): %v", err)
			}
			if exists != tc.wantExist {
				t.Fatalf("exists=%t, want %t", exists, tc.wantExist)
			}

			// After the call the two pins must be consistent: both present
			// (complete) or both absent (fresh / orphan recovered).
			allowExists, err := pinnedMapExists(MapNameAllowOutV3)
			if err != nil {
				t.Fatalf("pinnedMapExists(%s): %v", MapNameAllowOutV3, err)
			}
			dnsExists, err := pinnedMapExists(MapNameDNSAllowV2)
			if err != nil {
				t.Fatalf("pinnedMapExists(%s): %v", MapNameDNSAllowV2, err)
			}
			if allowExists != dnsExists {
				t.Fatalf("pins still inconsistent after recovery: %s=%t %s=%t",
					MapNameAllowOutV3, allowExists, MapNameDNSAllowV2, dnsExists)
			}
		})
	}
}

// TestPersistentPolicyGenerationRemovesOrphanPin asserts the orphan pin is
// actually unlinked (not merely reported as absent) so the next fresh load
// rebuilds a consistent pair.
func TestPersistentPolicyGenerationRemovesOrphanPin(t *testing.T) {
	mountBpffs(t)

	orphan := newAllowOutV3OuterMap(t)
	if err := orphan.Pin(pinPath(MapNameAllowOutV3)); err != nil {
		t.Fatalf("pin %s: %v", MapNameAllowOutV3, err)
	}

	if _, err := persistentPolicyGenerationExists(); err != nil {
		t.Fatalf("returned error (would brick startup): %v", err)
	}
	if _, err := os.Stat(pinPath(MapNameAllowOutV3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan %s pin still present after recovery: err=%v", MapNameAllowOutV3, err)
	}
}
