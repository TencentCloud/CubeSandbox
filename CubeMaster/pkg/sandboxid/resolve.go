// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxid

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	ErrNotFound  = errors.New("sandbox id not found")
	ErrAmbiguous = errors.New("ambiguous sandbox id prefix")

	fullIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// NormalizeInput trims leading and trailing whitespace from a sandbox ID input.
func NormalizeInput(input string) string {
	return strings.TrimSpace(input)
}

// IsFullID reports whether id is a 32-character hexadecimal sandbox ID.
func IsFullID(id string) bool {
	return fullIDPattern.MatchString(id)
}

// Resolve maps a short or full sandbox ID to the unique full ID among candidates.
func Resolve(input string, candidates []string) (string, error) {
	input = NormalizeInput(input)
	if input == "" {
		return "", ErrNotFound
	}

	inputLower := strings.ToLower(input)
	var matches []string
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		candidateLower := strings.ToLower(candidate)
		if candidateLower == inputLower {
			return candidate, nil
		}
		if strings.HasPrefix(candidateLower, inputLower) {
			matches = append(matches, candidate)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q", ErrNotFound, input)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: %q matches %d sandboxes", ErrAmbiguous, input, len(matches))
	}
}
