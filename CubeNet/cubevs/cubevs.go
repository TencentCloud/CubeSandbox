// Package cubevs is a library to manage CubeVS.
package cubevs

import (
	"errors"
	"net"
	"unsafe"

	"github.com/florianl/go-tc"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH localgw ../src/localgw.bpf.c -- -I../vmlinux/$GOARCH
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH mvmtap  ../src/mvmtap.bpf.c  -- -I../vmlinux/$GOARCH
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $GOARCH nodenic ../src/nodenic.bpf.c -- -I../vmlinux/$GOARCH

// Params is used to initialize CubeVS.
type Params struct {
	// IP and MAC address inside MVMs
	MVMInnerIP net.IP
	MVMMacAddr net.HardwareAddr
	// Gateway IP for MVMs
	MVMGatewayIP net.IP
	// Ifindex, IP and MAC address of the cubegw0 device (a.k.a cubedev)
	Cubegw0Ifindex uint32
	Cubegw0IP      net.IP
	Cubegw0MacAddr net.HardwareAddr
	// Ifindex, IP and MAC address of Node itself
	NodeIfindex uint32
	NodeIP      net.IP
	NodeMacAddr net.HardwareAddr
	// MAC address of the Node gateway (next hop)
	NodeGatewayMacAddr net.HardwareAddr
}

// TAPDevice contains info about a TAP device.
type TAPDevice struct {
	IP      net.IP
	ID      string
	Ifindex int
}

// mvmMetadata is used to retrieve BPF map values.
// The struct layout should be exactly the same as BPF side.
type mvmMetadata struct {
	Version        uint32
	IP             uint32
	UUID           [64]byte
	DNSPolicyFlags uint8
	Reserved       [55]uint8
}

// TCDirection is used to specified attach point of a TC filter.
type TCDirection uint32

const (
	// TCIngress attaches TC filter to the ingress path.
	TCIngress = TCDirection(tc.HandleMinIngress)
	// TCEgress attaches TC filter to the egress path.
	TCEgress = TCDirection(tc.HandleMinEgress)
)

// MVMPort is used to store and retrieve port mapping.
// The struct layout should be exactly the same as BPF side.
type MVMPort struct {
	Ifindex    uint32
	ListenPort uint16
	Reserved   uint16
}

type lpmKey struct {
	Prefixlen uint32
	IP        uint32
}

// netPolicyValueV2 mirrors struct net_policy_value_v2 on the BPF side.
type netPolicyValueV2 struct {
	ExpiresAtNS uint64
	Flags       uint8
	Reserved    [7]uint8
}

// dnsAllowKey mirrors struct dns_allow_key on the BPF side.
type dnsAllowKey struct {
	Prefixlen uint32
	Name      [maxDNSNameLen]byte
}

// dnsAllowValue mirrors struct dns_allow_value on the BPF side.
type dnsAllowValue struct {
	NameLen  uint32
	Flags    uint8
	Reserved [3]uint8
}

// dnsQueryTrackKey mirrors struct dns_query_track_key on the BPF side.
type dnsQueryTrackKey struct {
	Ifindex    uint32
	ServerIP   uint32
	SourcePort uint16
	DNSID      uint16
	Reserved   uint32
	QnameHash  uint64
}

// dnsQueryTrackValue mirrors struct dns_query_track_value on the BPF side.
type dnsQueryTrackValue struct {
	ExpiresAtNS uint64
	Flags       uint8
	Reserved    [7]uint8
}

const (
	// max length of MVM ID.
	maxIDLength = 64
	// DNS allow map layout. Must match src/cubevs.h.
	maxDNSAllowEntries = 1024
	maxDNSNameLen      = 256
	// DNS policy flags. Must match src/cubevs.h.
	dnsPolicyFlagLearningEnabled = 1 << 0
	// Network policy flags. Must match src/cubevs.h.
	netPolicyFlagL7Required = 1 << 0
	// Network policy value marker. Must match src/cubevs.h.
	netPolicyValueStatic = 1
	// programs that power CubeVS.
	programNameFromEnvoy = "from_envoy"
	programNameFromCube  = "from_cube"
	programNameFromWorld = "from_world"

	// DNS tail-call programs and slot layout. Must match src/dns_query.h.
	programNameDNSParseChunk            = "dns_parse_chunk"
	programNameDNSRevChunk              = "dns_rev_chunk"
	programNameDNSFinish                = "dns_finish"
	programNameDNSHandleResponse        = "dns_handle_response_prog"
	programNameDNSResponseFinish        = "dns_response_finish_prog"
	mapNameDNSTailCalls                 = "dns_tail_calls"
	dnsTailCallParse             uint32 = 0
	dnsTailCallReverse           uint32 = 1
	dnsTailCallFinish            uint32 = 2
	dnsTailCallResponse          uint32 = 3
	dnsTailCallResponseFinish    uint32 = 4

	// MapNameIfindexToMVMMetadata and the following are maps created by CubeVS.
	MapNameIfindexToMVMMetadata = "ifindex_to_mvmmeta"
	MapNameMVMIPToIfindex       = "mvmip_to_ifindex"
	MapNameRemotePortMapping    = "remote_port_mapping"
	MapNameLocalPortMapping     = "local_port_mapping"
	// MapNameAllowOut is the cube-v0.2.0 legacy migration source.
	MapNameAllowOut      = "allow_out"
	MapNameAllowOutV2    = "allow_out_v2"
	MapNameDenyOut       = "deny_out"
	MapNameDNSAllow      = "dns_allow"
	MapNameDNSQueryTrack = "dns_query_track"
	// constants referenced by BPF programs.
	globalNameMVMInnerIP           = "mvm_inner_ip"
	globalNameMVMMacaddrP1         = "mvm_macaddr_p1"
	globalNameMVMMacaddrP2         = "mvm_macaddr_p2"
	globalNameMVMGatewayIP         = "mvm_gateway_ip"
	globalNameCubegw0IP            = "cubegw0_ip"
	globalNameCubegw0Ifindex       = "cubegw0_ifindex"
	globalNameCubegw0MacaddrP1     = "cubegw0_macaddr_p1"
	globalNameCubegw0MacaddrP2     = "cubegw0_macaddr_p2"
	globalNameNodeIP               = "nodenic_ip"
	globalNameNodeIfindex          = "nodenic_ifindex"
	globalNameNodeMacaddrP1        = "nodenic_macaddr_p1"
	globalNameNodeMacaddrP2        = "nodenic_macaddr_p2"
	globalNameNodeGatewayMacaddrP1 = "nodegw_macaddr_p1"
	globalNameNodeGatewayMacaddrP2 = "nodegw_macaddr_p2"
	// for bpffs.
	bpfFSPath = "/sys/fs/bpf"
	// for TC.
	tcFlagDirectAction        = 1
	tcFilterHandle            = 1
	tcFilterPriority          = 1
	tcHandleClsact            = tc.HandleIngress
	tcHandleMajMask    uint32 = 0xFFFF0000
	tcHandleMinMask    uint32 = 0x0000FFFF
	tcAttrKindBPF             = "bpf"
	tcAttrKindClsact          = "clsact"
)

// Errors that will be returned to upper layer.
var (
	// ErrProgNotExist is returned when there is no specified BPF program in BPF object.
	ErrProgNotExist = errors.New("BPF program not exists")
	// ErrTooLong is returned when the provided MVM ID is too long.
	ErrTooLong = errors.New("MVM ID is too long")
)

func _() {
	{
		// static assert, make sure MVMIdentity is of size 128
		var arr [128]struct{}
		var obj mvmMetadata
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]   // error if size > 128
		_ = arr[size-128] // error if size < 128
	}

	{
		// static assert, make sure MVMPort is of size 8
		var arr [8]struct{}
		var obj MVMPort
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1] // error if size > 8
		_ = arr[size-8] // error if size < 8
	}

	{
		// static assert, make sure snatIP is of size 16
		var arr [16]struct{}
		var obj snatIP
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 16
		_ = arr[size-16] // error if size < 16
	}

	{
		// static assert, make sure SessionKey is of size 20
		var arr [20]struct{}
		var obj sessionKey
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 20
		_ = arr[size-20] // error if size < 20
	}

	{
		// static assert, make sure NATSession is of size 64
		var arr [64]struct{}
		var obj natSession
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 64
		_ = arr[size-64] // error if size < 64
	}

	{
		// static assert, make sure IngressSession is of size 16
		var arr [16]struct{}
		var obj ingressSessionValue
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 16
		_ = arr[size-16] // error if size < 16
	}

	{
		// static assert, make sure LpmKey is of size 8
		var arr [8]struct{}
		var obj lpmKey
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1] // error if size > 8
		_ = arr[size-8] // error if size < 8
	}

	{
		// static assert, make sure netPolicyValueV2 is of size 16
		var arr [16]struct{}
		var obj netPolicyValueV2
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 16
		_ = arr[size-16] // error if size < 16
	}

	{
		// static assert, make sure dnsAllowKey is of size 260
		var arr [260]struct{}
		var obj dnsAllowKey
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]   // error if size > 260
		_ = arr[size-260] // error if size < 260
	}

	{
		// static assert, make sure dnsAllowValue is of size 8
		var arr [8]struct{}
		var obj dnsAllowValue
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1] // error if size > 8
		_ = arr[size-8] // error if size < 8
	}

	{
		// static assert, make sure dnsQueryTrackKey is of size 24
		var arr [24]struct{}
		var obj dnsQueryTrackKey
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 24
		_ = arr[size-24] // error if size < 24
	}

	{
		// static assert, make sure dnsQueryTrackValue is of size 16
		var arr [16]struct{}
		var obj dnsQueryTrackValue
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 16
		_ = arr[size-16] // error if size < 16
	}
}
