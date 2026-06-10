package cubevs

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/cilium/ebpf"
)

const legacyAllowOutValueSize = uint32(4)

// migrateAllowOutV1ToV2 copies static v0.2.0 allow_out entries into
// allow_out_v2. The legacy value is only a presence marker, so migrated
// entries become static v2 entries with ExpiresAtNS set to zero.
func migrateAllowOutV1ToV2() error {
	legacy, err := ebpf.LoadPinnedMap(pinPath(MapNameAllowOut), nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load legacy %s failed: %w", MapNameAllowOut, err)
	}
	defer legacy.Close()

	if err := verifyMapLayout(legacy, MapNameAllowOut, ebpf.HashOfMaps, uint32(unsafe.Sizeof(uint32(0))), uint32(unsafe.Sizeof(uint32(0)))); err != nil {
		return err
	}

	current, err := loadPinnedMap(MapNameAllowOutV2)
	if err != nil {
		return err
	}
	defer current.Close()

	var ifindex uint32
	var innerMapID uint32
	iter := legacy.Iterate()
	for iter.Next(&ifindex, &innerMapID) {
		if err := migrateAllowOutInnerMap(current, ifindex, ebpf.MapID(innerMapID)); err != nil {
			return err
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("iterate legacy %s failed: %w", MapNameAllowOut, err)
	}
	return nil
}

func migrateAllowOutInnerMap(current *ebpf.Map, ifindex uint32, legacyInnerID ebpf.MapID) error {
	legacyInner, err := ebpf.NewMapFromID(legacyInnerID)
	if err != nil {
		return fmt.Errorf("open legacy %s inner map failed: %w, id: %d", MapNameAllowOut, err, legacyInnerID)
	}
	defer legacyInner.Close()

	if err := verifyMapLayout(legacyInner, MapNameAllowOut, ebpf.LPMTrie, uint32(unsafe.Sizeof(lpmKey{})), legacyAllowOutValueSize); err != nil {
		return err
	}

	if err := ensureAllowOutV2InnerMap(current, ifindex); err != nil {
		return err
	}
	inner, err := lookupInnerMap(current, ifindex)
	if err != nil {
		return err
	}
	defer inner.Close()

	var key lpmKey
	var oldValue uint32
	iter := legacyInner.Iterate()
	for iter.Next(&key, &oldValue) {
		value := netPolicyValueV2{}
		var existing netPolicyValueV2
		if err := inner.Lookup(&key, &existing); err == nil {
			value.Flags |= existing.Flags
		} else if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("lookup %s inner map failed: %w", MapNameAllowOutV2, err)
		}
		if err := inner.Update(&key, &value, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update %s inner map failed: %w", MapNameAllowOutV2, err)
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("iterate legacy %s inner map failed: %w", MapNameAllowOut, err)
	}
	return nil
}

func verifyMapLayout(m *ebpf.Map, name string, wantType ebpf.MapType, wantKeySize, wantValueSize uint32) error {
	info, err := m.Info()
	if err != nil {
		return fmt.Errorf("get %s map info failed: %w", name, err)
	}
	if info.Type != wantType || info.KeySize != wantKeySize || info.ValueSize != wantValueSize {
		return fmt.Errorf("%s map has incompatible ABI: type=%s key_size=%d value_size=%d, want type=%s key_size=%d value_size=%d", name, info.Type, info.KeySize, info.ValueSize, wantType, wantKeySize, wantValueSize) //nolint:err113
	}
	return nil
}
