// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func localConf() config.WebhookConf {
	c := config.DefaultWebhookConf()
	c.Enabled = true
	c.AllowHTTP = true
	c.AllowedCIDRs = []string{"127.0.0.0/8", "::1/128"}
	c.RequestTimeout = 150 * time.Millisecond
	c.Store.OperationTimeout = 100 * time.Millisecond
	c.Lease = 3 * time.Second
	return c
}
func TestSenderExactHeadersAndStatuses(t *testing.T) {
	for _, status := range []int{200, 204, 302, 400, 401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			task := testTask()
			var requests atomic.Int32
			ids := make(chan string, 2)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				b, _ := io.ReadAll(r.Body)
				if string(b) != string(task.Event.Body()) {
					t.Error("body changed")
				}
				if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("e2b-webhook-id") != task.SubscriptionID || r.Header.Get("e2b-signature") != "" || r.Header.Get("e2b-signature-version") != "" {
					t.Error("headers")
				}
				ids <- r.Header.Get("e2b-delivery-id")
				w.Header().Set("Location", "http://127.0.0.1:1/should-not-follow")
				w.WriteHeader(status)
			}))
			defer srv.Close()
			task.TargetURL = srv.URL
			s := NewSender(localConf())
			defer s.Close()
			for i := 0; i < 2; i++ {
				v := s.Send(testCtx, task, uuid.NewString())
				require.Equal(t, status, v.Summary.HTTPStatus)
				require.True(t, v.Attempted)
			}
			require.EqualValues(t, 2, requests.Load())
			require.NotEqual(t, <-ids, <-ids)
		})
	}
}
func TestSenderAddressPolicy(t *testing.T) {
	s := NewSender(localConf())
	defer s.Close()
	for _, tc := range []struct {
		ip string
		ok bool
	}{{"8.8.8.8", true}, {"2606:4700:4700::1111", true}, {"127.0.0.1", true}, {"::1", true}, {"::ffff:127.0.0.1", true}, {"10.1.1.1", false}, {"172.16.1.1", false}, {"192.168.1.1", false}, {"fc00::1", false}, {"169.254.169.254", false}, {"100.100.100.200", false}, {"0.0.0.0", false}, {"224.0.0.1", false}, {"::", false}, {"fe80::1", false}, {"2001:db8::1", false}, {"198.18.0.1", false}, {"192.0.2.1", false}} {
		require.Equal(t, tc.ok, s.permits(netip.MustParseAddr(tc.ip), false), tc.ip)
	}
	require.False(t, s.permits(netip.MustParseAddr("8.8.8.8"), true))
	s.allowed = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	require.False(t, s.permits(netip.MustParseAddr("169.254.169.254"), false))
	require.False(t, s.permits(netip.MustParseAddr("fe80::1"), false))
	require.True(t, s.permits(netip.MustParseAddr("10.0.0.1"), false))
	s.allowed = nil
	var dials atomic.Int32
	s.dial = func(context.Context, string, string) (net.Conn, error) { dials.Add(1); return nil, errAddress }
	s.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	_, err := s.dialContext(testCtx, "tcp", "example.test:443")
	require.ErrorIs(t, err, errAddress)
	require.Zero(t, dials.Load())
	// New resolution is checked for every fresh connection; no hostname redial.
	s.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	s.dial = func(_ context.Context, _ string, addr string) (net.Conn, error) {
		require.Equal(t, "8.8.8.8:443", addr)
		dials.Add(1)
		return nil, errAddress
	}
	_, _ = s.dialContext(testCtx, "tcp", "example.test:443")
	require.EqualValues(t, 1, dials.Load())
	s.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}
	_, _ = s.dialContext(testCtx, "tcp", "example.test:443")
	require.EqualValues(t, 1, dials.Load())
}
func TestSenderTLSProxyRedirectAndLimits(t *testing.T) {
	var proxyHits, redirectHits atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyHits.Add(1) }))
	defer trap.Close()
	t.Setenv("HTTP_PROXY", trap.URL)
	t.Setenv("HTTPS_PROXY", trap.URL)
	t.Setenv("ALL_PROXY", trap.URL)
	t.Setenv("NO_PROXY", "")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectHits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	s := NewSender(localConf())
	defer s.Close()
	task := testTask()
	task.TargetURL = redirect.URL
	require.Equal(t, 302, s.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	require.Zero(t, redirectHits.Load())
	require.Zero(t, proxyHits.Load())
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer tlsServer.Close()
	task.TargetURL = tlsServer.URL
	require.Zero(t, s.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	roots := x509.NewCertPool()
	roots.AddCert(tlsServer.Certificate())
	trusted := NewSender(localConf())
	defer trusted.Close()
	trusted.transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	require.Equal(t, 204, trusted.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	wrong := NewSender(localConf())
	defer wrong.Close()
	wrong.transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "wrong.example", MinVersion: tls.VersionTLS12}
	require.Zero(t, wrong.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	for _, kind := range []string{"slow-header", "slow-body", "large-header", "large-body", "disconnect"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch kind {
				case "slow-header":
					<-r.Context().Done()
				case "slow-body":
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				case "large-header":
					w.Header().Set("X-Large", strings.Repeat("x", 32000))
					w.WriteHeader(200)
				case "large-body":
					_, _ = io.WriteString(w, strings.Repeat("x", 100000))
				case "disconnect":
					c, _, _ := w.(http.Hijacker).Hijack()
					c.Close()
				}
			}))
			defer srv.Close()
			task.TargetURL = srv.URL
			start := time.Now()
			v := s.Send(testCtx, task, uuid.NewString())
			require.Less(t, time.Since(start), time.Second)
			if kind == "slow-body" || kind == "large-body" {
				require.Equal(t, 200, v.Summary.HTTPStatus)
			} else {
				require.Zero(t, v.Summary.HTTPStatus)
			}
		})
	}
	ctx, cancel := context.WithCancel(testCtx)
	cancel()
	require.Zero(t, s.Send(ctx, task, uuid.NewString()).Summary.HTTPStatus)
}
func TestProviderSnapshot(t *testing.T) {
	c := localConf()
	c.Subscriptions = []Subscription{{ID: "one", Enabled: true, URL: "https://example.test/token?secret=hidden", Events: []string{TypeCreated}}}
	p, err := NewStaticConfigSubscriptionProvider(c)
	require.NoError(t, err)
	c.Subscriptions[0].Events[0] = TypeKilled
	c.Subscriptions[0].URL = "https://changed.test"
	got := p.List()
	require.Equal(t, TypeCreated, got[0].Events[0])
	got[0].Events[0] = TypeKilled
	require.Equal(t, TypeCreated, p.List()[0].Events[0])
	require.Contains(t, p.List()[0].URL, "example.test")
}

func TestSenderReusedConnectionDoesNotReplayPost(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(204)
			return
		}
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()
	s := NewSender(localConf())
	defer s.Close()
	task := testTask()
	task.TargetURL = srv.URL
	require.Equal(t, 204, s.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	require.Zero(t, s.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	require.EqualValues(t, 2, calls.Load())
}
