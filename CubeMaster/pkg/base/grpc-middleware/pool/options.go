// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pool

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

type OptionType int32

const (
	DialTimeout = 3 * time.Second

	BackoffMaxDelay = 3 * time.Second

	KeepAliveTime = time.Duration(10) * time.Second

	KeepAliveTimeout = time.Duration(3) * time.Second

	InitialWindowSize = 1 << 30

	InitialConnWindowSize = 1 << 30

	MaxSendMsgSize = 1 << 30

	MaxRecvMsgSize = 1 << 30

	OverTimerOut = 300

	MaxCheckPoolNum = 10000

	ConnPool OptionType = 0

	SingleConn OptionType = 1
)

type Options struct {
	OptionType OptionType

	Dial func(ua, address string) (*grpc.ClientConn, error)

	MaxIdle int

	MaxActive int

	MaxConcurrentStreams int

	Reuse bool
}

var ConnPoolDefaultOptions = Options{
	Dial:                 Dial,
	MaxIdle:              2,
	MaxActive:            64,
	MaxConcurrentStreams: 64,
	Reuse:                true,
	OptionType:           ConnPool,
}

var SingleConnDefaultOptions = Options{
	Dial:       Dial,
	MaxIdle:    1,
	MaxActive:  1,
	Reuse:      true,
	OptionType: SingleConn,
}

func Dial(ua, address string) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DialTimeout)
	defer cancel()

	return grpc.DialContext(ctx, address, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithBackoffMaxDelay(BackoffMaxDelay),
		grpc.WithInitialWindowSize(InitialWindowSize),
		grpc.WithInitialConnWindowSize(InitialConnWindowSize),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(MaxSendMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxRecvMsgSize)),
		grpc.WithUserAgent("cubemaster-"+ua),
	)

}

func DialTest(address string) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DialTimeout)
	defer cancel()
	return grpc.DialContext(ctx, address, grpc.WithInsecure(), grpc.WithBlock())
}
