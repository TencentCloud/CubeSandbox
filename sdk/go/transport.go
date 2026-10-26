// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"
)

func newControlHTTPClient(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Timeout: cfg.RequestTimeout, Transport: transport}
}

func newDataHTTPClient(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{
		Timeout:   cfg.RequestTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext

	if cfg.ProxyNodeIP != "" {
		target := net.JoinHostPort(cfg.ProxyNodeIP, strconv.Itoa(cfg.ProxyPortHTTP))
		dialer := &net.Dialer{
			Timeout:   cfg.RequestTimeout,
			KeepAlive: 30 * time.Second,
		}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, target)
		}
	}

	return &http.Client{Transport: transport}
}
