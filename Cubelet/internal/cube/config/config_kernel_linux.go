// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package config

import (
	"context"
	"errors"
	"fmt"

	kernel "github.com/containerd/containerd/v2/pkg/kernelversion"
)

var kernelGreaterEqualThan = kernel.GreaterEqualThan

func ValidateEnableUnprivileged(ctx context.Context, c *RuntimeConfig) error {
	if c.EnableUnprivilegedICMP || c.EnableUnprivilegedPorts {
		fourDotEleven := kernel.KernelVersion{Kernel: 4, Major: 11}
		ok, err := kernelGreaterEqualThan(fourDotEleven)
		if err != nil {
			return fmt.Errorf("check current system kernel version error: %w", err)
		}
		if !ok {
			return errors.New("unprivileged_icmp and unprivileged_port require kernel version greater than or equal to 4.11")
		}
	}
	return nil
}
