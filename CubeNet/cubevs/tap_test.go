package cubevs

import (
	"errors"
	"testing"

	"github.com/cilium/ebpf"
)

type fakeTapMetadataMap struct {
	entries   map[uint32]uint32
	lookupErr error
	deleteErr error
}

func (m *fakeTapMetadataMap) Lookup(key, valueOut interface{}) error {
	if m.lookupErr != nil {
		return m.lookupErr
	}
	value, ok := m.entries[*key.(*uint32)]
	if !ok {
		return ebpf.ErrKeyNotExist
	}
	*valueOut.(*uint32) = value
	return nil
}

func (m *fakeTapMetadataMap) Delete(key interface{}) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	typedKey := *key.(*uint32)
	if _, ok := m.entries[typedKey]; !ok {
		return ebpf.ErrKeyNotExist
	}
	delete(m.entries, typedKey)
	return nil
}

func TestDeleteTAPDeviceMetadataEntriesDeletesMatchingReverseEntry(t *testing.T) {
	const (
		ifindex = uint32(12)
		mvmIP   = uint32(0x0a000002)
	)
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{ifindex: 0}}
	ipMap := &fakeTapMetadataMap{entries: map[uint32]uint32{mvmIP: ifindex}}

	if err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, ifindex, mvmIP); err != nil {
		t.Fatal(err)
	}
	if len(ifindexMap.entries) != 0 || len(ipMap.entries) != 0 {
		t.Fatalf("matching metadata was not fully deleted: ifindex=%#v ip=%#v", ifindexMap.entries, ipMap.entries)
	}
}

func TestDeleteTAPDeviceMetadataEntriesPreservesReusedIP(t *testing.T) {
	const (
		oldIfindex = uint32(12)
		newIfindex = uint32(13)
		mvmIP      = uint32(0x0a000002)
	)
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{oldIfindex: 0}}
	ipMap := &fakeTapMetadataMap{entries: map[uint32]uint32{mvmIP: newIfindex}}

	if err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, oldIfindex, mvmIP); err != nil {
		t.Fatal(err)
	}
	if _, ok := ifindexMap.entries[oldIfindex]; ok {
		t.Fatal("old ifindex metadata was not deleted")
	}
	if got := ipMap.entries[mvmIP]; got != newIfindex {
		t.Fatalf("reused IP mapping changed to ifindex %d, want %d", got, newIfindex)
	}
}

func TestDeleteTAPDeviceMetadataEntriesTreatsMissingReverseEntryAsClean(t *testing.T) {
	const (
		ifindex = uint32(12)
		mvmIP   = uint32(0x0a000002)
	)
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{ifindex: 0}}
	ipMap := &fakeTapMetadataMap{entries: make(map[uint32]uint32)}

	if err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, ifindex, mvmIP); err != nil {
		t.Fatal(err)
	}
	if _, ok := ifindexMap.entries[ifindex]; ok {
		t.Fatal("ifindex metadata was not deleted")
	}
}

func TestDeleteTAPDeviceMetadataEntriesLookupErrorDoesNotPartiallyDelete(t *testing.T) {
	const (
		ifindex = uint32(12)
		mvmIP   = uint32(0x0a000002)
	)
	lookupErr := errors.New("lookup failed")
	ifindexMap := &fakeTapMetadataMap{entries: map[uint32]uint32{ifindex: 0}}
	ipMap := &fakeTapMetadataMap{
		entries:   make(map[uint32]uint32),
		lookupErr: lookupErr,
	}

	err := deleteTAPDeviceMetadataEntries(ifindexMap, ipMap, ifindex, mvmIP)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want %v", err, lookupErr)
	}
	if _, ok := ifindexMap.entries[ifindex]; !ok {
		t.Fatal("ifindex metadata was deleted after reverse lookup failed")
	}
}

// fakeMvmMetadataMap is an in-memory mvmVersionMapOps for bumpMvmVersion tests.
type fakeMvmMetadataMap struct {
	entries map[uint32]mvmMetadata
}

func (m *fakeMvmMetadataMap) Lookup(key, valueOut interface{}) error {
	v, ok := m.entries[*key.(*uint32)]
	if !ok {
		return ebpf.ErrKeyNotExist
	}
	*valueOut.(*mvmMetadata) = v
	return nil
}

func (m *fakeMvmMetadataMap) Update(key, value any, _ ebpf.MapUpdateFlags) error {
	m.entries[*key.(*uint32)] = *value.(*mvmMetadata)
	return nil
}

func TestBumpMvmVersionIncrementsAndPreservesFields(t *testing.T) {
	const ifindex = uint32(42)
	orig := mvmMetadata{
		Version:        5,
		IP:             0x0a000002,
		DNSPolicyFlags: 3,
	}
	orig.UUID[0] = 'a'
	orig.Reserved[0] = 0x7f
	m := &fakeMvmMetadataMap{entries: map[uint32]mvmMetadata{ifindex: orig}}

	if err := bumpMvmVersion(m, ifindex); err != nil {
		t.Fatal(err)
	}
	got := m.entries[ifindex]
	if got.Version != 6 {
		t.Fatalf("version = %d, want 6 (map RMW +1, immune to a reset counter)", got.Version)
	}
	if got.IP != orig.IP || got.DNSPolicyFlags != orig.DNSPolicyFlags ||
		got.UUID != orig.UUID || got.Reserved != orig.Reserved {
		t.Fatal("bump must preserve IP/UUID/DNSPolicyFlags/Reserved")
	}
}

func TestBumpMvmVersionMissingEntry(t *testing.T) {
	m := &fakeMvmMetadataMap{entries: map[uint32]mvmMetadata{}}
	if err := bumpMvmVersion(m, 99); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("err = %v, want ErrKeyNotExist", err)
	}
}
