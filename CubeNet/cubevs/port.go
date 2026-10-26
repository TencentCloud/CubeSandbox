package cubevs

import (
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
)

type portMapOps interface {
	Lookup(key, valueOut interface{}) error
	Update(key, value any, flags ebpf.MapUpdateFlags) error
	Delete(key interface{}) error
}

// AddPortMapping adds port mapping for host port and guest port.
func AddPortMapping(ifindex uint32, listenPort uint16, hostPort uint16) error {
	// host port => ifindex + listen port
	m1, err := loadPinnedMap(MapNameRemotePortMapping)
	if err != nil {
		return err
	}
	defer m1.Close()

	// ifindex + listen port => host port
	m2, err := loadPinnedMap(MapNameLocalPortMapping)
	if err != nil {
		return err
	}
	defer m2.Close()

	return addPortMapping(m1, m2, ifindex, listenPort, hostPort)
}

func addPortMapping(remoteMap, localMap portMapOps, ifindex uint32, listenPort, hostPort uint16) error {
	networkHostPort := htons(hostPort)
	mvmPort := MVMPort{
		Ifindex:    ifindex,
		ListenPort: htons(listenPort),
	}

	var remoteMapping MVMPort
	remoteFound := true
	if err := remoteMap.Lookup(&networkHostPort, &remoteMapping); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("map.Lookup failed before update: %w, name: %s", err, MapNameRemotePortMapping)
		}
		remoteFound = false
	} else if !sameMVMPort(remoteMapping, mvmPort) {
		return fmt.Errorf(
			"host port %d already maps to ifindex/listen_port %d/%d, cannot map to %d/%d",
			hostPort,
			remoteMapping.Ifindex,
			ntohs(remoteMapping.ListenPort),
			ifindex,
			listenPort,
		)
	}

	var localHostPort uint16
	localFound := true
	if err := localMap.Lookup(&mvmPort, &localHostPort); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("map.Lookup failed before update: %w, name: %s", err, MapNameLocalPortMapping)
		}
		localFound = false
	} else if localHostPort != networkHostPort {
		return fmt.Errorf(
			"ifindex/listen_port %d/%d already maps to host port %d, cannot map to %d",
			ifindex,
			listenPort,
			ntohs(localHostPort),
			hostPort,
		)
	}

	if remoteFound && localFound {
		return nil
	}

	remoteInserted := false
	if !remoteFound {
		if err := remoteMap.Update(&networkHostPort, &mvmPort, ebpf.UpdateNoExist); err != nil {
			if !errors.Is(err, ebpf.ErrKeyExist) {
				return fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameRemotePortMapping)
			}
			var current MVMPort
			if lookupErr := remoteMap.Lookup(&networkHostPort, &current); lookupErr != nil {
				return errors.Join(
					fmt.Errorf("map.Update raced with another writer: %w, name: %s", err, MapNameRemotePortMapping),
					fmt.Errorf("map.Lookup failed after update race: %w, name: %s", lookupErr, MapNameRemotePortMapping),
				)
			}
			if !sameMVMPort(current, mvmPort) {
				return fmt.Errorf(
					"host port %d was concurrently mapped to ifindex/listen_port %d/%d, cannot map to %d/%d",
					hostPort,
					current.Ifindex,
					ntohs(current.ListenPort),
					ifindex,
					listenPort,
				)
			}
		} else {
			remoteInserted = true
		}
	}

	if localFound {
		return nil
	}
	if err := localMap.Update(&mvmPort, &networkHostPort, ebpf.UpdateNoExist); err != nil {
		updateErr := fmt.Errorf("map.Update failed: %w, name: %s", err, MapNameLocalPortMapping)
		if errors.Is(err, ebpf.ErrKeyExist) {
			var current uint16
			if lookupErr := localMap.Lookup(&mvmPort, &current); lookupErr != nil {
				updateErr = errors.Join(
					fmt.Errorf("map.Update raced with another writer: %w, name: %s", err, MapNameLocalPortMapping),
					fmt.Errorf("map.Lookup failed after update race: %w, name: %s", lookupErr, MapNameLocalPortMapping),
				)
			} else if current == networkHostPort {
				return nil
			} else {
				updateErr = fmt.Errorf(
					"ifindex/listen_port %d/%d was concurrently mapped to host port %d, cannot map to %d",
					ifindex,
					listenPort,
					ntohs(current),
					hostPort,
				)
			}
		}
		if remoteInserted {
			if rollbackErr := rollbackRemotePortMapping(remoteMap, networkHostPort, mvmPort); rollbackErr != nil {
				return errors.Join(updateErr, rollbackErr)
			}
		}
		return updateErr
	}

	return nil
}

