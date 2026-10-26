// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pool

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	ticketdns = "ticketdns"
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

	DnsRefreshInterval = 30 * time.Second
)

type Options struct {
	OptionType OptionType

	Dial func(address string) (*grpc.ClientConn, error)

	MaxIdle int

	MaxActive int

	MaxConcurrentStreams int

	Reuse bool

	DnsRefreshInterval time.Duration
	WithDnsResolver    bool
}

var ConnPoolDefaultOptions = Options{
	Dial:                 Dial,
	MaxIdle:              2,
	MaxActive:            64,
	MaxConcurrentStreams: 64,
	Reuse:                true,
	OptionType:           ConnPool,
	WithDnsResolver:      false,
	DnsRefreshInterval:   DnsRefreshInterval,
}

var SingleConnDefaultOptions = Options{
	Dial:               Dial,
	MaxIdle:            1,
	MaxActive:          1,
	Reuse:              true,
	OptionType:         SingleConn,
	WithDnsResolver:    false,
	DnsRefreshInterval: DnsRefreshInterval,
}

func Dial(address string) (*grpc.ClientConn, error) {
	var opts = []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(InitialWindowSize),
		grpc.WithInitialConnWindowSize(InitialConnWindowSize),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(MaxSendMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxRecvMsgSize)),
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: DialTimeout,
		}),
	}

	URL, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("failed to parse grpc address %s: %v", address, err)
	}
	if URL.Scheme == ticketdns {
		opts = append(opts, grpc.WithResolvers(
			&dnsBuilder{
				dnsScheme: URL.Scheme,
				refresh:   DnsRefreshInterval,
			},
		))
	}

	return grpc.NewClient(address, opts...)

}

func DialTest(address string) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DialTimeout)
	defer cancel()
	return grpc.DialContext(ctx, address, grpc.WithInsecure(), grpc.WithBlock())
}
