// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package handle

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Raw cgroupfs file readers shared by the v1 and v2 handlers:
// missing files read as zero values, "max" maps to MaxUint64, negative
// numbers map to 0, malformed content is an error.

// ReadKVFile parses a cgroupfs key-value file with one "key value" pair per
// line. A missing file returns a nil map; malformed content is an error.
func ReadKVFile(dir, name string) (map[string]uint64, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	out := make(map[string]uint64)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, v, err := parseKVLine(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, name), err)
		}
		out[key] = v
	}
	return out, sc.Err()
}

// ReadUintFile reads a single-value cgroupfs file. A missing file returns 0
// and no error. "max" maps to math.MaxUint64; negative values map to 0.
func ReadUintFile(dir, name string) (uint64, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	// 32 bytes is enough for the largest uint64 plus a trailing newline.
	buf := make([]byte, 32)
	n, err := f.Read(buf)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(buf[:n]))
	if s == "max" {
		return math.MaxUint64, nil
	}
	v, err := parseUintTolerant(s)
	if err != nil {
		return 0, fmt.Errorf("parse %s: invalid value %q", filepath.Join(dir, name), s)
	}
	return v, nil
}

// parseKVLine expects exactly two whitespace-separated fields.
func parseKVLine(raw string) (string, uint64, error) {
	parts := strings.Fields(raw)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid line %q", raw)
	}
	v, err := parseUintTolerant(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid value in %q", raw)
	}
	return parts[0], v, nil
}

// parseUintTolerant maps negative numbers (cgroup v1 "unlimited") to 0.
func parseUintTolerant(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err == nil {
		return v, nil
	}
	iv, intErr := strconv.ParseInt(s, 10, 64)
	if (intErr == nil && iv < 0) ||
		(intErr != nil && errors.Is(intErr, strconv.ErrRange) && iv < 0) {
		return 0, nil
	}
	return 0, err
}

// clockTicksPerSecond is USER_HZ, fixed at 100 on Linux.
const clockTicksPerSecond = 100

// CpuacctTicksToNanoseconds converts cpuacct.stat ticks to nanoseconds.
func CpuacctTicksToNanoseconds(ticks uint64) uint64 {
	return ticks * uint64(1000000000) / clockTicksPerSecond
}
