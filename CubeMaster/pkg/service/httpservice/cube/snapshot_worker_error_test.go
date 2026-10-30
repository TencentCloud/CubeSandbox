// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"fmt"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

func TestSnapshotWorkerFailureIsNotAClientParameterError(t *testing.T) {
	err := fmt.Errorf("lookup source: %w", errSandboxWorkerUnavailable)
	if code := snapshotErrorCode(err); code != int(errorcode.ErrorCode_CubeletUnHealthy) {
		t.Fatalf("worker availability was classified as code %d", code)
	}
}
