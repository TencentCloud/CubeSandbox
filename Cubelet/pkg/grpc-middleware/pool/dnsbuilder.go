// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pool

import (
	"net"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"google.golang.org/grpc/resolver"
)

type dnsBuilder struct {
	dnsScheme string
	refresh   time.Duration
}

func (b *dnsBuilder) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	r := &dnsResolver{
		target: target,
		cc:     cc,
		ticker: time.NewTicker(b.refresh),
		stop:   make(chan struct{}),
	}
	go r.watcher()
	r.ResolveNow(resolver.ResolveNowOptions{})
	return r, nil
}

func (b *dnsBuilder) Scheme() string {
	return b.dnsScheme
}

type dnsResolver struct {
	target resolver.Target
	cc     resolver.ClientConn
	ticker *time.Ticker
	stop   chan struct{}
}

func (r *dnsResolver) ResolveNow(opts resolver.ResolveNowOptions) {
	addrs := r.resolve()
	r.cc.UpdateState(resolver.State{Addresses: addrs})
}

func (r *dnsResolver) Close() {
	r.ticker.Stop()
	close(r.stop)
}

func (r *dnsResolver) watcher() {
	for {
		select {
		case <-r.ticker.C:
			r.ResolveNow(resolver.ResolveNowOptions{})
		case <-r.stop:
			return
		}
	}
}

func (r *dnsResolver) resolve() []resolver.Address {
	logEntry := log.L.WithFields(CubeLog.Fields{
		"endpoint": r.target.Endpoint(),
	})
	host, port, err := net.SplitHostPort(r.target.Endpoint())
	if err != nil {
		logEntry.Errorf("failed to split host and port: %v", err)
		return nil
	}

	ips, err := net.LookupHost(host)
	if err != nil {
		return nil
	}

	addrs := make([]resolver.Address, len(ips))
	for i, ip := range ips {
		addrs[i] = resolver.Address{Addr: ip + ":" + port}
	}
	return addrs
}
