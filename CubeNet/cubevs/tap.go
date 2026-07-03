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
	if err := UpsertTAPDeviceMeta(ifindex, ip, id, version); err != nil {
		return err
	}
	if err := applyNetPolicy(ifindex, opts); err != nil {
		_ = DelTAPDevice(ifindex, ip)
		return err
	}
	return nil
}

// UpsertTAPDeviceMeta registers or refreshes TAP metadata without touching
// per-sandbox policy maps. Recovery paths use this to repair metadata while
// preserving allow_out_v2, deny_out and dns_allow contents.
func UpsertTAPDeviceMeta(ifindex uint32, ip net.IP, id string, version uint32) error {
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
	if err := UpsertTAPDeviceMeta(ifindex, ip, id, version); err != nil {
		return err
	}
	return replaceNetPolicy(ifindex, opts)
}

// DelTAPDevice removes a TAP device from CubeVS.
func DelTAPDevice(ifindex uint32, ip net.IP) error {
	// Clean up policy inner map entries first.
	err := cleanupNetPolicy(ifindex)
	if err != nil {
		return err
	}
	if err := cleanupDNSAllow(ifindex); err != nil {
		return err
	}

	mvmIP := ipToUint32(ip)

	// ifindex <-> MVM metadata
	m, err := loadPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}
	defer m.Close()

	err = m.Delete(&ifindex)
	if err != nil {
		return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}

	// MVM IP <-> ifindex
	m, err = loadPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		return err
	}
	defer m.Close()

	err = m.Delete(&mvmIP)
	if err != nil {
		return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameMVMIPToIfindex)
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
