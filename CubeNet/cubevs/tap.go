package cubevs

import (
	"fmt"
	"net"

	"github.com/cilium/ebpf"
)

type MVMOptions struct {
	AllowInternetAccess *bool
	AllowOut            *[]string // CIDR or IP
	DenyOut             *[]string // CIDR or IP
}

// ListTAPDevices lists all TAP devices that managed by CubeVS.
func ListTAPDevices() ([]TAPDevice, error) {
	m, err := cachedPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return nil, err
	}

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
	m, err := cachedPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}

	err = m.Update(&ifindex, &mvmID, ebpf.UpdateAny)
	if err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}

	// MVM IP <-> ifindex
	m, err = cachedPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		return err
	}

	err = m.Update(&mvmIP, &ifindex, ebpf.UpdateAny)
	if err != nil {
		return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameMVMIPToIfindex)
	}

	return applyNetPolicy(ifindex, opts)
}

// DelTAPDevice removes a TAP device from CubeVS.
func DelTAPDevice(ifindex uint32, ip net.IP) error {
	// Clean up network policy inner map entries first.
	err := cleanupNetPolicy(ifindex)
	if err != nil {
		return err
	}

	mvmIP := ipToUint32(ip)

	// ifindex <-> MVM metadata
	m, err := cachedPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return err
	}

	err = m.Delete(&ifindex)
	if err != nil {
		return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameIfindexToMVMMetadata)
	}

	// MVM IP <-> ifindex
	m, err = cachedPinnedMap(MapNameMVMIPToIfindex)
	if err != nil {
		return err
	}

	err = m.Delete(&mvmIP)
	if err != nil {
		return fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameMVMIPToIfindex)
	}

	return nil
}

// GetTAPDevice returns a TAP device associated with the specific ifindex.
func GetTAPDevice(ifindex uint32) (*TAPDevice, error) {
	m, err := cachedPinnedMap(MapNameIfindexToMVMMetadata)
	if err != nil {
		return nil, err
	}

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
