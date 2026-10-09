package cubevs

import (
	"errors"
	"fmt"
	"net"

	"github.com/cilium/ebpf"
)

// L7Target describes one (host-or-CIDR, port, scheme) tuple carried inside
// MVMOptions.L7AllowOut. Host is either a domain (with optional wildcard) or
// an IPv4 / IPv4-CIDR literal. Port == 0 with SchemeNone signals the legacy
// default port set {80/http, 443/https}; when Port > 0, Scheme must be
// L7SchemeHTTP or L7SchemeHTTPS.
type L7Target struct {
	Host   string
	Port   uint16 // 0 = unspecified (default port set)
	Scheme uint8  // one of L7SchemeNone / L7SchemeHTTP / L7SchemeHTTPS
}

type MVMOptions struct {
	AllowInternetAccess *bool
	AllowOut            *[]string   // CIDR, IP, or domain
	L7AllowOut          *[]L7Target // Host + optional (port, scheme) for L7 policy handling
	DenyOut             *[]string   // CIDR or IP
}

type tapMetadataMapOps interface {
	Lookup(key, valueOut interface{}) error
	Delete(key interface{}) error
}

// ListTAPDevices lists all TAP devices that managed by CubeVS.
func ListTAPDevices() ([]TAPDevice, error) {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	var taps []TAPDevice
	var key uint32
	var value mvmMetadata
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		taps = append(taps, TAPDevice{
			IP:      uint32ToIP(value.IP),
			ID:      bytesToString(value.UUID[:]),
			Ifindex: int(key),
		})
	}
	err = iter.Err()
	if err != nil {
		return nil, fmt.Errorf("map.Iterate failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}

	return taps, nil
}

// AddTAPDevice adds a new device to CubeVS.
func AddTAPDevice(ifindex uint32, ip net.IP, id string, version uint32, opts MVMOptions) error {
	if err := UpsertTAPDeviceMetadata(ifindex, ip, id, version); err != nil {
		return err
	}
	if err := setDNSTrackRateLimit(ifindex); err != nil {
		_ = DeleteTAPDevice(ifindex, ip)
		return err
	}
	if err := applyNetPolicy(ifindex, opts); err != nil {
		_ = DeleteTAPDevice(ifindex, ip)
		return err
	}
	return nil
}

// setDNSTrackRateLimit installs the sandbox's zeroed DNS query-tracking
// counter. It is installed before any policy so the ceiling is in place for the
// sandbox's very first query — a missing entry means "unlimited" on the BPF
// side, which is the right default for an unregistered ifindex but not for a
// live one.
func setDNSTrackRateLimit(ifindex uint32) error {
	m, err := loadPinnedMap(MapNameDNSTrackRL)
	if err != nil {
		return err
	}
	defer m.Close()

	// All-zero: the first BPF access sees window_end_ns == 0 and opens a fresh
	// window. The Lock field's contents belong to the kernel.
	state := dnsTrackRLState{}
	if err := m.Update(&ifindex, &state, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameDNSTrackRL)
	}
	return nil
}

// deleteDNSTrackRateLimit drops the sandbox's counter on teardown so a reused
// ifindex starts from a fresh window rather than inheriting a stale one.
func deleteDNSTrackRateLimit(ifindex uint32) error {
	m, err := loadPinnedMap(MapNameDNSTrackRL)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Delete(&ifindex); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameDNSTrackRL)
	}
	return nil
}

// UpsertTAPDeviceMetadata registers or refreshes TAP metadata without touching
// per-sandbox policy maps. Recovery paths use this to repair metadata while
// preserving allow_out_v3, deny_out and dns_allow_v2 contents.
func UpsertTAPDeviceMetadata(ifindex uint32, ip net.IP, id string, version uint32) error {
	if len(id) > maxIDLength {
		return ErrTooLong
	}

	mvmIP := ipToUint32(ip)
	// A fresh TAP starts at generation 0, which is also what metadata and
	// sessions written before this field existed carry. Matching them keeps an
	// upgrade from re-checking anything: those flows were admitted under the
	// policy the sandbox still has, so the only thing a forced re-check could
	// find is a DNS-learned allow entry that has since aged out -- and it would
	// drop the flow for it. The first real update bumps the generation and makes
	// them stale, which is when a re-check is actually owed.
	mvmID := mvmMetadata{
		IP:            mvmIP,
		UUID:          stringToByteArray(id),
		Version:       version,
		PolicyVersion: 0,
	}

	// ifindex <-> MVM metadata (IP, ID and tunnels)
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}
	defer m.Close()

	var oldMVMID mvmMetadata
	oldMVMIP := uint32(0)
	if err := m.Lookup(&ifindex, &oldMVMID); err == nil {
		oldMVMIP = oldMVMID.IP
		mvmID.DNSPolicyFlags = oldMVMID.DNSPolicyFlags
		mvmID.Reserved = oldMVMID.Reserved
		// Carry the policy generation across metadata rewrites (recovery bumps
		// Version on every restart). Resetting it would make every live session
		// look stale and force a re-check storm on a dense node.
		mvmID.PolicyVersion = oldMVMID.PolicyVersion
	} else if !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}

	err = m.Update(&ifindex, &mvmID, ebpf.UpdateAny)
	if err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}

	// MVM IP <-> ifindex
	m, err = loadPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		return err
	}
	defer m.Close()

	if oldMVMIP != 0 && oldMVMIP != mvmIP {
		if err := m.Delete(&oldMVMIP); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameMVMIPToIfindex)
		}
	}

	err = m.Update(&mvmIP, &ifindex, ebpf.UpdateAny)
	if err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameMVMIPToIfindex)
	}

	return nil
}

