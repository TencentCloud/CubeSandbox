// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package version

import "testing"

func TestVersion(t *testing.T) {
	if len(Version) == 0 {
		t.Fatal("version not set")
	}
	Version = "1.3.1"
	if Version >= "1.3.2" {
		t.Log(Version)
	}
	Version = "1.3.2"
	if Version >= "1.3.2" {
		t.Log("yes")
	}
}
