// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package remote

import (
	"context"
	"crypto/tls"

	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker/config"
)

func createTunnelTransport(fn createConnFn, proxy *url.URL) *http.Transport {
	return &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			return proxy, nil
		},

		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return fn()
		},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
}

type createConnFn func() (net.Conn, error)

func WithTunnelHttpTransport(fn createConnFn, addr *url.URL) config.UpdateClientFunc {
	return func(client *http.Client) error {
		client.Transport = createTunnelTransport(fn, addr)
		return nil
	}
}
