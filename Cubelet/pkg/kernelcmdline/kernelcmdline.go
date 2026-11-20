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
	for _, p := range paramsA {
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
	for _, p := range paramsB {
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

// isZeroBound reports whether a monitor_region bound is a zero placeholder
// that should be recomputed. Parsing is numeric to match how the kernel parses
// module params: "0", "00" and "0x0" are all zero. An empty value is also a
// placeholder, while a non-numeric value is left as an explicit bound.
func isZeroBound(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	n, err := strconv.ParseUint(value, 0, 64)
	return err == nil && n == 0
}

// FillDamonMonitorRegion fills monitor_region_start / monitor_region_end into
// the JSON cmdline list. memBytes is the guest RAM size of the finalized VM
// spec, so the derived region matches the physical memory map for every
// instance type.
//
// When the list carries no damon_reclaim.* entry it is returned untouched. When
// the region cannot be derived (unsupported arch or zero RAM) the caller must
// supply both bounds explicitly, otherwise an error is returned so the guest
// never silently falls back to DAMON's incomplete default region. An explicit
// pair is also rejected when start >= end or when a bound is not an integer.
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
	var hasDamon, hasStart, hasEnd, zeroStart, zeroEnd bool
	var rawStart, rawEnd string
	for _, p := range params {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "damon_reclaim.") {
			hasDamon = true
		}
		switch {
		case strings.HasPrefix(p, startPrefix):
			hasStart = true
			rawStart = strings.TrimPrefix(p, startPrefix)
			zeroStart = isZeroBound(rawStart)
		case strings.HasPrefix(p, endPrefix):
			hasEnd = true
			rawEnd = strings.TrimPrefix(p, endPrefix)
			zeroEnd = isZeroBound(rawEnd)
		}
	}
	// No DAMON param requested: leave the list untouched.
	if !hasDamon {
		return raw, nil
	}

	start, end, ok := DamonMonitorRegion(goarch, memBytes)
	if !ok {
		// The region cannot be derived for this arch/VM size. Fail closed
		// unless the caller supplied both bounds explicitly, so the guest never
		// silently falls back to DAMON's incomplete default region.
		if (!hasStart || zeroStart) || (!hasEnd || zeroEnd) {
			return "", fmt.Errorf(
				"cannot derive damon_reclaim monitor region for arch %q with %d bytes of guest RAM; "+
					"set both damon_reclaim.monitor_region_start and damon_reclaim.monitor_region_end explicitly",
				goarch, memBytes)
		}
	}

	// Explicit non-zero bounds win over the computed region; resolve the
	// effective pair so an out-of-range override is rejected instead of being
	// written into the guest cmdline (where damon_reclaim would fail silently).
	effStart, effEnd := start, end
	if hasStart && !zeroStart {
		v, err := strconv.ParseUint(strings.TrimSpace(rawStart), 0, 64)
		if err != nil {
			return "", fmt.Errorf("%s%s must be a decimal or 0x-prefixed integer: %w",
				startPrefix, rawStart, err)
		}
		effStart = v
	}
	if hasEnd && !zeroEnd {
		v, err := strconv.ParseUint(strings.TrimSpace(rawEnd), 0, 64)
		if err != nil {
			return "", fmt.Errorf("%s%s must be a decimal or 0x-prefixed integer: %w",
				endPrefix, rawEnd, err)
		}
		effEnd = v
	}
	if effStart >= effEnd {
		return "", fmt.Errorf("damon_reclaim monitor region start %d must be less than end %d",
			effStart, effEnd)
	}

	kept := make([]string, 0, len(params)+2)
	for _, p := range params {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Drop zero placeholders; they are recomputed below.
		if (zeroStart && strings.HasPrefix(p, startPrefix)) ||
			(zeroEnd && strings.HasPrefix(p, endPrefix)) {
			continue
		}
		kept = append(kept, p)
	}

	// Each bound is filled independently, so a single missing bound is still
	// supplied from the computed region.
	if !hasStart || zeroStart {
		kept = append(kept, fmt.Sprintf("%s%d", startPrefix, start))
	}
	if !hasEnd || zeroEnd {
		kept = append(kept, fmt.Sprintf("%s%d", endPrefix, end))
	}
	if len(kept) == 0 {
		return "", nil
	}
	out, err := jsoniter.Marshal(kept)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
