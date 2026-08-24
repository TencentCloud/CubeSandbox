// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeRoundTripper lets sender tests exercise the full client.Do path
// (headers, signature, classification) without binding a listener — this
// environment forbids listen sockets in the test process.
type fakeRoundTripper struct {
	fn func(*http.Request) (*http.Response, error)
}

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.fn(r)
}

func testSenderWith(fn func(*http.Request) (*http.Response, error)) *Sender {
	return newSenderWithClient(&http.Client{
		Transport: fakeRoundTripper{fn: fn},
		Timeout:   5 * time.Second,
	})
}

func testDelivery(url string) *DeliveryForSend {
	return &DeliveryForSend{
		ID:             1,
		EventID:        "test:1",
		Payload:        []byte(`{"event":"sandbox.created"}`),
		SubscriptionID: 42,
		URL:            url,
		Secret:         "s3cr3t",
	}
}

func okResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}
}

func TestSender_SignsPayload(t *testing.T) {
	var gotSig, gotEvent, gotDelivery, gotTs string
	var gotBody []byte
	s := testSenderWith(func(r *http.Request) (*http.Response, error) {
		gotSig = r.Header.Get("X-Cube-Signature-256")
		gotEvent = r.Header.Get("X-Cube-Event-ID")
		gotDelivery = r.Header.Get("X-Cube-Delivery")
		gotTs = r.Header.Get("X-Cube-Timestamp")
		gotBody, _ = io.ReadAll(r.Body)
		return okResponse(), nil
	})

	d := testDelivery("https://example.com/hook")
	res := s.Send(context.Background(), d)
	if res.Class != ResultSucceeded {
		t.Fatalf("class = %q, want succeeded (%v)", res.Class, res.Err)
	}
	mac := hmac.New(sha256.New, []byte(d.Secret))
	mac.Write(d.Payload)
	if want := hex.EncodeToString(mac.Sum(nil)); gotSig != want {
		t.Fatalf("signature = %q, want %q", gotSig, want)
	}
	if gotEvent != "test:1" || gotDelivery != "test:1:42" {
		t.Fatalf("headers event=%q delivery=%q", gotEvent, gotDelivery)
	}
	if gotTs == "" {
		t.Fatal("X-Cube-Timestamp missing")
	}
	if string(gotBody) != string(d.Payload) {
		t.Fatalf("body changed: %q", gotBody)
	}
}

func TestSender_Classification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		rtErr     error
		wantClass string
	}{
		{"2xx", http.StatusOK, nil, ResultSucceeded},
		{"4xx permanent", http.StatusBadRequest, nil, ResultPermanent},
		{"408 retryable", http.StatusRequestTimeout, nil, ResultRetryable},
		{"429 retryable", http.StatusTooManyRequests, nil, ResultRetryable},
		{"5xx retryable", http.StatusInternalServerError, nil, ResultRetryable},
		{"redirect retryable", http.StatusFound, nil, ResultRetryable},
		{"network error", 0, errors.New("connection refused"), ResultRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testSenderWith(func(_ *http.Request) (*http.Response, error) {
				if tc.rtErr != nil {
					return nil, tc.rtErr
				}
				resp := okResponse()
				resp.StatusCode = tc.status
				resp.Status = http.StatusText(tc.status)
				return resp, nil
			})
			res := s.Send(context.Background(), testDelivery("https://example.com/hook"))
			if res.Class != tc.wantClass {
				t.Fatalf("class = %q, want %q (err=%v)", res.Class, tc.wantClass, res.Err)
			}
		})
	}
}

func TestSender_ShutdownCancelIsNotAttempt(t *testing.T) {
	var wg sync.WaitGroup
	s := testSenderWith(func(r *http.Request) (*http.Response, error) {
		wg.Add(1)
		defer wg.Done()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	res := s.Send(ctx, testDelivery("https://example.com/hook"))
	wg.Wait()
	if res.Class != ResultShutdown {
		t.Fatalf("class = %q, want shutdown", res.Class)
	}
}

func TestSender_NoSecretOmitsSignature(t *testing.T) {
	var got string
	s := testSenderWith(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Get("X-Cube-Signature-256")
		return okResponse(), nil
	})
	d := testDelivery("https://example.com/hook")
	d.Secret = ""
	if res := s.Send(context.Background(), d); res.Class != ResultSucceeded {
		t.Fatalf("send failed: %v", res.Err)
	}
	if got != "" {
		t.Fatalf("unsigned delivery must not set X-Cube-Signature-256, got %q", got)
	}
}

func TestPinnedDialContext_RejectsBeforeDial(t *testing.T) {
	// Rejection happens before any socket is created, so no listener is
	// needed: loopback / private / metadata / mapped-v6 must all error.
	rejected := []string{
		"127.0.0.1:80", "::1:80", "10.0.0.1:80", "192.168.1.1:80",
		"169.254.169.254:80", "100.64.0.1:80", "224.0.0.1:80", "::ffff:10.0.0.1:80",
	}
	dial := pinnedDialContext(false)
	for _, addr := range rejected {
		if _, err := dial(context.Background(), "tcp", addr); err == nil {
			t.Errorf("%s should be rejected pre-dial", addr)
		}
	}
	// With allow_private_networks=true, loopback/private are no longer
	// rejected at the policy layer (dialing them may still fail in a
	// sandbox, but the error must NOT be an SSRF policy error).
	loose := pinnedDialContext(true)
	if _, err := loose(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("127.0.0.1 should pass policy but fail to connect")
	} else if strings.Contains(err.Error(), "denied range") || strings.Contains(err.Error(), "private") {
		t.Fatalf("allow_private_networks=true must relax loopback, got policy error: %v", err)
	}
}

func TestCheckSSRFAddr(t *testing.T) {
	rejected := []string{
		"127.0.0.1", "::1", "::ffff:127.0.0.1", "169.254.169.254",
		"::ffff:169.254.169.254", "10.1.2.3", "::ffff:10.1.2.3", "192.168.1.1",
		"224.0.0.1", "ff02::1", "::", "fe80::1", "100.64.0.1",
	}
	for _, s := range rejected {
		ip := netip.MustParseAddr(s)
		if err := checkSSRFAddr(ip, false); err == nil {
			t.Errorf("%s should be rejected", s)
		}
	}
	// Private + loopback are allowed only with allow_private_networks=true.
	for _, s := range []string{"10.1.2.3", "127.0.0.1", "::1"} {
		if err := checkSSRFAddr(netip.MustParseAddr(s), true); err != nil {
			t.Errorf("%s with allow=true should pass: %v", s, err)
		}
	}
	// Always-denied ranges stay denied even with the flag.
	for _, s := range []string{"100.64.0.1", "224.0.0.1", "::", "ff02::1"} {
		if err := checkSSRFAddr(netip.MustParseAddr(s), true); err == nil {
			t.Errorf("%s should stay rejected with allow=true", s)
		}
	}
	// Public addresses always pass.
	for _, s := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if err := checkSSRFAddr(netip.MustParseAddr(s), false); err != nil {
			t.Errorf("%s should pass: %v", s, err)
		}
	}
}

func TestSender_SSRFCounterIncrementsOnRejectedSend(t *testing.T) {
	before := testutil.ToFloat64(ssrfRejectedTotal)
	dial := pinnedDialContext(false)
	_, _ = dial(context.Background(), "tcp", "127.0.0.1:80")
	if testutil.ToFloat64(ssrfRejectedTotal) <= before {
		t.Fatal("ssrf rejected counter not incremented")
	}
}
