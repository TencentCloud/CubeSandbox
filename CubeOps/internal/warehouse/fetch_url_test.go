// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"strings"
	"testing"
)

func TestReleaseURLEscapesTag(t *testing.T) {
	t.Parallel()
	tag := "v1.0.0#rc"
	gh := githubReleaseURL("TencentCloud/CubeSandbox", tag, "amd64")
	cnb := cnbReleaseURL("CubeSandbox/CubeSandbox", tag, "amd64")
	if strings.Contains(gh, "/v1.0.0#rc/") || !strings.Contains(gh, "%23") {
		t.Fatalf("github URL did not escape tag: %s", gh)
	}
	if strings.Contains(cnb, "/v1.0.0#rc/") || !strings.Contains(cnb, "%23") {
		t.Fatalf("cnb URL did not escape tag: %s", cnb)
	}
}
