// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cow

import "testing"

func TestBackendNames(t *testing.T) {
	t.Parallel()
	if NameXfsCow != "xfscow" {
		t.Fatalf("NameXfsCow=%q", NameXfsCow)
	}
	if NameS3 != "s3" {
		t.Fatalf("NameS3=%q", NameS3)
	}
	if KindVolume != "volume" || KindSnapshot != "snapshot" {
		t.Fatalf("kinds volume=%q snapshot=%q", KindVolume, KindSnapshot)
	}
}
