// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pool

import (
	"errors"

	"google.golang.org/grpc"
)

// New creates a pool for addr. The module-specific dial signature is adapted
// here so the shared lifecycle implementation in pool.go stays independent of
// the CubeMaster/Cubelet Options differences.
func New(ua, address string, option Options) (Pool, error) {
	if address == "" {
		return nil, errors.New("invalid address settings")
	}
	if option.Dial == nil {
		return nil, errors.New("invalid dial settings")
	}
	return newPoolWithSlots(address, option, func() (*grpc.ClientConn, error) {
		return option.Dial(ua, address)
	})
}
