// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import "context"

type createContextKeyType struct{}

func WithCreateContext(ctx context.Context, createContext *CreateContext) context.Context {
	return context.WithValue(ctx, createContextKeyType{}, createContext)
}

func GetCreateContext(ctx context.Context) *CreateContext {
	createContext, ok := ctx.Value(createContextKeyType{}).(*CreateContext)
	if !ok {
		return nil
	}
	return createContext
}
