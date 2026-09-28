// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"context"
)

type traceCtxKey struct{}

func WithRequestTrace(ctx context.Context, trace *RequestTrace) context.Context {
	ctx = context.WithValue(ctx, traceCtxKey{}, trace)
	return ctx
}

func GetTraceInfo(ctx context.Context) *RequestTrace {
	rt := ctx.Value(traceCtxKey{})
	if rt == nil {
		return nil
	}
	return rt.(*RequestTrace)
}