// BumpMvmVersion increments the sandbox's rollback generation
// (ifindex_to_mvmmeta[ifindex].version) via a read-modify-write of the map
// value, preserving IP/UUID/DNSPolicyFlags/Reserved. A sandbox rollback calls
// this after the guest has been restored and resumed; the dataplane then treats
// a session whose stamped gen differs from the current version as stale and
// resets it. This is a map RMW (not the process-local registration counter) so
// it stays monotonic per ifindex even across a Cubelet restart that reset that
// counter — otherwise a bump could write a value equal to the registration
// version and make the rollback invisible.
func BumpMvmVersion(ifindex uint32) error {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}
	defer m.Close()
	return bumpMvmVersion(m, ifindex)
}

// LookupIfindexByIP resolves a sandbox's TAP ifindex from its IP via the
// mvmip_to_ifindex map. O(1), unlike listing and scanning every TAP device.
func LookupIfindexByIP(ip net.IP) (uint32, error) {
	m, err := loadPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		return 0, err
	}
	defer m.Close()

	key := ipToUint32(ip)
	var ifindex uint32
	if err := m.Lookup(&key, &ifindex); err != nil {
		return 0, fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameMVMIPToIfindex)
	}
	return ifindex, nil
}

// mvmVersionMapOps is the subset of the metadata map used to bump the version;
// it is an interface so the read-modify-write can be unit-tested against a fake.
type mvmVersionMapOps interface {
	Lookup(key, valueOut interface{}) error
	Update(key, value any, flags ebpf.MapUpdateFlags) error
}

// bumpMvmVersion does the read-modify-write on the metadata map value. It
// preserves IP/UUID/DNSPolicyFlags/Reserved and only increments Version.
func bumpMvmVersion(m mvmVersionMapOps, ifindex uint32) error {
	var meta mvmMetadata
	if err := m.Lookup(&ifindex, &meta); err != nil {
		return fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	meta.Version++
	if err := m.Update(&ifindex, &meta, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	return nil
}

// UpsertTAPDevice registers a TAP device and replaces its desired policy state.
func UpsertTAPDevice(ifindex uint32, ip net.IP, id string, version uint32, opts MVMOptions) error {
	if err := UpsertTAPDeviceMetadata(ifindex, ip, id, version); err != nil {
		return err
	}
	return replaceNetPolicy(ifindex, opts)
}

// DeleteTAPDevice removes all CubeVS state for a TAP device. It is a compatibility
// wrapper for callers that still want the old all-in-one behavior; new runtime
// cleanup paths should compose CleanupTAPDevicePolicy and DeleteTAPDeviceMetadata
// explicitly so policy cleanup and metadata cleanup remain separate steps.
func DeleteTAPDevice(ifindex uint32, ip net.IP) error {
	if err := CleanupTAPDevicePolicy(ifindex); err != nil {
		return err
	}
	if err := deleteDNSTrackRateLimit(ifindex); err != nil {
		return err
	}
	return DeleteTAPDeviceMetadata(ifindex, ip)
}

// DeleteTAPDeviceMetadata removes TAP identity metadata from CubeVS without touching
// policy maps. Missing keys are treated as already-clean so cleanup paths remain
// idempotent.
func DeleteTAPDeviceMetadata(ifindex uint32, ip net.IP) error {
	mvmIP := ipToUint32(ip)

	// ifindex <-> MVM metadata
	ifindexMap, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}
	defer ifindexMap.Close()

	// MVM IP <-> ifindex
	ipMap, err := loadPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		return err
	}
	defer ipMap.Close()

	return deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, ifindex, mvmIP)
}

func deleteTAPDeviceMetadataEntries(ifindexMap, ipMap tapMetadataMapOps, ifindex, mvmIP uint32) error {
	var mappedIfindex uint32
	ipMappingFound := true
	if err := ipMap.Lookup(&mvmIP, &mappedIfindex); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("map.Lookup failed before delete: %w, name: %s", err, MapNameMVMIPToIfindex)
		}
		ipMappingFound = false
	}

	if err := ifindexMap.Delete(&ifindex); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}
	if ipMappingFound && mappedIfindex == ifindex {
		if err := ipMap.Delete(&mvmIP); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameMVMIPToIfindex)
		}
	}

	return nil
}

// GetTAPDevice returns a TAP device associated with the specific ifindex.
func GetTAPDevice(ifindex uint32) (*TAPDevice, error) {
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	var mvmMeta mvmMetadata
	err = m.Lookup(&ifindex, &mvmMeta)
	if err != nil {
		return nil, fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}

	return &TAPDevice{
		IP:      uint32ToIP(mvmMeta.IP),
		ID:      bytesToString(mvmMeta.UUID[:]),
		Ifindex: int(ifindex),
	}, nil
}
