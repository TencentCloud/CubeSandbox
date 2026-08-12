package cubevs

import (
	"net"
	"testing"

	"github.com/cilium/ebpf"
)

func testRewriteConstantsParams() Params {
	return Params{
		MVMInnerIP:          net.IPv4(169, 254, 68, 6),
		MVMMacAddr:          net.HardwareAddr{0x20, 0x90, 0x6f, 0xfc, 0xfc, 0xfc},
		MVMGatewayIP:        net.IPv4(169, 254, 68, 5),
		Cubegw0Ifindex:      9,
		Cubegw0IP:           net.IPv4(192, 168, 0, 1),
		Cubegw0MacAddr:      net.HardwareAddr{0x20, 0x90, 0x6f, 0xcf, 0xcf, 0xcf},
		EgressSrcMacAddr:    net.HardwareAddr{0x52, 0x54, 0x00, 0x68, 0xdd, 0x16},
		EgressDstMacAddr:    net.HardwareAddr{0xfe, 0xee, 0x32, 0x47, 0x6b, 0x93},
		EgressRedirectFlags: 0,
		NodeIfindex:         2,
		NodeIP:              net.IPv4(10, 2, 3, 4),
		NodeIPMask:          net.CIDRMask(24, 32),
		NodeMacAddr:         net.HardwareAddr{0x52, 0x54, 0x00, 0x68, 0xdd, 0x16},
		NodeGatewayMacAddr:  net.HardwareAddr{0xfe, 0xee, 0x32, 0x47, 0x6b, 0x93},
	}
}

func TestRewriteConstantsSetsNodeNetmask(t *testing.T) {
	spec, err := loadMvmtap()
	if err != nil {
		t.Fatal(err)
	}
	params := testRewriteConstantsParams()
	if err := rewriteConstants(spec, params); err != nil {
		t.Fatal(err)
	}

	variable, ok := spec.Variables[globalNameNodeNetmask]
	if !ok {
		t.Fatalf("BPF variable %q not found", globalNameNodeNetmask)
	}
	var got uint32
	if err := variable.Get(&got); err != nil {
		t.Fatal(err)
	}
	if got != 0x00ffffff {
		t.Fatalf("BPF node netmask = %#08x, want %#08x", got, uint32(0x00ffffff))
	}

	// Objects without from_cube do not use the netmask constant.
	params.NodeIPMask = nil
	localgwSpec, err := loadLocalgw()
	if err != nil {
		t.Fatal(err)
	}
	if err := rewriteConstants(localgwSpec, params); err != nil {
		t.Fatalf("rewrite unused node netmask: %v", err)
	}
}

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
