package cubevs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

// DumpOptions controls which CubeVS business maps are dumped.
type DumpOptions struct {
	MapNames      []string
	FilterIfindex bool
	Ifindex       uint32
}

// BusinessMapsDump is the JSON-friendly dump result for CubeVS business maps.
type BusinessMapsDump struct {
	Maps   map[string]any       `json:"maps"`
	Errors []BusinessMapDumpErr `json:"errors,omitempty"`
}

// BusinessMapDumpErr records a per-map dump error while allowing other maps to be dumped.
type BusinessMapDumpErr struct {
	Map   string `json:"map"`
	Error string `json:"error"`
}

type MVMIPToIfindexDump struct {
	IP      string `json:"ip"`
	Ifindex uint32 `json:"ifindex"`
}

type MVMMetadataDump struct {
	Ifindex        uint32 `json:"ifindex"`
	Version        uint32 `json:"version"`
	IP             string `json:"ip"`
	ID             string `json:"id"`
	DNSPolicyFlags uint8  `json:"dns_policy_flags"`
	DNSLearning    bool   `json:"dns_learning_enabled"`
}

type RemotePortMappingDump struct {
	HostPort  uint16 `json:"host_port"`
	Ifindex   uint32 `json:"ifindex"`
	GuestPort uint16 `json:"guest_port"`
}

type LocalPortMappingDump struct {
	Ifindex   uint32 `json:"ifindex"`
	GuestPort uint16 `json:"guest_port"`
	HostPort  uint16 `json:"host_port"`
}

type SNATIPDump struct {
	Index    uint32 `json:"index"`
	Ifindex  uint32 `json:"ifindex"`
	IP       string `json:"ip"`
	MaxPort  uint16 `json:"max_port"`
	Reserved uint16 `json:"reserved,omitempty"`
}

type SessionKeyDump struct {
	SourceIP     string `json:"source_ip"`
	SourcePort   uint16 `json:"source_port"`
	TargetIP     string `json:"target_ip"`
	TargetPort   uint16 `json:"target_port"`
	Version      uint32 `json:"version"`
	Protocol     uint8  `json:"protocol"`
	ProtocolName string `json:"protocol_name"`
}

type EgressSessionDump struct {
	Key            SessionKeyDump `json:"key"`
	AccessTimeNS   uint64         `json:"access_time_ns"`
	ExpiresAtNS    uint64         `json:"expires_at_ns"`
	ExpiresInNS    int64          `json:"expires_in_ns"`
	ExpiresIn      string         `json:"expires_in"`
	Expired        bool           `json:"expired"`
	NodeIfindex    uint32         `json:"node_ifindex"`
	NodeIP         string         `json:"node_ip"`
	VMIfindex      uint32         `json:"vm_ifindex"`
	VMIP           string         `json:"vm_ip"`
	NodePort       uint16         `json:"node_port"`
	VMPort         uint16         `json:"vm_port"`
	State          string         `json:"state"`
	StateRaw       uint8          `json:"state_raw"`
	ActiveClose    bool           `json:"active_close"`
	ActiveCloseRaw uint8          `json:"active_close_raw"`
}

type IngressSessionDump struct {
	Key       SessionKeyDump `json:"key"`
	Version   uint32         `json:"version"`
	VMIP      string         `json:"vm_ip"`
	VMIfindex uint32         `json:"vm_ifindex,omitempty"`
	VMPort    uint16         `json:"vm_port"`
}

type PolicyMapDump struct {
	Ifindex uint32            `json:"ifindex"`
	Entries []PolicyEntryDump `json:"entries"`
}

type PolicyEntryDump struct {
	CIDR        string `json:"cidr"`
	ExpiresAtNS uint64 `json:"expires_at_ns,omitempty"`
	ExpiresInNS int64  `json:"expires_in_ns,omitempty"`
	ExpiresIn   string `json:"expires_in,omitempty"`
	Expired     bool   `json:"expired"`
	L7Required  bool   `json:"l7_required"`
	Flags       uint8  `json:"flags"`
	Static      bool   `json:"static"`
}

