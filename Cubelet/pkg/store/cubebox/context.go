// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import "context"

type cubeboxkey struct{}

func WithCubeBox(ctx context.Context, box *CubeBox) context.Context {
	return context.WithValue(ctx, cubeboxkey{}, box)
}

func GetCubeBox(ctx context.Context) *CubeBox {
	box, ok := ctx.Value(cubeboxkey{}).(*CubeBox)
	if !ok {
		return nil
	}
	return box
}
