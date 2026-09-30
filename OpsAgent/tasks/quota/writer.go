// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package quota implements the quota task domain: validating a desired spec,
// merging it into the cubelet dynamicconf (host.quota section) with a
// text-anchored rewrite that preserves unrelated lines and comments, and
// reporting the currently effective values.
package quota

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

// Spec is the desired quota carried on push/pull; zero values mean "use
// cubelet defaults". JSON keys match CubeOps' OpsAgentPushRequest.Spec.
type Spec struct {
	MCpuLimit                  int64   `json:"mcpu_limit"`
	MemLimit                   string  `json:"mem_limit"`
	MvmLimit                   int64   `json:"mvm_limit"`
	CreationConcurrentNum      int64   `json:"creation_concurrent_num"`
	PausedResourceReleaseRatio float64 `json:"paused_resource_release_ratio"`

	// OnlyRatio restricts Matches/Merge to the ratio key (cluster-managed
	// rows); set from the payload flag, not serialized.
	OnlyRatio bool `json:"-"`
}

// PhysicalTotals mirrors CubeOps' capacity snapshot for node-side guards.
type PhysicalTotals struct {
	CpuTotal   int64 `json:"cpu_total"`
	MemMBTotal int64 `json:"mem_mb_total"`
}

// Fixed overcommit ceilings, kept in sync with CubeOps' validateQuotaSpec.
const (
	hardMaxCPUOvercommit = 20.0
	hardMaxMemOvercommit = 3.0
)

// quotaKeys are the dynamicconf keys under host.quota this domain owns, in
// canonical order. The bool says whether the value is quoted in yaml.
var quotaKeyOrder = []struct {
	key    string
	quoted bool
}{
	{"mcpu_limit", false},
	{"mem_limit", true},
	{"mvm_limit", false},
	{"creation_concurrent_num", false},
	{"paused_resource_release_ratio", false},
}

// memLimitRe accepts only binary suffixes (Ki/Mi/Gi/Ti); anything else
// silently parses as bytes in cubelet and desyncs spec from actual.
var memLimitRe = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?)(Ti|Gi|Mi|Ki)$`)

// Validate checks the spec shape and the overcommit guards.
func (s *Spec) Validate(p PhysicalTotals) error {
	if s.MCpuLimit < 0 || s.MvmLimit < 0 || s.CreationConcurrentNum < 0 {
		return fmt.Errorf("negative quota values are not allowed")
	}
	if r := s.PausedResourceReleaseRatio; r < 0 || r > 1 {
		return fmt.Errorf("paused_resource_release_ratio must be in [0,1], got %v", r)
	}
	var memMB int64
	if s.MemLimit != "" {
		var err error
		if memMB, err = parseMemLimitMB(s.MemLimit); err != nil {
			return err
		}
	}
	if p.CpuTotal > 0 && s.MCpuLimit > 0 {
		if limit := int64(float64(p.CpuTotal) * 1000 * hardMaxCPUOvercommit); s.MCpuLimit > limit {
			return fmt.Errorf("mcpu_limit %d exceeds node guard %d (cpu=%d x %v)", s.MCpuLimit, limit, p.CpuTotal, hardMaxCPUOvercommit)
		}
	}
	if p.MemMBTotal > 0 && memMB > 0 {
		if limit := int64(float64(p.MemMBTotal) * hardMaxMemOvercommit); memMB > limit {
			return fmt.Errorf("mem_limit %dMB exceeds node guard %dMB (mem=%dMB x %v)", memMB, limit, p.MemMBTotal, hardMaxMemOvercommit)
		}
	}
	return nil
}

func (s *Spec) valueOf(key string) string {
	switch key {
	case "mcpu_limit":
		return strconv.FormatInt(s.MCpuLimit, 10)
	case "mem_limit":
		return `"` + s.MemLimit + `"`
	case "mvm_limit":
		return strconv.FormatInt(s.MvmLimit, 10)
	case "creation_concurrent_num":
		return strconv.FormatInt(s.CreationConcurrentNum, 10)
	case "paused_resource_release_ratio":
		return strconv.FormatFloat(s.PausedResourceReleaseRatio, 'g', -1, 64)
	}
	return ""
}

// Effective returns the host.quota values currently present in content.
func Effective(content string) (map[string]string, error) {
	var doc struct {
		Host struct {
			Quota map[string]interface{} `yaml:"quota"`
		} `yaml:"host"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("parse dynamicconf: %w", err)
	}
	out := make(map[string]string, len(doc.Host.Quota))
	for k, v := range doc.Host.Quota {
		switch tv := v.(type) {
		case string:
			out[k] = tv
		case float64:
			out[k] = strconv.FormatFloat(tv, 'g', -1, 64)
		case uint64:
			out[k] = strconv.FormatUint(tv, 10)
		case int:
			out[k] = strconv.Itoa(tv)
		default:
			out[k] = fmt.Sprintf("%v", tv)
		}
	}
	return out, nil
}

// managedKeys returns the dynamicconf keys this spec is authoritative for,
// in canonical order. The bool says whether the value is quoted in yaml.
func (s *Spec) managedKeys() []struct {
	key    string
	quoted bool
} {
	if s.OnlyRatio {
		return quotaKeyOrder[len(quotaKeyOrder)-1:] // the ratio key alone
	}
	return quotaKeyOrder
}