type DenyPolicyMapDump struct {
	Ifindex uint32                `json:"ifindex"`
	Entries []DenyPolicyEntryDump `json:"entries"`
}

type DenyPolicyEntryDump struct {
	CIDR  string `json:"cidr"`
	Value uint32 `json:"value"`
}

type DNSAllowMapDump struct {
	Ifindex         uint32             `json:"ifindex"`
	Enabled         bool               `json:"enabled"`
	LearningEnabled bool               `json:"learning_enabled"`
	Flags           uint8              `json:"flags"`
	Rules           []DNSAllowRuleDump `json:"rules"`
}

type DNSAllowRuleDump struct {
	Domain     string `json:"domain"`
	Wildcard   bool   `json:"wildcard"`
	L7Required bool   `json:"l7_required"`
	Flags      uint8  `json:"flags"`
	NameLen    uint32 `json:"name_len"`
	Prefixlen  uint32 `json:"prefixlen"`
}

type DNSQueryTrackDump struct {
	Ifindex     uint32 `json:"ifindex"`
	ServerIP    string `json:"server_ip"`
	SourcePort  uint16 `json:"source_port"`
	DNSID       uint16 `json:"dns_id"`
	QnameHash   string `json:"qname_hash"`
	ExpiresAtNS uint64 `json:"expires_at_ns"`
	ExpiresInNS int64  `json:"expires_in_ns"`
	ExpiresIn   string `json:"expires_in"`
	Expired     bool   `json:"expired"`
	L7Required  bool   `json:"l7_required"`
	Flags       uint8  `json:"flags"`
}

const mapNameSNATIPList = mapSNATIPList

var businessMapDumpOrder = []string{
	MapNameIfindexToMVMMetadata,
	MapNameMVMIPToIfindex,
	MapNameRemotePortMapping,
	MapNameLocalPortMapping,
	MapNameEgressSessions,
	MapNameIngressSessions,
	mapNameSNATIPList,
	MapNameAllowOutV2,
	MapNameDenyOut,
	MapNameDNSAllow,
	MapNameDNSQueryTrack,
}

type businessMapDumper func(DumpOptions, uint64) (any, error)

var businessMapDumpers = map[string]businessMapDumper{
	MapNameIfindexToMVMMetadata: dumpIfindexToMVMMetadata,
	MapNameMVMIPToIfindex:       dumpMVMIPToIfindex,
	MapNameRemotePortMapping:    dumpRemotePortMapping,
	MapNameLocalPortMapping:     dumpLocalPortMapping,
	MapNameEgressSessions:       dumpEgressSessions,
	MapNameIngressSessions:      dumpIngressSessions,
	mapNameSNATIPList:           dumpSNATIPList,
	MapNameAllowOutV2:           dumpAllowOutV2,
	MapNameDenyOut:              dumpDenyOut,
	MapNameDNSAllow:             dumpDNSAllow,
	MapNameDNSQueryTrack:        dumpDNSQueryTrack,
}

// BusinessMapNames returns the supported business map names in dump order.
func BusinessMapNames() []string {
	return slices.Clone(businessMapDumpOrder)
}

// DumpBusinessMaps dumps CubeVS business data from pinned eBPF maps.
func DumpBusinessMaps(opts DumpOptions) (*BusinessMapsDump, error) {
	mapNames, err := normalizeBusinessMapNames(opts.MapNames)
	if err != nil {
		return nil, err
	}

	now, err := currentNS()
	if err != nil {
		return nil, err
	}

	result := &BusinessMapsDump{
		Maps: make(map[string]any, len(mapNames)),
	}
	for _, name := range mapNames {
		dumper := businessMapDumpers[name]
		entries, err := dumper(opts, now)
		if err != nil {
			result.Errors = append(result.Errors, BusinessMapDumpErr{
				Map:   name,
				Error: err.Error(),
			})
			continue
		}
		result.Maps[name] = entries
	}

	return result, nil
}

func normalizeBusinessMapNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return BusinessMapNames(), nil
	}

	seen := make(map[string]struct{}, len(names))
	selected := make([]string, 0, len(names))
	for _, rawName := range names {
		name := strings.TrimSpace(rawName)
		if name == "" || name == "all" {
			return BusinessMapNames(), nil
		}
		if _, ok := businessMapDumpers[name]; !ok {
			return nil, fmt.Errorf("unknown business map %q", name) //nolint:err113
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		selected = append(selected, name)
	}
	return selected, nil
}

func dumpMVMIPToIfindex(opts DumpOptions, _ uint64) (any, error) {
	m, err := loadPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]MVMIPToIfindexDump, 0)
	var key uint32
	var ifindex uint32
	iter := m.Iterate()
	for iter.Next(&key, &ifindex) {
		if !ifindexMatches(opts, ifindex) {
			continue
		}
		entries = append(entries, MVMIPToIfindexDump{
			IP:      uint32ToIP(key).String(),
			Ifindex: ifindex,
		})
	}
	return entries, wrapIterErr(iter.Err(), MapNameMVMIPToIfindex)
}

func dumpIfindexToMVMMetadata(opts DumpOptions, _ uint64) (any, error) {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]MVMMetadataDump, 0)
	var ifindex uint32
	var meta mvmMetadata
	iter := m.Iterate()
	for iter.Next(&ifindex, &meta) {
		if !ifindexMatches(opts, ifindex) {
			continue
		}
		entries = append(entries, MVMMetadataDump{
			Ifindex:        ifindex,
			Version:        meta.Version,
			IP:             uint32ToIP(meta.IP).String(),
			ID:             bytesToString(meta.UUID[:]),
			DNSPolicyFlags: meta.DNSPolicyFlags,
			DNSLearning:    dnsPolicyLearningEnabled(meta.DNSPolicyFlags),
		})
	}
	return entries, wrapIterErr(iter.Err(), MapNameIfindexToMVMMetadata)
}

func dumpRemotePortMapping(opts DumpOptions, _ uint64) (any, error) {
	m, err := loadPinnedMap(MapNameRemotePortMapping)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]RemotePortMappingDump, 0)
	var hostPort uint16
	var mvmPort MVMPort
	iter := m.Iterate()
	for iter.Next(&hostPort, &mvmPort) {
		if !ifindexMatches(opts, mvmPort.Ifindex) {
			continue
		}
		entries = append(entries, RemotePortMappingDump{
			HostPort:  ntohs(hostPort),
			Ifindex:   mvmPort.Ifindex,
			GuestPort: ntohs(mvmPort.ListenPort),
		})
	}
	return entries, wrapIterErr(iter.Err(), MapNameRemotePortMapping)
}

func dumpLocalPortMapping(opts DumpOptions, _ uint64) (any, error) {
	m, err := loadPinnedMap(MapNameLocalPortMapping)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]LocalPortMappingDump, 0)
	var mvmPort MVMPort
	var hostPort uint16
	iter := m.Iterate()
	for iter.Next(&mvmPort, &hostPort) {
		if !ifindexMatches(opts, mvmPort.Ifindex) {
			continue
		}
		entries = append(entries, LocalPortMappingDump{
			Ifindex:   mvmPort.Ifindex,
			GuestPort: ntohs(mvmPort.ListenPort),
			HostPort:  ntohs(hostPort),
		})
	}
	return entries, wrapIterErr(iter.Err(), MapNameLocalPortMapping)
}

func dumpSNATIPList(opts DumpOptions, _ uint64) (any, error) {
	m, err := loadPinnedMap(mapNameSNATIPList)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]SNATIPDump, 0)
	for i := uint32(0); i < maxSNATIPs; i++ {
		var value snatIP
		if err := m.Lookup(&i, &value); err != nil {
			if errors.Is(err, ebpf.ErrKeyNotExist) {
				continue
			}
			return nil, fmt.Errorf("map.Lookup failed: %w, name: %s, index: %d", err, mapNameSNATIPList, i)
		}
		if !ifindexMatches(opts, value.Ifindex) {
			continue
		}
		entries = append(entries, SNATIPDump{
			Index:    i,
			Ifindex:  value.Ifindex,
			IP:       uint32ToIP(value.IP).String(),
			MaxPort:  value.MaxPort,
			Reserved: value.Reserved,
		})
	}
	return entries, nil
}

