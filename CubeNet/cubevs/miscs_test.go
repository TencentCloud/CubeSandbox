package cubevs

import (
	"testing"

	"github.com/cilium/ebpf"
)

func TestPopulateDNSTailCallsBindsCurrentDNSPrograms(t *testing.T) {
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

func TestPopulateDNSTailCallsNoopWhenObjectDoesNotOwnDNSPrograms(t *testing.T) {
	original := []ebpf.MapKV{{Key: uint32(9), Value: "keep"}}
	spec := &ebpf.CollectionSpec{
		Maps: map[string]*ebpf.MapSpec{
			mapNameDNSTailCalls: {
				Name:       mapNameDNSTailCalls,
				Type:       ebpf.ProgramArray,
				KeySize:    4,
				ValueSize:  4,
				MaxEntries: 16,
				Contents:   append([]ebpf.MapKV(nil), original...),
			},
		},
		Programs: map[string]*ebpf.ProgramSpec{},
	}

	if err := populateDNSTailCalls(spec); err != nil {
		t.Fatalf("populateDNSTailCalls error=%v", err)
	}

	contents := spec.Maps[mapNameDNSTailCalls].Contents
	if len(contents) != len(original) || contents[0].Key != original[0].Key || contents[0].Value != original[0].Value {
		t.Fatalf("contents=%#v, want %#v", contents, original)
	}
}
