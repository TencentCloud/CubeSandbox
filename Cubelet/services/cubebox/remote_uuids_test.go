// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func TestUploadRemoteUUIDsSkipsXFS(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"", cow.BackendXFS, "cubecow"} {
		if raw := uploadRemoteUUIDs(context.Background(), backend, "snap-1"); raw != "" {
			t.Fatalf("backend %q: unexpected uuids %s", backend, raw)
		}
	}
}
