// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package version

import "testing"

func TestVersion(t *testing.T) {
	if len(Version) == 0 {
		t.Fatal("version not set")
	}
}