func dumpEgressSessions(opts DumpOptions, now uint64) (any, error) {
	m, err := loadPinnedMap(MapNameEgressSessions)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]EgressSessionDump, 0)
	var key sessionKey
	var value natSession
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		if !ifindexMatches(opts, value.VMIfindex) {
			continue
		}
		expiresAt := value.AccessTime + sessionTimeoutNS(&key, &value)
		entries = append(entries, EgressSessionDump{
			Key:            dumpSessionKey(key),
			AccessTimeNS:   value.AccessTime,
			ExpiresAtNS:    expiresAt,
			ExpiresInNS:    remainingNS(expiresAt, now),
			ExpiresIn:      remainingDuration(expiresAt, now),
			Expired:        expiresAt <= now,
			NodeIfindex:    value.NodeIfindex,
			NodeIP:         uint32ToIP(value.NodeIP).String(),
			VMIfindex:      value.VMIfindex,
			VMIP:           uint32ToIP(value.VMIP).String(),
			NodePort:       ntohs(value.NodePort),
			VMPort:         ntohs(value.VMPort),
			State:          sessionStateString(key.Protocol, value.State),
			StateRaw:       value.State,
			ActiveClose:    value.ActiveClose != 0,
			ActiveCloseRaw: value.ActiveClose,
		})
	}
	return entries, wrapIterErr(iter.Err(), MapNameEgressSessions)
}

func dumpIngressSessions(opts DumpOptions, _ uint64) (any, error) {
	ipIfindex, err := dumpLoadMVMIPIfindexIndex(opts.FilterIfindex)
	if err != nil {
		return nil, err
	}

	m, err := loadPinnedMap(MapNameIngressSessions)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]IngressSessionDump, 0)
	var key sessionKey
	var value ingressSessionValue
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		vmIfindex := ipIfindex[value.VMIP]
		if opts.FilterIfindex && vmIfindex != opts.Ifindex {
			continue
		}
		entries = append(entries, IngressSessionDump{
			Key:       dumpSessionKey(key),
			Version:   value.Version,
			VMIP:      uint32ToIP(value.VMIP).String(),
			VMIfindex: vmIfindex,
			VMPort:    ntohs(value.VMPort),
		})
	}
	return entries, wrapIterErr(iter.Err(), MapNameIngressSessions)
}

func dumpAllowOutV2(opts DumpOptions, now uint64) (any, error) {
	return dumpPolicyMap(opts, now, MapNameAllowOutV2)
}

