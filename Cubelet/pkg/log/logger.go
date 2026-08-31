// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package log

import (
	"runtime/debug"

	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var AuditLogger *CubeLog.Logger = CubeLog.GetDefaultLogger()

func IsDebug() bool {
	return CubeLog.GetLevel() <= CubeLog.DEBUG
}

func WithJsonValue(obj any) string {
	bs, err := jsoniter.MarshalToString(obj)
	if err != nil {
		return ""
	}
	return bs
}

func WithDebugStack() string {
	return string(debug.Stack())
}
