// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
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

// Read the complete request before blocking. Cleanup always releases handlers,
// including when a test fails before the client cancels its request.
type receivedWebhook struct {
	body       []byte
	deliveryID string
	err        error
}

func blockedWebhookReceiver(t *testing.T) (*httptest.Server, <-chan receivedWebhook) {
	t.Helper()
	requests := make(chan receivedWebhook, 4)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		select {
		case requests <- receivedWebhook{body, r.Header.Get("e2b-delivery-id"), err}:
		case <-release:
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv, requests
}

type webhookRoundTripFunc func(*http.Request) (*http.Response, error)

func (f webhookRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSenderTimeoutAndCancellationClassification(t *testing.T) {
	for _, mode := range []string{"request-deadline", "client-timeout", "explicit-cancel", "pre-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			srv, requests := blockedWebhookReceiver(t)
			c := localConf()
			c.RequestTimeout = 30 * time.Second
			s := NewSender(c)
			defer s.Close()
			ctx, cancel := context.WithCancel(testCtx)
			defer cancel()
			want := "cancelled"
			if mode == "request-deadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, time.Second)
				defer deadlineCancel()
				want = "timeout"
			}
			if mode == "client-timeout" {
				s.client.Timeout = time.Second
				want = "timeout"
			}
			if mode == "pre-cancelled" {
				cancel()
			}
			task := testTask()
			task.Target = srv.URL
			done := make(chan SendResult, 1)
			go func() { done <- s.Send(ctx, task, uuid.NewString()) }()
			if mode != "pre-cancelled" {
				select {
				case req := <-requests:
					require.NoError(t, req.err)
					require.Equal(t, task.Event.Body(), req.body)
				case <-time.After(5 * time.Second):
					t.Fatal("receiver did not read the request")
				}
			}
			if mode == "explicit-cancel" {
				cancel()
			}
			select {
			case result := <-done:
				t.Logf("%s: code=%s attempted=%t status=%d context=%v", mode, result.Summary.Code, result.Attempted, result.Summary.HTTPStatus, ctx.Err())
				require.Equal(t, want, result.Summary.Code)
				require.Zero(t, result.Summary.HTTPStatus)
				require.Equal(t, mode != "pre-cancelled", result.Attempted)
			case <-time.After(5 * time.Second):
				t.Fatal("sender did not finish")
			}
			if mode == "client-timeout" {
				require.NoError(t, ctx.Err(), "client timeout must be independent of caller context")
			}
			if mode == "pre-cancelled" {
				require.Empty(t, requests)
			}
		})
	}
}

func TestSenderWrappedErrorsClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"cancelled", context.Canceled, "cancelled"},
		{"network-timeout", &net.DNSError{IsTimeout: true}, "timeout"},
		{"address", errAddress, "address_rejected"},
		{"transport", io.ErrUnexpectedEOF, "transport_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSender(localConf())
			defer s.Close()
			s.client.Transport = webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("wrapped: %w", tc.err)
			})
			result := s.Send(testCtx, testTask(), uuid.NewString())
			require.NoError(t, testCtx.Err())
			require.Equal(t, tc.want, result.Summary.Code)
			require.Zero(t, result.Summary.HTTPStatus)
			require.False(t, result.Attempted)
		})
	}
}

func localConf() config.WebhookConf {
	c := config.DefaultWebhookConf()
	c.Enabled = true
	c.AllowHTTP = true
	c.AllowedCIDRs = []string{"127.0.0.0/8", "::1/128"}
	c.RequestTimeout = 150 * time.Millisecond
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
			task.Target = srv.URL
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
	task.Target = redirect.URL
	require.Equal(t, 302, s.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	require.Zero(t, redirectHits.Load())
	require.Zero(t, proxyHits.Load())
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer tlsServer.Close()
	task.Target = tlsServer.URL
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
			task.Target = srv.URL
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
	c.Subscriptions = []config.WebhookSubscription{{ID: "one", Enabled: true, URL: "https://example.test/token?secret=hidden", Events: []string{TypeCreated}}}
	p, err := testSubscriptions(c)
	require.NoError(t, err)
	c.Subscriptions[0].Events[0] = TypeKilled
	c.Subscriptions[0].URL = "https://changed.test"
	got := p.List()
	require.Equal(t, TypeCreated, got[0].Events[0])
	got[0].Events[0] = TypeKilled
	require.Equal(t, TypeCreated, p.List()[0].Events[0])
	require.Contains(t, p.List()[0].Target, "example.test")
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
	task.Target = srv.URL
	require.Equal(t, 204, s.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	require.Zero(t, s.Send(testCtx, task, uuid.NewString()).Summary.HTTPStatus)
	require.EqualValues(t, 2, calls.Load())
}
