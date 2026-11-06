// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

var errAddress = errors.New("webhook: destination blocked")

// Special-use ranges never become public destinations. Private and loopback
// addresses alone can be enabled by an explicit CIDR; metadata cannot.
var reservedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "168.63.129.16/32", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "fec0::/10", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

type Sender struct {
	client    *http.Client
	transport *http.Transport
	allowHTTP bool
	timeout   time.Duration
	allowed   []netip.Prefix
	lookup    func(context.Context, string) ([]net.IPAddr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
}
type SendResult struct {
	Summary   FailureSummary
	Attempted bool
}

func NewSender(c config.WebhookConf) *Sender {
	s := &Sender{allowHTTP: c.AllowHTTP, timeout: c.RequestTimeout, lookup: net.DefaultResolver.LookupIPAddr, dial: (&net.Dialer{Timeout: c.RequestTimeout, KeepAlive: 30 * time.Second}).DialContext}
	for _, v := range c.AllowedCIDRs {
		p, e := netip.ParsePrefix(v)
		if e == nil {
			s.allowed = append(s.allowed, p)
		}
	}
	s.transport = &http.Transport{Proxy: nil, DialContext: s.dialContext, MaxConnsPerHost: c.Workers, MaxIdleConns: c.Workers, MaxIdleConnsPerHost: c.Workers, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: c.RequestTimeout, ResponseHeaderTimeout: c.RequestTimeout, MaxResponseHeaderBytes: 16 * 1024, DisableCompression: true}
	s.client = &http.Client{Transport: s.transport, Timeout: c.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return s
}
func (s *Sender) permits(ip netip.Addr, plain bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip == netip.IPv6Loopback() {
		for _, p := range s.allowed {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, p := range reservedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	if ip.IsPrivate() || ip.IsLoopback() {
		for _, p := range s.allowed {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	// Plain HTTP is exclusively an explicit private/local exception.
	return !plain && ip.IsGlobalUnicast() && (ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip))
}

type plainHTTPKey struct{}

func (s *Sender) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errAddress
	}
	ips, err := s.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	plain, _ := ctx.Value(plainHTTPKey{}).(bool)
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip.IP)
		if !ok || ip.Zone != "" || !s.permits(a, plain) {
			return nil, errAddress
		}
	}
	for _, ip := range ips {
		conn, e := s.dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if e == nil {
			return conn, nil
		}
		err = e
		if ctx.Err() != nil {
			break
		}
	}
	if err == nil {
		err = errAddress
	}
	return nil, err
}
func (s *Sender) Send(ctx context.Context, t DeliveryTask, deliveryID string) SendResult {
	u, err := config.ValidateWebhookURL(t.TargetURL, s.allowHTTP)
	if err != nil {
		return SendResult{Summary: FailureSummary{Code: "address_rejected"}}
	}
	ctx = context.WithValue(ctx, plainHTTPKey{}, u.Scheme == "http")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.TargetURL, bytes.NewReader(t.Event.Body()))
	if err != nil {
		return SendResult{Summary: FailureSummary{Code: "invalid_request"}}
	}
	// No GetBody: a reused connection must not transparently replay this POST.
	req.GetBody = nil
	var attempted atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteHeaders: func() { attempted.Store(true) }}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("e2b-webhook-id", t.SubscriptionID)
	req.Header.Set("e2b-delivery-id", deliveryID)
	resp, err := s.client.Do(req)
	if err != nil {
		code := "transport_error"
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			code = "timeout"
		}
		if errors.Is(err, errAddress) {
			code = "address_rejected"
		}
		if ctx.Err() != nil {
			code = "cancelled"
		}
		return SendResult{Summary: FailureSummary{Code: code}, Attempted: attempted.Load()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8*1024))
	if resp.StatusCode < 100 || resp.StatusCode > 599 {
		return SendResult{Summary: FailureSummary{Code: "invalid_status"}, Attempted: true}
	}
	return SendResult{Summary: FailureSummary{Code: "http", HTTPStatus: resp.StatusCode}, Attempted: true}
}
func (s *Sender) Close() { s.transport.CloseIdleConnections() }
