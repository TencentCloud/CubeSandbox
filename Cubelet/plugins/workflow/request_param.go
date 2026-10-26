// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"fmt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/disk"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func GetQosFromReq(req *cubebox.RunCubeSandboxRequest, key string) (*disk.RateLimiter, error) {
	if req == nil {
		return nil, fmt.Errorf("reqinfo is nil")
	}
	v, ok := req.GetAnnotations()[key]
	if !ok || v == "" {
		return nil, nil
	}
	r := &disk.RateLimiter{}
	err := utils.Decode(v, r)
	if err != nil {
		return nil, fmt.Errorf("%v decode fail:%s", key, v)
	}
	return r, nil
}
