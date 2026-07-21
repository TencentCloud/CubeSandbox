// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/sandboxid"
)

func resolveSandboxIDFromList(items []*cubebox.CubeSandbox, input string) (string, error) {
	candidates := make([]string, 0, len(items))
	for _, item := range items {
		candidates = append(candidates, item.GetId())
	}
	resolved, err := sandboxid.Resolve(input, candidates)
	if err == nil || !errors.Is(err, sandboxid.ErrNotFound) {
		return resolved, err
	}

	// Preserve historical cubecli behavior: also accept a unique container ID
	// (or prefix) and map it back to the owning sandbox ID.
	return resolveSandboxIDByContainer(items, input)
}

func resolveSandboxIDByContainer(items []*cubebox.CubeSandbox, input string) (string, error) {
	input = sandboxid.NormalizeInput(input)
	if input == "" {
		return "", sandboxid.ErrNotFound
	}
	inputLower := strings.ToLower(input)

	var matches []string
	seen := make(map[string]struct{})
	for _, item := range items {
		sandboxID := item.GetId()
		if sandboxID == "" {
			continue
		}
		for _, container := range item.GetContainers() {
			containerID := container.GetId()
			if containerID == "" {
				continue
			}
			containerLower := strings.ToLower(containerID)
			if containerLower != inputLower && !strings.HasPrefix(containerLower, inputLower) {
				continue
			}
			if _, ok := seen[sandboxID]; ok {
				continue
			}
			seen[sandboxID] = struct{}{}
			matches = append(matches, sandboxID)
			break
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q", sandboxid.ErrNotFound, input)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: %q matches %d sandboxes", sandboxid.ErrAmbiguous, input, len(matches))
	}
}
