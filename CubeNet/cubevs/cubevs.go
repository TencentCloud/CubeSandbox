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
	// Ordinary egress L2 rewrite and redirect behavior.
	EgressSrcMacAddr    net.HardwareAddr
	EgressDstMacAddr    net.HardwareAddr
	EgressRedirectFlags uint64
	// Ifindex and MAC address of the cube-router device. Ifindex 0 disables
	// cube-router hook attachment.
	CubeRouterIfindex uint32
	// Ifindex, IP and MAC address of Node itself
	NodeIfindex uint32
	NodeIP      net.IP
	NodeIPMask  net.IPMask
	NodeMacAddr net.HardwareAddr
	// MAC address of the Node gateway (next hop)
	NodeGatewayMacAddr net.HardwareAddr
	// L7 skb->mark values stamped by the dataplane and matched by the iptables
	// TPROXY rules. Zero means "use the shipped default"; override from the
	// install-time config shared with the iptables init script.
	L7MarkHTTP  uint32
	L7MarkHTTPS uint32
	L7MarkMask  uint32
}

// TAPDevice contains info about a TAP device.
type TAPDevice struct {
	IP      net.IP
	ID      string
	Ifindex int
}

// mvmMetadata is used to retrieve BPF map values.
// The struct layout should be exactly the same as BPF side.
// mvmMetadata mirrors struct mvm_meta on the BPF side. PolicyVersion is the
// per-sandbox network-policy generation; the datapath compares it against the
// copy cached in each nat_session to decide whether an established flow needs
// re-evaluating. Version is a different thing entirely (TAP generation, part of
// session_key) and must not be reused for it.
type mvmMetadata struct {
	Version        uint32
	IP             uint32
	UUID           [64]byte
	DNSPolicyFlags uint8
	Reserved0      [3]uint8
	PolicyVersion  uint32
	Reserved       [48]uint8
}

// TCDirection is used to specified attach point of a TC filter.
type TCDirection uint32

