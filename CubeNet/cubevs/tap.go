package cubevs

import (
	"errors"
	"fmt"
	"net"

	"github.com/cilium/ebpf"
)

type MVMOptions struct {
	AllowInternetAccess *bool
	AllowOut            *[]string // CIDR, IP, or domain
	L7AllowOut          *[]string // CIDR, IP, or domain that requires L7 policy handling
	DenyOut             *[]string // CIDR or IP
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
	if err := applyNetPolicy(ifindex, opts); err != nil {
		_ = DeleteTAPDevice(ifindex, ip)
		return err
	}
	return nil
}

// UpsertTAPDeviceMetadata registers or refreshes TAP metadata without touching
// per-sandbox policy maps. Recovery paths use this to repair metadata while
// preserving allow_out_v2, deny_out and dns_allow contents.
func UpsertTAPDeviceMetadata(ifindex uint32, ip net.IP, id string, version uint32) error {
	if len(id) > maxIDLength {
		return ErrTooLong
	}

	mvmIP := ipToUint32(ip)
	mvmID := mvmMetadata{
		IP:      mvmIP,
		UUID:    stringToByteArray(id),
		Version: version,
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
