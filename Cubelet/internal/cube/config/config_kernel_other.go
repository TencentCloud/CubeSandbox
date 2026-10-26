// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package config

import (
	"context"
)

func ValidateEnableUnprivileged(ctx context.Context, c *RuntimeConfig) error {
	return nil
}