const (
	// TCIngress attaches TC filter to the ingress path.
	TCIngress = TCDirection(tc.HandleMinIngress)
	// TCEgress attaches TC filter to the egress path.
	TCEgress = TCDirection(tc.HandleMinEgress)
	// BPFRedirectFlagIngress redirects packets to the peer device ingress path.
	BPFRedirectFlagIngress uint64 = 1
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

// l7PortEntry mirrors struct l7_port_entry on the BPF side. Port is stored in
// network byte order to match tcphdr->dest so the datapath can compare without
// an endianness conversion. Scheme is one of L7SchemeHTTP / L7SchemeHTTPS.
type l7PortEntry struct {
	Port   uint16 // network byte order
	Scheme uint8
	Pad    uint8
}

// netPolicyValueV2 mirrors struct net_policy_value_v2 on the BPF side. This is
// the legacy 16-byte layout, read only when migrating a pre-v3 allow_out_v2
// map to allow_out_v3; the current dataplane uses netPolicyValueV3.
type netPolicyValueV2 struct {
	ExpiresAtNS uint64
	Flags       uint8
	Reserved    [7]uint8
}

// lpmKeyV3 mirrors struct lpm_key_v3 on the BPF side. IP and port are
// in network byte order; a single longest-prefix lookup resolves exact
// (ip, port) (prefixlen 48), ip-only (prefixlen 32), or ip/mask
// (prefixlen < 32) rules. Pad keeps the LPM data payload 4-byte aligned.
type lpmKeyV3 struct {
	Prefixlen uint32
	IP        uint32
	Port      uint16
	Pad       uint16
}

// netPolicyValueV3 mirrors struct net_policy_value_v3 on the BPF side.
// Unlike netPolicyValueV2, the port lives in the key, so the scheme is
// resolved at insert time and stored directly here. KeyPrefixlen records
// the prefixlen of the key this value was written under: LPM lookups are
// longest-prefix, so writers merging with an existing entry for the EXACT
// same key must compare it against their key's prefixlen first.
type netPolicyValueV3 struct {
	ExpiresAtNS  uint64
	Flags        uint8
	Scheme       uint8
	KeyPrefixlen uint8
	Reserved     [5]uint8
}

// dnsAllowKey mirrors struct dns_allow_key on the BPF side.
type dnsAllowKey struct {
	Prefixlen uint32
	Name      [maxDNSNameLen]byte
}

// dnsAllowValue mirrors struct dns_allow_value on the BPF side. Ports carries
// the (port, scheme) tuples the userspace built from all rules sharing this
// host. PortCount == 0 means "unspecified, default 80/443".
type dnsAllowValue struct {
	NameLen   uint32
	Flags     uint8
	PortCount uint8
	Reserved  [2]uint8
	Ports     [maxL7PortsPerHost]l7PortEntry
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
// Ports is copied from the matched dns_allow_value at query time so the
// response handler can rebuild net_policy_value_v3 without a second lookup.
type dnsQueryTrackValue struct {
	ExpiresAtNS uint64
	Flags       uint8
	PortCount   uint8
	Reserved    [6]uint8
	Ports       [maxL7PortsPerHost]l7PortEntry
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
	// netPolicyFlagL3Allowed marks a domain present in both plain allow_out
	// and an L7 rule, so the datapath learns the plain /32 any-port entry
	// alongside the L7 /48 entries. Must match src/cubevs.h.
	netPolicyFlagL3Allowed = 1 << 1
	// L7 scheme values in dns_allow_value / net_policy_value_v3 per-port
	// entries. Must match L7_SCHEME_* in src/cubevs.h.
	L7SchemeNone  uint8 = 0
	L7SchemeHTTP  uint8 = 1
	L7SchemeHTTPS uint8 = 2
	// Maximum number of (port, scheme) tuples per host. Must match
	// MAX_L7_PORTS_PER_HOST in src/cubevs.h.
	maxL7PortsPerHost = 8
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
	MapNameAllowOutV2    = "allow_out_v2" // legacy 16-byte policy value
	MapNameAllowOutV3    = "allow_out_v3" // current 16-byte policy value
	MapNameDenyOut       = "deny_out"
	MapNameDNSAllow      = "dns_allow"    // legacy 8-byte DNS value
	MapNameDNSAllowV2    = "dns_allow_v2" // current 40-byte DNS value
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
	globalNameEgressSMacaddrP1     = "egress_smacaddr_p1"
	globalNameEgressSMacaddrP2     = "egress_smacaddr_p2"
	globalNameEgressDMacaddrP1     = "egress_dmacaddr_p1"
	globalNameEgressDMacaddrP2     = "egress_dmacaddr_p2"
	globalNameEgressRedirectFlags  = "egress_redirect_flags"
	globalNameNodeIP               = "nodenic_ip"
	globalNameNodeNetmask          = "nodenic_netmask"
	globalNameNodeIfindex          = "nodenic_ifindex"
	globalNameNodeMacaddrP1        = "nodenic_macaddr_p1"
	globalNameNodeMacaddrP2        = "nodenic_macaddr_p2"
	globalNameNodeGatewayMacaddrP1 = "nodegw_macaddr_p1"
	globalNameNodeGatewayMacaddrP2 = "nodegw_macaddr_p2"
	globalNameCubeL7MarkHTTP       = "cube_l7_mark_http"
	globalNameCubeL7MarkHTTPS      = "cube_l7_mark_https"
	globalNameCubeL7MarkMask       = "cube_l7_mark_mask"
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
		// static assert, make sure LpmKeyV3 is of size 12
		var arr [12]struct{}
		var obj lpmKeyV3
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 12
		_ = arr[size-12] // error if size < 12
	}

	{
		// static assert, make sure l7PortEntry is of size 4
		var arr [4]struct{}
		var obj l7PortEntry
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1] // error if size > 4
		_ = arr[size-4] // error if size < 4
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
		// static assert, make sure dnsAllowValue is of size 40
		var arr [40]struct{}
		var obj dnsAllowValue
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 40
		_ = arr[size-40] // error if size < 40
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
		// static assert, make sure dnsQueryTrackValue is of size 48
		var arr [48]struct{}
		var obj dnsQueryTrackValue
		const size = unsafe.Sizeof(obj)
		_ = arr[size-1]  // error if size > 48
		_ = arr[size-48] // error if size < 48
	}
}