func rollbackRemotePortMapping(remoteMap portMapOps, hostPort uint16, installed MVMPort) error {
	var current MVMPort
	if err := remoteMap.Lookup(&hostPort, &current); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("map.Lookup failed during rollback: %w, name: %s", err, MapNameRemotePortMapping)
	}
	if !sameMVMPort(current, installed) {
		return nil
	}
	if err := remoteMap.Delete(&hostPort); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("map.Delete failed during rollback: %w, name: %s", err, MapNameRemotePortMapping)
	}
	return nil
}

// DelPortMapping deletes existing port mapping from CubeVS.
// This will cause network interruption and should be called only when the MVM exits.
// Entries which no longer match the expected mapping are left untouched.
func DelPortMapping(ifindex uint32, listenPort uint16, hostPort uint16) error {
	m1, err := loadPinnedMap(MapNameLocalPortMapping)
	if err != nil {
		return err
	}
	defer m1.Close()

	m2, err := loadPinnedMap(MapNameRemotePortMapping)
	if err != nil {
		return err
	}
	defer m2.Close()

	return delPortMapping(m1, m2, ifindex, listenPort, hostPort)
}

func delPortMapping(localMap, remoteMap portMapOps, ifindex uint32, listenPort, hostPort uint16) error {
	mvmPort := MVMPort{
		Ifindex:    ifindex,
		ListenPort: htons(listenPort),
	}
	networkHostPort := htons(hostPort)

	var localHostPort uint16
	localFound := true
	if err := localMap.Lookup(&mvmPort, &localHostPort); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("map.Lookup failed before delete: %w, name: %s", err, MapNameLocalPortMapping)
		}
		localFound = false
	}

	var remoteMVMPort MVMPort
	remoteFound := true
	if err := remoteMap.Lookup(&networkHostPort, &remoteMVMPort); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("map.Lookup failed before delete: %w, name: %s", err, MapNameRemotePortMapping)
		}
		remoteFound = false
	}

	var deleteErr error
	if localFound && localHostPort == networkHostPort {
		if err := localMap.Delete(&mvmPort); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameLocalPortMapping))
		}
	}
	if remoteFound && sameMVMPort(remoteMVMPort, mvmPort) {
		if err := remoteMap.Delete(&networkHostPort); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("map.Delete failed: %w, name: %s", err, MapNameRemotePortMapping))
		}
	}

	return deleteErr
}

type portMappingTuple struct {
	Ifindex    uint32
	ListenPort uint16
	HostPort   uint16
}

// DeletePortMappingsByIfindex removes every local or remote port-map entry
// which still belongs to ifindex. It scans the two maps independently so a
// one-sided stale entry or an unrelated conflicting tuple cannot block cleanup.
func DeletePortMappingsByIfindex(ifindex uint32) error {
	remoteMap, err := loadPinnedMap(MapNameRemotePortMapping)
	if err != nil {
		return err
	}
	defer remoteMap.Close()

	localMap, err := loadPinnedMap(MapNameLocalPortMapping)
	if err != nil {
		return err
	}
	defer localMap.Close()

	tuples := make(map[portMappingTuple]struct{})
	var (
		hostPort uint16
		mvmPort  MVMPort
	)
	iter := remoteMap.Iterate()
	for iter.Next(&hostPort, &mvmPort) {
		addPortMappingTupleForIfindex(tuples, ifindex, mvmPort, hostPort)
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("map.Iterate failed: %w, name: %s", err, MapNameRemotePortMapping)
	}

	iter = localMap.Iterate()
	for iter.Next(&mvmPort, &hostPort) {
		addPortMappingTupleForIfindex(tuples, ifindex, mvmPort, hostPort)
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("map.Iterate failed: %w, name: %s", err, MapNameLocalPortMapping)
	}

	return deletePortMappingTuples(localMap, remoteMap, tuples)
}

