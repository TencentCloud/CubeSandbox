// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pool

import (
	"errors"

	"google.golang.org/grpc"
)

// New creates a pool for addr. The module-specific dial signature and DNS
// resolver adaptation live here so the shared lifecycle implementation in
// pool.go stays independent of the CubeMaster/Cubelet Options differences.
func New(address string, option Options) (Pool, error) {
	if address == "" {
		return nil, errors.New("invalid address settings")
	}
	if option.Dial == nil {
		return nil, errors.New("invalid dial settings")
	}
	// Validate the caller's address before DNS rewriting so an empty address
	// still fails with the original error instead of becoming "ticketdns:///".
	if err := validatePoolConfig(address, option); err != nil {
		return nil, err
	}
	if option.WithDnsResolver {
		address = ticketdns + ":///" + address
	}
	return newPoolWithSlots(address, option, func() (*grpc.ClientConn, error) {
		return option.Dial(address)
	})
}
