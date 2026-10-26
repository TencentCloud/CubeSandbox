// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SafeJoinPath joins baseDir with the untrusted component and verifies the
// resulting path stays within baseDir (prevents path traversal attacks).
// Returns an error if the resolved path escapes the base directory.
func SafeJoinPath(baseDir, untrusted string) (string, error) {
	if untrusted == "" {
		return "", fmt.Errorf("path component must not be empty")
	}
	// Reject any component that contains path separators or current/parent dir markers.
	if strings.ContainsAny(untrusted, `/\`) || untrusted == "." || untrusted == ".." ||
		strings.Contains(untrusted, "..") {
		return "", fmt.Errorf("invalid path component %q: contains path traversal characters", untrusted)
	}
	joined := filepath.Join(baseDir, untrusted)
	// Resolve symlinks / ".." via Clean and verify the prefix.
	cleaned := filepath.Clean(joined)
	base := filepath.Clean(baseDir)
	if !strings.HasPrefix(cleaned, base+string(filepath.Separator)) && cleaned != base {
		return "", fmt.Errorf("path %q escapes base directory %q", cleaned, base)
	}
	return cleaned, nil
}