// Matches reports whether the host.quota section of content already equals
// the spec (the desired raw values), i.e. applying it would be a no-op.
func Matches(content string, spec *Spec) (bool, error) {
	effective, err := Effective(content)
	if err != nil {
		return false, err
	}
	for _, q := range spec.managedKeys() {
		want := spec.valueOf(q.key)
		want = strings.Trim(want, `"`)
		if effective[q.key] != want {
			return false, nil
		}
	}
	return true, nil
}

// Merge rewrites the host.quota section of content so that the owned keys
// carry the spec values. It is a text-anchored edit: lines and comments
// outside the owned "key: value" lines are preserved byte-for-byte.
func Merge(content string, spec *Spec) (string, error) {
	lines := strings.Split(content, "\n")

	hostIdx := findSectionKey(lines, 0, "host")
	if hostIdx < 0 {
		return "", fmt.Errorf("dynamicconf has no top-level host section")
	}
	quotaIdx := findSectionKey(lines, hostIdx+1, "quota")
	if quotaIdx < 0 {
		return "", fmt.Errorf("dynamicconf host section has no quota subsection")
	}

	// quota section body: until the next key at host-child indentation.
	bodyStart := quotaIdx + 1
	bodyEnd := len(lines)
	for i := bodyStart; i < len(lines); i++ {
		if isKeyLine(lines[i], "  ") {
			bodyEnd = i
			break
		}
	}

	replaced := map[string]bool{}
	out := make([]string, 0, len(lines)+len(quotaKeyOrder))
	out = append(out, lines[:bodyStart]...)
	for _, line := range lines[bodyStart:bodyEnd] {
		replacedLine := line
		for _, q := range spec.managedKeys() {
			if keyOfLine(line, "    ") == q.key {
				replacedLine = fmt.Sprintf("    %s: %s", q.key, spec.valueOf(q.key))
				replaced[q.key] = true
				break
			}
		}
		out = append(out, replacedLine)
	}
	// Append owned keys that were absent from the section, after the last
	// non-blank, non-comment line of the body so trailing comments stay put.
	for _, q := range spec.managedKeys() {
		if !replaced[q.key] {
			out = append(out, fmt.Sprintf("    %s: %s", q.key, spec.valueOf(q.key)))
		}
	}
	out = append(out, lines[bodyEnd:]...)

	merged := strings.Join(out, "\n")
	// Read-back guard: the merged document must parse and carry exactly the
	// spec values. This catches any anchoring bug before it reaches the file.
	ok, err := Matches(merged, spec)
	if err != nil {
		return "", fmt.Errorf("merged dynamicconf does not parse: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("merged dynamicconf does not carry the spec values (internal merge bug)")
	}
	return merged, nil
}

// findSectionKey returns the index of the "<indent>name:" line at the given
// indentation, searching until a line at a lower indentation appears.
func findSectionKey(lines []string, from int, name string) int {
	for i := from; i < len(lines); i++ {
		if isKeyLine(lines[i], "") && keyOfLine(lines[i], "") == name {
			return i
		}
	}
	return -1
}

// isKeyLine reports whether line is a mapping key at exactly indent spaces.
func isKeyLine(line, indent string) bool {
	if indent != "" && !strings.HasPrefix(line, indent) {
		return false
	}
	if indent != "" && strings.HasPrefix(strings.TrimPrefix(line, indent), " ") {
		return false // deeper indentation: child of another key
	}
	trimmed := strings.TrimPrefix(line, indent)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return false
	}
	return strings.Contains(trimmed, ":")
}

// keyOfLine extracts the mapping key of a "<indent>key: ..." line.
func keyOfLine(line, indent string) string {
	trimmed := strings.TrimPrefix(line, indent)
	if i := strings.Index(trimmed, ":"); i > 0 {
		return strings.TrimSpace(trimmed[:i])
	}
	return ""
}

// parseMemLimitMB accepts the quantity shapes cubelet's dynamicconf uses and
// returns whole megabytes rounded up so guards never under-count.
func parseMemLimitMB(v string) (int64, error) {
	m := memLimitRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, fmt.Errorf("invalid mem_limit %q: want a binary-suffix quantity, e.g. 256Gi / 262144Mi", v)
	}
	num, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid mem_limit %q: %w", v, err)
	}
	switch m[3] {
	case "Ki":
		num /= 1024
	case "Mi":
		// already MB
	case "Gi":
		num *= 1024
	case "Ti":
		num *= 1024 * 1024
	}
	return int64(num + 0.9999), nil
}

// hostPhysical reads the node's real capacity — the overcommit guards'
// authoritative source — CPU cores via runtime.NumCPU, memory via /proc/meminfo.
func hostPhysical() (PhysicalTotals, error) {
	memKB, err := readMemTotalKB()
	if err != nil {
		return PhysicalTotals{}, err
	}
	return PhysicalTotals{
		CpuTotal:   int64(runtime.NumCPU()),
		MemMBTotal: memKB / 1024,
	}, nil
}

// readMemTotalKB returns the MemTotal value from /proc/meminfo in kB.
func readMemTotalKB() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return parseMemTotalKB(string(data))
}

// parseMemTotalKB extracts the MemTotal kB value from /proc/meminfo content.
func parseMemTotalKB(data string) (int64, error) {
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("malformed MemTotal line in /proc/meminfo")
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal in /proc/meminfo: %w", err)
		}
		return kb, nil
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
