package runtime

import (
	"fmt"
	"net"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
)

type fakeCubeVSAdapter struct {
	addTAPDeviceCalls      []uint32
	upsertTAPDeviceCalls   []uint32
	addPortMappings        []PortMapping
	deletePortMappings     []PortMapping
	cleanupPolicyCalls     []uint32
	defaultDenyPolicyCalls []uint32
	listPortMappings       map[uint16]cubevs.MVMPort
	getTAPDeviceByIndex    map[uint32]*cubevs.TAPDevice
	deletedTAPDevices      map[uint32]struct{}
	cleanupPolicyErr       error
	deleteMetadataErr      error
	deletePortMappingErr   error
	updatedPolicies        []updatedPolicy
	updateTAPPolicyErr     error
	recorder               *createOrderRecorder
}

// updatedPolicy records one UpdateTAPPolicy call so tests can assert both that
// the policy update reached CubeVS and what it carried.
type updatedPolicy struct {
	ifindex uint32
	opts    cubevs.MVMOptions
}

func (f *fakeCubeVSAdapter) AddTAPDevice(_ uint32, _ net.IP, _ string, _ uint32, _ cubevs.MVMOptions) error {
	if f.recorder != nil {
		f.recorder.record("cubevs_tap")
	}
	return nil
}

func (f *fakeCubeVSAdapter) UpsertTAPDevice(ifindex uint32, _ net.IP, _ string, _ uint32, _ cubevs.MVMOptions) error {
	f.upsertTAPDeviceCalls = append(f.upsertTAPDeviceCalls, ifindex)
	return nil
}

func (f *fakeCubeVSAdapter) UpsertTAPDeviceMetadata(_ uint32, _ net.IP, _ string, _ uint32) error {
	return nil
}

func (f *fakeCubeVSAdapter) UpdateTAPPolicy(ifindex uint32, opts cubevs.MVMOptions) error {
	f.updatedPolicies = append(f.updatedPolicies, updatedPolicy{ifindex: ifindex, opts: opts})
	return f.updateTAPPolicyErr
}

func (f *fakeCubeVSAdapter) GetTAPDevice(ifindex uint32) (*cubevs.TAPDevice, error) {
	if _, deleted := f.deletedTAPDevices[ifindex]; deleted {
		return nil, ebpf.ErrKeyNotExist
	}
	if f.getTAPDeviceByIndex == nil {
		return nil, ebpf.ErrKeyNotExist
	}
	if dev := f.getTAPDeviceByIndex[ifindex]; dev != nil {
		return dev, nil
	}
	return nil, ebpf.ErrKeyNotExist
}

func (f *fakeCubeVSAdapter) CleanupTAPPolicy(ifindex uint32) error {
	if f.recorder != nil {
		f.recorder.record("cubevs_policy_cleaned")
	}
	f.cleanupPolicyCalls = append(f.cleanupPolicyCalls, ifindex)
	return f.cleanupPolicyErr
}

func (f *fakeCubeVSAdapter) DeleteTAPDeviceMetadata(ifindex uint32, _ net.IP) error {
	if f.recorder != nil {
		f.recorder.record("cubevs_tap_metadata_deleted")
	}
	if f.deleteMetadataErr != nil {
		return f.deleteMetadataErr
	}
	if f.deletedTAPDevices == nil {
		f.deletedTAPDevices = make(map[uint32]struct{})
	}
	f.deletedTAPDevices[ifindex] = struct{}{}
	return nil
}

func (f *fakeCubeVSAdapter) AttachFilter(_ uint32) error {
	return nil
}

func (f *fakeCubeVSAdapter) InstallTAPDefaultDenyPolicy(ifindex uint32) error {
	if f.recorder != nil {
		f.recorder.record("cubevs_default_deny")
	}
	f.defaultDenyPolicyCalls = append(f.defaultDenyPolicyCalls, ifindex)
	return nil
}

func (f *fakeCubeVSAdapter) GCStaleNetPolicyMaps(_ map[uint32]struct{}, _ func(uint32) bool, _ func(uint32)) (int, error) {
	return 0, nil
}

func (f *fakeCubeVSAdapter) AddPortMapping(ifindex uint32, containerPort, hostPort uint16) error {
	if f.recorder != nil {
		f.recorder.record("cubevs_port_mapping")
	}
	f.addPortMappings = append(f.addPortMappings, PortMapping{
		HostPort:      int32(hostPort),
		ContainerPort: int32(containerPort),
	})
	if f.listPortMappings == nil {
		f.listPortMappings = make(map[uint16]cubevs.MVMPort)
	}
	if existing, ok := f.listPortMappings[hostPort]; ok &&
		(existing.Ifindex != ifindex || existing.ListenPort != containerPort) {
		return fmt.Errorf("host port %d belongs to another tuple", hostPort)
	}
	f.listPortMappings[hostPort] = cubevs.MVMPort{Ifindex: ifindex, ListenPort: containerPort}
	return nil
}

func (f *fakeCubeVSAdapter) DelPortMapping(ifindex uint32, containerPort, hostPort uint16) error {
	if f.recorder != nil {
		f.recorder.record("cubevs_port_mapping_deleted")
	}
	if f.deletePortMappingErr != nil {
		return f.deletePortMappingErr
	}
	if f.listPortMappings != nil {
		current, ok := f.listPortMappings[hostPort]
		if !ok || current.Ifindex != ifindex || current.ListenPort != containerPort {
			return nil
		}
		f.deletePortMappings = append(f.deletePortMappings, PortMapping{
			HostPort:      int32(hostPort),
			ContainerPort: int32(containerPort),
		})
		delete(f.listPortMappings, hostPort)
		return nil
	}
	f.deletePortMappings = append(f.deletePortMappings, PortMapping{
		HostPort:      int32(hostPort),
		ContainerPort: int32(containerPort),
	})
	return nil
}

func (f *fakeCubeVSAdapter) DeletePortMappingsByIfindex(ifindex uint32) error {
	if f.deletePortMappingErr != nil {
		return f.deletePortMappingErr
	}
	for hostPort, mapping := range f.listPortMappings {
		if mapping.Ifindex != ifindex {
			continue
		}
		f.deletePortMappings = append(f.deletePortMappings, PortMapping{
			HostPort:      int32(hostPort),
			ContainerPort: int32(mapping.ListenPort),
		})
		delete(f.listPortMappings, hostPort)
	}
	return nil
}

func TestApplyAndClearPortMappingsUseCubeVSAdapter(t *testing.T) {
	ports, err := newPortBinder()
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeCubeVSAdapter{}
	controller := &NetworkController{ports: ports, cubevsAdapter: adapter}
	tap := &tapDevice{
		Index:        7,
		Name:         "z10.0.0.2",
		PortMappings: []PortMapping{{HostPort: 20001, ContainerPort: 8080}},
	}

	if _, err := controller.reservePortMappings("sandbox1", tap, tap.PortMappings); err != nil {
		t.Fatal(err)
	}
	if err := controller.applyPortMappings("sandbox1", tap); err != nil {
		t.Fatal(err)
	}
	if len(adapter.addPortMappings) != 1 || adapter.addPortMappings[0].HostPort != 20001 {
		t.Fatalf("addPortMappings = %#v", adapter.addPortMappings)
	}

	if err := controller.cleanupPortMappings("sandbox1", tap); err != nil {
		t.Fatal(err)
	}
	if len(adapter.deletePortMappings) != 1 || adapter.deletePortMappings[0].HostPort != 20001 {
		t.Fatalf("deletePortMappings = %#v", adapter.deletePortMappings)
	}
}