func addPortMappingTupleForIfindex(tuples map[portMappingTuple]struct{}, ifindex uint32, mvmPort MVMPort, networkHostPort uint16) {
	if mvmPort.Ifindex != ifindex {
		return
	}
	tuples[portMappingTuple{
		Ifindex:    ifindex,
		ListenPort: ntohs(mvmPort.ListenPort),
		HostPort:   ntohs(networkHostPort),
	}] = struct{}{}
}

func deletePortMappingTuples(localMap, remoteMap portMapOps, tuples map[portMappingTuple]struct{}) error {
	var cleanupErr error
	for tuple := range tuples {
		if err := delPortMapping(localMap, remoteMap, tuple.Ifindex, tuple.ListenPort, tuple.HostPort); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

// ListPortMapping returns host port to guest port (with ifindex) mapping.
func ListPortMapping() (map[uint16]MVMPort, error) {
	m1, err := loadPinnedMap(MapNameRemotePortMapping)
	if err != nil {
		return nil, err
	}
	defer m1.Close()

	var (
		key    uint16
		value  MVMPort
		result = make(map[uint16]MVMPort)
	)

	iter := m1.Iterate()
	for iter.Next(&key, &value) {
		if err := addListedPortMapping(result, ntohs(key), MVMPort{
			Ifindex:    value.Ifindex,
			ListenPort: ntohs(value.ListenPort),
		}); err != nil {
			return nil, err
		}
	}
	err = iter.Err()
	if err != nil {
		return nil, fmt.Errorf("map.Iterate failed: %w, name: %s", err, MapNameRemotePortMapping)
	}

	m2, err := loadPinnedMap(MapNameLocalPortMapping)
	if err != nil {
		return nil, err
	}
	defer m2.Close()

	iter = m2.Iterate()
	for iter.Next(&value, &key) {
		if err := addListedPortMapping(result, ntohs(key), MVMPort{
			Ifindex:    value.Ifindex,
			ListenPort: ntohs(value.ListenPort),
		}); err != nil {
			return nil, err
		}
	}
	err = iter.Err()
	if err != nil {
		return nil, fmt.Errorf("map.Iterate failed: %w, name: %s", err, MapNameLocalPortMapping)
	}

	return result, nil
}

func addListedPortMapping(result map[uint16]MVMPort, hostPort uint16, mapping MVMPort) error {
	mapping.Reserved = 0
	if existing, ok := result[hostPort]; ok && !sameMVMPort(existing, mapping) {
		return fmt.Errorf(
			"conflicting port mapping for host port %d: ifindex/listen_port %d/%d and %d/%d",
			hostPort,
			existing.Ifindex,
			existing.ListenPort,
			mapping.Ifindex,
			mapping.ListenPort,
		)
	}
	result[hostPort] = mapping
	return nil
}

func sameMVMPort(left, right MVMPort) bool {
	return left.Ifindex == right.Ifindex && left.ListenPort == right.ListenPort
}

// GetPortMapping returns the host port assigned for the specified ifindex and listen port.
func GetPortMapping(ifindex uint32, listenPort uint16) (uint16, error) {
	m, err := loadPinnedMap(MapNameLocalPortMapping)
	if err != nil {
		return 0, err
	}
	defer m.Close()

	var hostPort uint16
	err = m.Lookup(&MVMPort{
		Ifindex:    ifindex,
		ListenPort: htons(listenPort),
	}, &hostPort)
	if err != nil {
		return 0, fmt.Errorf("map.Lookup failed: %w, name: %s", err, MapNameLocalPortMapping)
	}

	return ntohs(hostPort), nil
}
