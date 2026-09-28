// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"testing"

	s3driver "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/s3"
)

func TestPutPartSizeConstant(t *testing.T) {
	if PutPartSize != 64<<20 {
		t.Fatalf("PutPartSize=%d want 64MiB so unknown-length uploads do not buffer 512MiB", PutPartSize)
	}
	if PutPartSize != s3driver.PutPartSize {
		t.Fatalf("warehouse PutPartSize=%d s3 driver=%d", PutPartSize, s3driver.PutPartSize)
	}
}