func dumpPolicyMap(opts DumpOptions, now uint64, mapName string) (any, error) {
	outer, err := loadPinnedMap(mapName)
	if err != nil {
		return nil, err
	}
	defer outer.Close()

	entries := make([]PolicyMapDump, 0)
	err = dumpSelectedInnerMapIDs(opts, outer, mapName, func(ifindex uint32, innerMapID uint32) error {
		innerEntries, err := dumpPolicyInnerMap(innerMapID, now)
		if err != nil {
			return fmt.Errorf("dump %s inner map failed: %w, ifindex: %d", mapName, err, ifindex)
		}
		entries = append(entries, PolicyMapDump{Ifindex: ifindex, Entries: innerEntries})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func dumpPolicyInnerMap(innerMapID uint32, now uint64) ([]PolicyEntryDump, error) {
	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return nil, fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	entries := make([]PolicyEntryDump, 0)
	seen := make(map[lpmKey]struct{})
	var key lpmKey
	var value netPolicyValueV2
	iter := inner.Iterate()
	for iter.Next(&key, &value) {
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("policy inner map iteration returned duplicate key: %s", dumpLPMCIDR(key)) //nolint:err113
		}
		seen[key] = struct{}{}
		if len(seen) > maxNetPolicyEntries {
			return nil, fmt.Errorf("policy inner map iteration exceeded max entries: %d", maxNetPolicyEntries) //nolint:err113
		}

		entry := PolicyEntryDump{
			CIDR:       dumpLPMCIDR(key),
			Expired:    netPolicyValueV2Expired(value, now),
			L7Required: value.Flags&uint8(netPolicyFlagL7Required) != 0,
			Flags:      value.Flags,
			Static:     value.ExpiresAtNS == 0,
		}
		if value.ExpiresAtNS != 0 {
			entry.ExpiresAtNS = value.ExpiresAtNS
			entry.ExpiresInNS = remainingNS(value.ExpiresAtNS, now)
			entry.ExpiresIn = remainingDuration(value.ExpiresAtNS, now)
		}
		entries = append(entries, entry)
	}
	return entries, wrapIterErr(iter.Err(), "policy inner")
}

func dumpDenyOut(opts DumpOptions, _ uint64) (any, error) {
	outer, err := loadPinnedMap(MapNameDenyOut)
	if err != nil {
		return nil, err
	}
	defer outer.Close()

	entries := make([]DenyPolicyMapDump, 0)
	err = dumpSelectedInnerMapIDs(opts, outer, MapNameDenyOut, func(ifindex uint32, innerMapID uint32) error {
		innerEntries, err := dumpDenyInnerMap(innerMapID)
		if err != nil {
			return fmt.Errorf("dump %s inner map failed: %w, ifindex: %d", MapNameDenyOut, err, ifindex)
		}
		entries = append(entries, DenyPolicyMapDump{Ifindex: ifindex, Entries: innerEntries})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func dumpDenyInnerMap(innerMapID uint32) ([]DenyPolicyEntryDump, error) {
	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return nil, fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	entries := make([]DenyPolicyEntryDump, 0)
	seen := make(map[lpmKey]struct{})
	var key lpmKey
	var value uint32
	iter := inner.Iterate()
	for iter.Next(&key, &value) {
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("deny_out inner map iteration returned duplicate key: %s", dumpLPMCIDR(key)) //nolint:err113
		}
		seen[key] = struct{}{}
		if len(seen) > maxNetPolicyEntries {
			return nil, fmt.Errorf("deny_out inner map iteration exceeded max entries: %d", maxNetPolicyEntries) //nolint:err113
		}

		entries = append(entries, DenyPolicyEntryDump{
			CIDR:  dumpLPMCIDR(key),
			Value: value,
		})
	}
	return entries, wrapIterErr(iter.Err(), "deny_out inner")
}

func dumpDNSAllow(opts DumpOptions, _ uint64) (any, error) {
	outer, err := loadPinnedMap(MapNameDNSAllow)
	if err != nil {
		return nil, err
	}
	defer outer.Close()

	metadata, err := dumpLoadMVMMetadataIndex()
	if err != nil {
		return nil, err
	}

	entries := make([]DNSAllowMapDump, 0)
	err = dumpSelectedInnerMapIDs(opts, outer, MapNameDNSAllow, func(ifindex uint32, innerMapID uint32) error {
		innerDump, err := dumpDNSAllowInnerMap(innerMapID)
		if err != nil {
			return fmt.Errorf("dump %s inner map failed: %w, ifindex: %d", MapNameDNSAllow, err, ifindex)
		}
		innerDump.Ifindex = ifindex
		if meta, ok := metadata[ifindex]; ok {
			applyDNSPolicyModeDump(&innerDump, meta.DNSPolicyFlags)
		}
		entries = append(entries, innerDump)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func dumpDNSAllowInnerMap(innerMapID uint32) (DNSAllowMapDump, error) {
	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return DNSAllowMapDump{}, fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	result := DNSAllowMapDump{Rules: make([]DNSAllowRuleDump, 0)}
	seen := make(map[dnsAllowKey]struct{})
	var key dnsAllowKey
	var value dnsAllowValue
	iter := inner.Iterate()
	for iter.Next(&key, &value) {
		if _, ok := seen[key]; ok {
			return DNSAllowMapDump{}, fmt.Errorf("dns_allow inner map iteration returned duplicate key, prefixlen: %d", key.Prefixlen) //nolint:err113
		}
		seen[key] = struct{}{}
		if len(seen) > maxDNSAllowEntries {
			return DNSAllowMapDump{}, fmt.Errorf("dns_allow inner map iteration exceeded max entries: %d", maxDNSAllowEntries) //nolint:err113
		}

		if key.Prefixlen == 0 && value.NameLen == 0 {
			continue
		}
		rule, err := dumpDNSAllowRule(key, value)
		if err != nil {
			return DNSAllowMapDump{}, err
		}
		result.Rules = append(result.Rules, rule)
	}
	return result, wrapIterErr(iter.Err(), "dns_allow inner")
}

func applyDNSPolicyModeDump(result *DNSAllowMapDump, flags uint8) {
	result.Enabled = flags != 0
	result.LearningEnabled = dnsPolicyLearningEnabled(flags)
	result.Flags = flags
}

func dumpDNSQueryTrack(opts DumpOptions, now uint64) (any, error) {
	m, err := loadPinnedMap(MapNameDNSQueryTrack)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	entries := make([]DNSQueryTrackDump, 0)
	var key dnsQueryTrackKey
	var value dnsQueryTrackValue
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		if !ifindexMatches(opts, key.Ifindex) {
			continue
		}
		entries = append(entries, DNSQueryTrackDump{
			Ifindex:     key.Ifindex,
			ServerIP:    uint32ToIP(key.ServerIP).String(),
			SourcePort:  ntohs(key.SourcePort),
			DNSID:       ntohs(key.DNSID),
			QnameHash:   fmt.Sprintf("0x%016x", key.QnameHash),
			ExpiresAtNS: value.ExpiresAtNS,
			ExpiresInNS: remainingNS(value.ExpiresAtNS, now),
			ExpiresIn:   remainingDuration(value.ExpiresAtNS, now),
			Expired:     value.ExpiresAtNS <= now,
			L7Required:  value.Flags&uint8(netPolicyFlagL7Required) != 0,
			Flags:       value.Flags,
		})
	}
	return entries, wrapIterErr(iter.Err(), MapNameDNSQueryTrack)
}

func dumpSelectedInnerMapIDs(opts DumpOptions, outer *ebpf.Map, mapName string, visit func(uint32, uint32) error) error {
	if opts.FilterIfindex {
		return dumpSelectedInnerMapID(outer, mapName, opts.Ifindex, visit)
	}

	ifindexes, err := dumpLoadActiveIfindexes()
	if err != nil {
		return fmt.Errorf("load active sandbox ifindexes failed: %w", err)
	}
	for _, ifindex := range ifindexes {
		if err := dumpSelectedInnerMapID(outer, mapName, ifindex, visit); err != nil {
			return err
		}
	}
	return nil
}

func dumpSelectedInnerMapID(outer *ebpf.Map, mapName string, ifindex uint32, visit func(uint32, uint32) error) error {
	var innerMapID uint32
	err := outer.Lookup(&ifindex, &innerMapID)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("map.Lookup failed: %w, name: %s, ifindex: %d", err, mapName, ifindex)
	}
	return visit(ifindex, innerMapID)
}

func dumpLoadActiveIfindexes() ([]uint32, error) {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	ifindexes := make([]uint32, 0)
	var ifindex uint32
	var meta mvmMetadata
	iter := m.Iterate()
	for iter.Next(&ifindex, &meta) {
		ifindexes = append(ifindexes, ifindex)
	}
	if err := iter.Err(); err != nil {
		return nil, wrapIterErr(err, MapNameIfindexToMVMMetadata)
	}
	slices.Sort(ifindexes)
	return ifindexes, nil
}

func dumpLoadMVMMetadataIndex() (map[uint32]mvmMetadata, error) {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	metadata := make(map[uint32]mvmMetadata)
	var ifindex uint32
	var meta mvmMetadata
	iter := m.Iterate()
	for iter.Next(&ifindex, &meta) {
		metadata[ifindex] = meta
	}
	if err := iter.Err(); err != nil {
		return nil, wrapIterErr(err, MapNameIfindexToMVMMetadata)
	}
	return metadata, nil
}

func dumpLoadMVMIPIfindexIndex(required bool) (map[uint32]uint32, error) {
	m, err := loadPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		if required {
			return nil, err
		}
		return map[uint32]uint32{}, nil
	}
	defer m.Close()

	result := make(map[uint32]uint32)
	var ip uint32
	var ifindex uint32
	iter := m.Iterate()
	for iter.Next(&ip, &ifindex) {
		result[ip] = ifindex
	}
	if err := iter.Err(); err != nil {
		if required {
			return nil, wrapIterErr(err, MapNameMVMIPToIfindex)
		}
		return map[uint32]uint32{}, nil
	}
	return result, nil
}

func dumpSessionKey(key sessionKey) SessionKeyDump {
	return SessionKeyDump{
		SourceIP:     uint32ToIP(key.SourceIP).String(),
		SourcePort:   ntohs(key.SourcePort),
		TargetIP:     uint32ToIP(key.TargetIP).String(),
		TargetPort:   ntohs(key.TargetPort),
		Version:      key.Version,
		Protocol:     key.Protocol,
		ProtocolName: protocolName(key.Protocol),
	}
}

func sessionTimeoutNS(key *sessionKey, sess *natSession) uint64 {
	switch key.Protocol {
	case unix.IPPROTO_UDP:
		return sess.udpTimeout()
	case unix.IPPROTO_ICMP:
		return sess.icmpTimeout()
	default:
		return sess.tcpTimeout()
	}
}

func protocolName(protocol uint8) string {
	switch protocol {
	case unix.IPPROTO_TCP:
		return "tcp"
	case unix.IPPROTO_UDP:
		return "udp"
	case unix.IPPROTO_ICMP:
		return "icmp"
	default:
		return fmt.Sprintf("unknown(%d)", protocol)
	}
}

func dumpLPMCIDR(key lpmKey) string {
	return fmt.Sprintf("%s/%d", uint32ToIP(key.IP).String(), key.Prefixlen)
}

func dumpDNSAllowRule(key dnsAllowKey, value dnsAllowValue) (DNSAllowRuleDump, error) {
	nameLen := int(value.NameLen)
	if nameLen <= 0 || nameLen > len(key.Name) {
		return DNSAllowRuleDump{}, fmt.Errorf("invalid dns_allow name length: %d", value.NameLen) //nolint:err113
	}

	terminator := key.Name[nameLen-1]
	wildcard := terminator == '.'
	if terminator != 0 && terminator != '.' {
		return DNSAllowRuleDump{}, fmt.Errorf("invalid dns_allow rule terminator: %d", terminator) //nolint:err113
	}

	reversedName := key.Name[:nameLen-1]
	name := make([]byte, len(reversedName))
	for i := range reversedName {
		name[i] = reversedName[len(reversedName)-1-i]
	}
	domain := string(name)
	if wildcard {
		domain = "*." + domain
	}

	return DNSAllowRuleDump{
		Domain:     domain,
		Wildcard:   wildcard,
		L7Required: value.Flags&uint8(netPolicyFlagL7Required) != 0,
		Flags:      value.Flags,
		NameLen:    value.NameLen,
		Prefixlen:  key.Prefixlen,
	}, nil
}

func ifindexMatches(opts DumpOptions, ifindex uint32) bool {
	return !opts.FilterIfindex || opts.Ifindex == ifindex
}

func remainingNS(expiresAt, now uint64) int64 {
	if expiresAt >= now {
		return int64(expiresAt - now)
	}
	return -int64(now - expiresAt)
}

func remainingDuration(expiresAt, now uint64) string {
	return (time.Duration(remainingNS(expiresAt, now)) * time.Nanosecond).String()
}

func wrapIterErr(err error, name string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("map.Iterate failed: %w, name: %s", err, name)
}
