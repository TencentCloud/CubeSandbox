package cubevs

import (
	"errors"
	"fmt"
	"net"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

const maxNetPolicyEntries = 8192

var alwaysDeniedSandboxCIDRs = []string{
	"10.0.0.0/8",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// newInnerLPMMap creates a new LPM trie map to be used as inner map
// for allow_out / deny_out hash-of-maps.
func newInnerLPMMap() (*ebpf.Map, error) {
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(lpmKey{})),
		ValueSize:  uint32(unsafe.Sizeof(uint32(0))),
		MaxEntries: maxNetPolicyEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
	})
	if err != nil {
		return nil, fmt.Errorf("ebpf.NewMap(LPMTrie) failed: %w", err)
	}
	return m, nil
}

// ensureInnerMap checks whether the outer hash-of-maps already has an
// inner map for the given ifindex.  If not, it creates one and inserts it.
func ensureInnerMap(outerMap *ebpf.Map, ifindex uint32, mapName string) error {
	// Check if inner map already exists for this ifindex.
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err == nil {
		// Already present, nothing to do.
		return nil
	}
	if !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("map.Lookup failed: %w, name: %s", err, mapName)
	}

	// Create a new inner LPM trie map and insert it.
	inner, err := newInnerLPMMap()
	if err != nil {
		return err
	}
	defer inner.Close()

	err = outerMap.Put(&ifindex, inner)
	if err != nil {
		return fmt.Errorf("map.Put failed: %w, name: %s", err, mapName)
	}
	return nil
}

// initNetPolicy creates inner LPM trie maps for the given ifindex
// in both allow_out and deny_out hash-of-maps, if not already present.
// This should be called during AttachFilter.
func initNetPolicy(ifindex uint32) error {
	allowOut, err := cachedPinnedMap(MapNameAllowOut)
	if err != nil {
		return err
	}

	err = ensureInnerMap(allowOut, ifindex, MapNameAllowOut)
	if err != nil {
		return err
	}

	denyOut, err := cachedPinnedMap(MapNameDenyOut)
	if err != nil {
		return err
	}

	return ensureInnerMap(denyOut, ifindex, MapNameDenyOut)
}

// flushInnerMap removes all entries from the inner LPM trie map
// associated with the given ifindex in the outer hash-of-maps.
func flushInnerMap(outerMap *ebpf.Map, ifindex uint32) error {
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("map.Lookup failed: %w", err)
	}

	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	var key lpmKey
	iter := inner.Iterate()
	for iter.Next(&key, new(uint32)) {
		_ = inner.Delete(&key)
	}
	return nil
}

// cleanupNetPolicy flushes all entries in the inner LPM trie maps
// for the given ifindex in both allow_out and deny_out.
// This should be called during DelTAPDevice.
func cleanupNetPolicy(ifindex uint32) error {
	allowOut, err := cachedPinnedMap(MapNameAllowOut)
	if err != nil {
		return err
	}

	err = flushInnerMap(allowOut, ifindex)
	if err != nil {
		return fmt.Errorf("flush %s failed: %w", MapNameAllowOut, err)
	}

	denyOut, err := cachedPinnedMap(MapNameDenyOut)
	if err != nil {
		return err
	}

	return flushInnerMap(denyOut, ifindex)
}

// parseCIDR parses a CIDR string (e.g. "10.0.0.0/8") or a plain IP
// (e.g. "10.1.2.3") into an lpmKey.
func parseCIDR(s string) (lpmKey, error) {
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		// Try as a plain IP address (treated as /32).
		ip := net.ParseIP(s)
		if ip == nil {
			return lpmKey{}, fmt.Errorf("invalid CIDR or IP: %s", s) //nolint:err113
		}
		return lpmKey{Prefixlen: 32, IP: ipToUint32(ip)}, nil
	}
	ones, _ := ipNet.Mask.Size()
	return lpmKey{Prefixlen: uint32(ones), IP: ipToUint32(ipNet.IP)}, nil
}

// populateInnerMap parses the given CIDR list and inserts each entry
// into the inner LPM trie map for the specified ifindex.
func populateInnerMap(outerMap *ebpf.Map, ifindex uint32, cidrs []string) error {
	var innerMapID uint32
	err := outerMap.Lookup(&ifindex, &innerMapID)
	if err != nil {
		return fmt.Errorf("map.Lookup failed: %w", err)
	}

	inner, err := ebpf.NewMapFromID(ebpf.MapID(innerMapID))
	if err != nil {
		return fmt.Errorf("ebpf.NewMapFromID failed: %w, id: %d", err, innerMapID)
	}
	defer inner.Close()

	val := uint32(1)
	for _, cidr := range cidrs {
		key, err := parseCIDR(cidr)
		if err != nil {
			return err
		}
		err = inner.Update(&key, &val, ebpf.UpdateAny)
		if err != nil {
			return fmt.Errorf("inner map update failed: %w, cidr: %s", err, cidr)
		}
	}
	return nil
}

// applyNetPolicy configures egress network policy for the given ifindex
// based on MVMOptions.
//
// Rules:
//   - AllowOut non-empty: insert entries into allow_out inner map.
//   - DenyOut always includes alwaysDeniedSandboxCIDRs.
//   - AllowInternetAccess=false: DenyOut is set to "0.0.0.0/0" (deny all).
func applyNetPolicy(ifindex uint32, opts MVMOptions) error {
	// Process allowOut.
	var allowOut []string
	if opts.AllowOut != nil {
		allowOut = *opts.AllowOut
	}
	if len(allowOut) > 0 {
		allowOutMap, err := cachedPinnedMap(MapNameAllowOut)
		if err != nil {
			return err
		}

		err = populateInnerMap(allowOutMap, ifindex, allowOut)
		if err != nil {
			return fmt.Errorf("populate %s failed: %w", MapNameAllowOut, err)
		}
	}

	// Process denyOut: always append alwaysDeniedSandboxCIDRs.
	// If AllowInternetAccess is false, deny all outbound traffic.
	var denyOut []string
	if opts.AllowInternetAccess != nil && !*opts.AllowInternetAccess {
		denyOut = []string{"0.0.0.0/0"}
	} else {
		if opts.DenyOut != nil {
			denyOut = append(*opts.DenyOut, alwaysDeniedSandboxCIDRs...)
		} else {
			denyOut = append(denyOut, alwaysDeniedSandboxCIDRs...)
		}
	}

	if len(denyOut) > 0 {
		denyOutMap, err := cachedPinnedMap(MapNameDenyOut)
		if err != nil {
			return err
		}

		err = populateInnerMap(denyOutMap, ifindex, denyOut)
		if err != nil {
			return fmt.Errorf("populate %s failed: %w", MapNameDenyOut, err)
		}
	}

	return nil
}
