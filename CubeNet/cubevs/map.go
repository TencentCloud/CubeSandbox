package cubevs

import (
	"fmt"
	"path/filepath"

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
