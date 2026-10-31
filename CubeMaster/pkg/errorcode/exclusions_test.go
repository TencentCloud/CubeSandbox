// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package errorcode

import (
	"sync"
	"testing"

	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestCapacityRetryExclusionBeforeAndAfterConfiguration(t *testing.T) {
	before := excludeLoopRetryCodes
	t.Cleanup(func() { excludeLoopRetryCodes = before })
	code := MasterCode(cubeleterrorcode.ErrorCode_HostResourcesNotEnough)
	excludeLoopRetryCodes = nil
	if IsExcludesRetryCode(code) {
		t.Fatal("unconfigured exclusion rejected capacity retry")
	}
	excludeLoopRetryCodes = new(sync.Map)
	excludeLoopRetryCodes.Store(code, struct{}{})
	if !IsExcludesRetryCode(code) {
		t.Fatal("configured capacity exclusion was ignored")
	}
}
