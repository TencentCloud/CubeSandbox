package cubevs

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/cilium/ebpf"
)

func pinPath(name string) string {
	path := filepath.Join(bpfFSPath, name)

	return filepath.Clean(path)
}

func loadPinnedMap(name string) (*ebpf.Map, error) {
	path := pinPath(name)
	m, err := ebpf.LoadPinnedMap(path, nil)
	if err != nil {
		return nil, fmt.Errorf("ebpf.LoadPinnedMap failed: %w, name: %s", err, name)
	}

	return m, nil
}

var (
	pinnedMapCacheMu sync.Mutex
	pinnedMapCache   = make(map[string]*ebpf.Map)
)

// cachedPinnedMap returns a process-lifetime cached handle to the named pinned
// map, opening it (a bpffs LoadPinnedMap syscall) only on first use. The named
// outer maps are created once at Init and live for the whole process, so there
// is no need to re-open and Close them on every operation — that open/close pair
// was a measurable per-request syscall cost on the create/delete hot path.
//
// The returned *ebpf.Map is SHARED and MUST NOT be closed by callers. ebpf.Map
// methods (Lookup/Update/Delete/Put/Iterate) are safe for concurrent use, so the
// cache only needs to guard the lazy open.
func cachedPinnedMap(name string) (*ebpf.Map, error) {
	pinnedMapCacheMu.Lock()
	defer pinnedMapCacheMu.Unlock()

	if m, ok := pinnedMapCache[name]; ok {
		return m, nil
	}
	m, err := loadPinnedMap(name)
	if err != nil {
		return nil, err
	}
	pinnedMapCache[name] = m
	return m, nil
}
