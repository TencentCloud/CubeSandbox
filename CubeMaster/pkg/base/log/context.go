// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package log

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var (
	G = GetLogger
)

type loggerKey struct{}

func WithLogger(ctx context.Context, e *CubeLog.Entry) context.Context {
	return context.WithValue(ctx, loggerKey{}, e)
}

func GetLogger(ctx context.Context) *CubeLog.Entry {
	logger := ctx.Value(loggerKey{})

	if logger == nil {
		return CubeLog.WithContext(ctx)
	}
	return logger.(*CubeLog.Entry)
}

func ReNewLogger(ctx context.Context) context.Context {
	old := ctx.Value(loggerKey{})
	if old == nil {
		return ctx
	}
	return WithLogger(ctx, CubeLog.WithContext(ctx))
}
