// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package log

import (
	"encoding/json"
	"runtime/debug"

	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var AuditLogger *CubeLog.Logger = CubeLog.GetDefaultLogger()

func IsDebug() bool {
	return CubeLog.GetLevel() <= CubeLog.DEBUG
}

func WithJsonValue(obj any) string {
	bs, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(bs)
}

func WithDebugStack() string {
	return string(debug.Stack())
}
