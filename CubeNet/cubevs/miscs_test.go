package cubevs

import (
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
