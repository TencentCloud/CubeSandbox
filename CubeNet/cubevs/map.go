package cubevs

import (
	"fmt"
	"path/filepath"

	"github.com/cilium/ebpf"
)

// bpfFSPath is the bpffs mount point used for pinning maps. It is a var (not a
// const) so tests can point it at a temporary bpffs mount instead of the real
// /sys/fs/bpf.
var bpfFSPath = "/sys/fs/bpf"

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
