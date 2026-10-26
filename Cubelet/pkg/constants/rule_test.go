// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import "testing"

func TestRegexContainerName(t *testing.T) {
	validNames := []string{
		"my-container",
		"container-123",
		"c-1",
	}
	invalidNames := []string{
		"-container",
		"container123456789012345678901234567890123456789012345678901234567890",
		"container.1",
		"container_1",
	}

	for _, name := range validNames {
		if !RegexContainerName.MatchString(name) {
			t.Errorf("RegexContainerName failed to match valid name: %s", name)
		}
	}

	for _, name := range invalidNames {
		if RegexContainerName.MatchString(name) {
			t.Errorf("RegexContainerName matched invalid name: %s", name)
		}
	}
}
