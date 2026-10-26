// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pathutil

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ifNameRE matches the character set allowed in Linux network interface
// names. The kernel limit is IFNAMSIZ-1 = 15, but Cube may use slightly
// longer aliases, so the bound is conservatively raised to 32.
var ifNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,32}$`)

// uuidRE matches UUID-like strings. The 8-4-4-4-12 grouping is not
// enforced because some upstream hardware UUIDs (e.g. cube disk UUIDs)
// omit the hyphens; we only enforce a strict character allowlist and a
// reasonable length bound.
var uuidRE = regexp.MustCompile(`^[A-Fa-f0-9-]{1,64}$`)

func ValidateSafeID(id string) error {
	if id == "" {
		return fmt.Errorf("id cannot be empty")
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid id %q: contains path separators or traversal sequences", id)
	}
	return nil
}

func ValidatePathUnderBase(basePath, inputPath string) (string, error) {
	if inputPath == "" {
		return "", fmt.Errorf("path cannot be empty")
	}
	cleaned := filepath.Clean(inputPath)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("path %q is not absolute", inputPath)
	}
	baseAbs, err := filepath.Abs(basePath)
	if err != nil {
		return "", fmt.Errorf("invalid base path %q: %w", basePath, err)
	}
	inputAbs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", inputPath, err)
	}
	if inputAbs != baseAbs && !strings.HasPrefix(inputAbs, baseAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is not under base %q", inputPath, basePath)
	}
	return inputAbs, nil
}

func ValidateNoTraversal(p string) error {
	if p == "" {
		return nil
	}
	cleaned := filepath.Clean(p)
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("path %q contains traversal sequence", p)
	}
	return nil
}

// ValidateIfName validates a network interface name that originated from
// external input. Only [A-Za-z0-9_.:-] with a length of 1..32 is allowed.
// Callers must reject the request when this returns a non-nil error.
func ValidateIfName(name string) error {
	if name == "" {
		return fmt.Errorf("ifname cannot be empty")
	}
	if !ifNameRE.MatchString(name) {
		return fmt.Errorf("invalid ifname %q", name)
	}
	return nil
}

// ValidateUUID validates a UUID-like string that originated from external
// input. Only hexadecimal digits and hyphens are allowed, with a length of
// 1..64. Callers must reject the request when this returns a non-nil error.
func ValidateUUID(id string) error {
	if id == "" {
		return fmt.Errorf("uuid cannot be empty")
	}
	if !uuidRE.MatchString(id) {
		return fmt.Errorf("invalid uuid %q", id)
	}
	return nil
}
