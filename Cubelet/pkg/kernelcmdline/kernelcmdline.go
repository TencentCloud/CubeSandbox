// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package kernelcmdline merges guest kernel command line params carried in the
// cube.vm.kernel.cmdline.append annotation.
package kernelcmdline

import (
	"fmt"
	"strconv"
	"strings"

	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

// MergeAnnotation stores k into dst, unioning the JSON array for the kernel
// cmdline annotation so independent producers do not overwrite each other.
func MergeAnnotation(dst map[string]string, k, v string) {
	if k != constants.AnnotationVMKernelCmdlineAppend {
		dst[k] = v
		return
	}
	dst[k] = UnionJSON(dst[k], v)
}

// UnionJSON returns the union of two JSON string arrays, keyed by the text
// before the first '='. Entries from b win on duplicate keys, and a key that
// appears more than once in either input keeps only its last value.
func UnionJSON(a, b string) string {
	var paramsA, paramsB []string
	// Inputs are already validated by FilterVMKernelCmdline; an empty or
	// malformed side degrades to "no params" instead of failing the merge.
	_ = jsoniter.Unmarshal([]byte(a), &paramsA)
	_ = jsoniter.Unmarshal([]byte(b), &paramsB)

	paramKey := func(p string) string {
		if idx := strings.IndexByte(p, '='); idx >= 0 {
			return p[:idx]
		}
		return p
	}

	merged := make([]string, 0, len(paramsA)+len(paramsB))
	index := make(map[string]int, len(paramsA)+len(paramsB))
	for _, params := range [][]string{paramsA, paramsB} {
		for _, p := range params {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			k := paramKey(p)
			if i, ok := index[k]; ok {
				merged[i] = p
				continue
			}
			index[k] = len(merged)
			merged = append(merged, p)
		}
	}

	out, err := jsoniter.Marshal(merged)
	if err != nil {
		return b
	}
	return string(out)
}

// DamonMonitorRegion returns the guest physical address range DAMON must
// monitor: the kernel's default single-block pick misses RAM above the PCI
// hole. The values mirror the memory map built by CubeSandbox's in-tree VMM
// (hypervisor/arch/src/{x86_64,aarch64}/mod.rs arch_memory_regions), which
// CubeShim embeds.
//
// The guest physical memory map is arch-specific. Cubelet runs on the worker
// node that boots the guest, so passing runtime.GOARCH here yields the guest
// arch; memBytes must be the guest RAM size of the finalized VM spec.
func DamonMonitorRegion(goarch string, memBytes uint64) (start, end uint64, ok bool) {
	const gib = uint64(1) << 30
	if memBytes == 0 {
		return 0, 0, false
	}
	switch goarch {
	case "amd64":
		// x86_64 keeps RAM contiguous up to 3 GiB, then skips the 3-4 GiB
		// 32-bit device hole and resumes at 4 GiB, so the top of RAM is size
		// below the hole and size + 1 GiB above it.
		// A guest with at most one page of RAM has no room for the first page,
		// so fail closed rather than emit start >= end.
		if memBytes <= 4096 {
			return 0, 0, false
		}
		if memBytes <= 3*gib {
			end = memBytes
		} else {
			end = memBytes + gib
		}
		// DAMON treats monitor_region_start=0 as "unset", so start at the
		// first page; the 4 KiB below it carries no RAM.
		return 4096, end, true
	case "arm64":
		// aarch64 starts RAM at 1 GiB (RAM_START) and skips a 64 MiB hole at
		// 3.9375 GiB (MEM_32BIT_RESERVED_START) before resuming at 4 GiB
		// (RAM_64BIT_START), so the top of RAM is size + 1 GiB below the hole
		// and size + 1.0625 GiB above it.
		const (
			ramStart   = uint64(0x4000_0000)   // 1 GiB
			holeStart  = uint64(0xfc00_0000)   // 3.9375 GiB
			ram64Start = uint64(0x1_0000_0000) // 4 GiB
		)
		ram32bitSpace := holeStart - ramStart // 2.9375 GiB
		if memBytes <= ram32bitSpace {
			end = ramStart + memBytes
		} else {
			end = ram64Start + (memBytes - ram32bitSpace)
		}
		return ramStart, end, true
	}
	return 0, 0, false
}

// FillDamonMonitorRegion fills monitor_region_start / monitor_region_end into
// the JSON cmdline list. memBytes is the guest RAM size of the finalized VM
// spec, so the derived region matches the physical memory map for every
// instance type.
//
// When the list carries no damon_reclaim.* entry it is returned untouched.
// Otherwise there are two modes:
//
//   - Neither bound is set (a zero value counts as unset): both bounds are
//     derived from DamonMonitorRegion. When the region cannot be derived
//     (unsupported arch or zero RAM) an error is returned, so the guest never
//     silently falls back to DAMON's incomplete default region.
//   - At least one bound is set: both bounds must be positive integers with
//     start < end, and the list is returned untouched.
func FillDamonMonitorRegion(raw, goarch string, memBytes uint64) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var params []string
	if err := jsoniter.Unmarshal([]byte(raw), &params); err != nil {
		return "", fmt.Errorf("kernel cmdline append must be a JSON array of strings: %w", err)
	}

	const (
		startPrefix = "damon_reclaim.monitor_region_start="
		endPrefix   = "damon_reclaim.monitor_region_end="
	)
	var start, end uint64
	hasDamon := false
	for _, p := range params {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "damon_reclaim.") {
			hasDamon = true
		}
		var err error
		switch {
		case strings.HasPrefix(p, startPrefix):
			start, err = parseBound(p, startPrefix)
		case strings.HasPrefix(p, endPrefix):
			end, err = parseBound(p, endPrefix)
		}
		if err != nil {
			return "", err
		}
	}
	// No DAMON param requested: leave the list untouched.
	if !hasDamon {
		return raw, nil
	}

	// An explicit bound was set: require the full pair and keep it as-is.
	// Repeated monitor_region entries are kept verbatim; the kernel honors the
	// last occurrence.
	if start != 0 || end != 0 {
		if start == 0 || end == 0 || start >= end {
			return "", fmt.Errorf(
				"damon_reclaim.monitor_region_start and damon_reclaim.monitor_region_end "+
					"must both be set to positive integers with start < end (got %d and %d)",
				start, end)
		}
		return raw, nil
	}

	// Neither bound was set: derive both. Fail closed when the region cannot be
	// derived so the guest never silently falls back to DAMON's incomplete
	// default region.
	start, end, ok := DamonMonitorRegion(goarch, memBytes)
	if !ok {
		return "", fmt.Errorf(
			"cannot derive damon_reclaim monitor region for arch %q with %d bytes of guest RAM; "+
				"set both damon_reclaim.monitor_region_start and damon_reclaim.monitor_region_end explicitly",
			goarch, memBytes)
	}
	kept := make([]string, 0, len(params)+2)
	for _, p := range params {
		p = strings.TrimSpace(p)
		// Drop zero placeholders; the derived pair below replaces them.
		if p == "" || strings.HasPrefix(p, startPrefix) || strings.HasPrefix(p, endPrefix) {
			continue
		}
		kept = append(kept, p)
	}
	kept = append(kept,
		fmt.Sprintf("%s%d", startPrefix, start),
		fmt.Sprintf("%s%d", endPrefix, end))
	out, err := jsoniter.Marshal(kept)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseBound parses the value of a "<key>=<value>" monitor_region entry.
func parseBound(param, prefix string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(param, prefix)), 0, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a decimal or 0x-prefixed integer: %w", param, err)
	}
	return v, nil
}
