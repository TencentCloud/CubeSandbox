// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package terminalcore contains transport-independent terminal validation and
// process defaults. Keeping it free of containerd dependencies makes the
// security-sensitive rules directly testable on every development platform.
package terminalcore

import (
	"errors"
	"fmt"
	"strings"
)

const (
	MinCols = 2
	MaxCols = 500
	MinRows = 1
	MaxRows = 200
)

func ValidateOpen(sandboxID, containerID string, cols, rows uint32) error {
	if strings.TrimSpace(sandboxID) == "" {
		return errors.New("sandbox_id is required")
	}
	if strings.TrimSpace(containerID) == "" {
		return errors.New("container_id is required")
	}
	if err := ValidateSize(cols, rows); err != nil {
		return err
	}
	return nil
}

func ValidateSize(cols, rows uint32) error {
	if cols < MinCols || cols > MaxCols || rows < MinRows || rows > MaxRows {
		return fmt.Errorf(
			"terminal size must be between %dx%d and %dx%d",
			MinCols,
			MinRows,
			MaxCols,
			MaxRows,
		)
	}
	return nil
}

func Command(args []string) []string {
	if len(args) == 0 {
		return []string{"/bin/sh"}
	}
	return append([]string(nil), args...)
}

func MergeEnv(base, overrides []string) []string {
	result := make([]string, 0, len(base)+len(overrides)+1)
	indexes := make(map[string]int, len(base)+len(overrides))

	merge := func(entries []string) {
		for _, entry := range entries {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || key == "" {
				continue
			}
			normalized := key + "=" + value
			if index, exists := indexes[key]; exists {
				result[index] = normalized
				continue
			}
			indexes[key] = len(result)
			result = append(result, normalized)
		}
	}

	merge(base)
	merge(overrides)
	if _, exists := indexes["TERM"]; !exists {
		result = append(result, "TERM=xterm-256color")
	}
	return result
}
