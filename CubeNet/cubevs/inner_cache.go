package cubevs

import (
	"errors"
	"fmt"
	"sync"

	"github.com/cilium/ebpf"
)

// policyInnerCache keeps open FDs for HashOfMaps inners keyed by outer map name
// and TAP ifindex. Userspace BPF_MAP_LOOKUP_ELEM on HashOfMaps is extremely
// expensive on large hosts; create/apply/reaper paths must reuse cached inners
// instead of looking up the outer map on every call.
//
// FD budget: each live TAP may pin up to one FD per net-policy outer
// (allow_out_v2, deny_out, dns_allow) — about 3 × active-TAP-count on Cubelet,
// plus any Active ifindexes warmed by the DNS reaper. Entries are released only
// on TAP destroy / startup stale-outer GC; there is no size-bounded eviction.
// There is also no BPF-reload generation counter today (no reload path); if
// pinned outers are ever recreated, call clearPolicyInnerCacheForTest-style
// invalidation (or add a generation) before reuse.
type policyInnerKey struct {
	mapName string
	ifindex uint32
}

var policyInnerMaps sync.Map // policyInnerKey -> *ebpf.Map

// acquireInnerMap returns the inner map for ifindex, creating it when missing
// and newInner is non-nil. The returned map is owned by the process-wide cache;
// callers must not Close it. A nil newInner means "must already exist".
//
// Cubelet completes stale-outer GC synchronously before starting background TAP
// work or serving requests, and its TAP lifecycle prevents concurrent ownership
// of one ifindex. This cache therefore does not add another per-key lock.
func acquireInnerMap(outerMap *ebpf.Map, ifindex uint32, mapName string,
	newInner func() (*ebpf.Map, error),
) (*ebpf.Map, error) {
	key := policyInnerKey{mapName: mapName, ifindex: ifindex}
	if cached, ok := policyInnerMaps.Load(key); ok {
		return cached.(*ebpf.Map), nil
	}

	var inner *ebpf.Map
	err := outerMap.Lookup(&ifindex, &inner)
	if err == nil {
		actual, loaded := policyInnerMaps.LoadOrStore(key, inner)
		if loaded {
			_ = inner.Close()
			return actual.(*ebpf.Map), nil
		}
		return inner, nil
	}
	if !errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil, fmt.Errorf("map.Lookup failed: %w, name: %s", err, mapName)
	}
	if newInner == nil {
		return nil, fmt.Errorf("map.Lookup failed: %w, name: %s", ebpf.ErrKeyNotExist, mapName)
	}

	created, err := newInner()
	if err != nil {
		return nil, err
	}
	if err := outerMap.Put(&ifindex, created); err != nil {
		_ = created.Close()
		return nil, fmt.Errorf("map.Put failed: %w, name: %s", err, mapName)
	}
	actual, loaded := policyInnerMaps.LoadOrStore(key, created)
	if loaded {
		_ = created.Close()
		return actual.(*ebpf.Map), nil
	}
	return created, nil
}

// releaseCachedInner closes and drops a cached inner FD.
func releaseCachedInner(mapName string, ifindex uint32) {
	key := policyInnerKey{mapName: mapName, ifindex: ifindex}
	if cached, ok := policyInnerMaps.LoadAndDelete(key); ok {
		_ = cached.(*ebpf.Map).Close()
	}
}

// deleteCachedInnerAndOuter removes the outer key and its cached userspace FD.
func deleteCachedInnerAndOuter(outer *ebpf.Map, mapName string, ifindex uint32) error {
	key := policyInnerKey{mapName: mapName, ifindex: ifindex}
	var toClose *ebpf.Map
	if cached, ok := policyInnerMaps.LoadAndDelete(key); ok {
		toClose = cached.(*ebpf.Map)
	}
	err := outer.Delete(&ifindex)
	if toClose != nil {
		_ = toClose.Close()
	}
	if err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}

func clearPolicyInnerCacheForTest() {
	policyInnerMaps.Range(func(k, v any) bool {
		policyInnerMaps.Delete(k)
		if m, ok := v.(*ebpf.Map); ok && m != nil {
			_ = m.Close()
		}
		return true
	})
}
