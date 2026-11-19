// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// throttlingClient puts a store in front of fake that answers the first
// `throttle` requests with 429, the way a store does above its bandwidth quota.
func throttlingClient(t *testing.T, throttle int32, retryAfter string) (*Client, *atomic.Int32) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}}
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) <= throttle {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			http.Error(w, "<Error><Code>MaxBandwidthExceeded</Code></Error>", http.StatusTooManyRequests)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "ak", SecretKey: "sk", PathStyle: true, Prefix: "ckpt"})
	if err != nil {
		t.Fatal(err)
	}
	return c, &seen
}

func fastThrottleBackoff(t *testing.T) {
	t.Helper()
	pb, pm, pa := throttleBackoffBase, throttleBackoffMax, retryAfterMax
	t.Cleanup(func() { throttleBackoffBase, throttleBackoffMax, retryAfterMax = pb, pm, pa })
	throttleBackoffBase, throttleBackoffMax, retryAfterMax = time.Millisecond, 4*time.Millisecond, 20*time.Millisecond
}

func TestThrottledRequestsAreRetriedPatiently(t *testing.T) {
	fastThrottleBackoff(t)
	// Seven 429s in a row exhausted the old five-attempt budget and failed a
	// whole checkpoint transfer.
	c, seen := throttlingClient(t, 7, "1")
	start := time.Now()
	if err := c.Put(context.Background(), "ckpt/k", []byte("v")); err != nil {
		t.Fatalf("throttled PUT not retried to success: %v", err)
	}
	if n := seen.Load(); n != 8 {
		t.Fatalf("%d requests, want 8", n)
	}
	// Retry-After (1 s, capped at 20 ms here) outweighs the 1-4 ms backoff.
	if d := time.Since(start); d < 7*20*time.Millisecond {
		t.Fatalf("Retry-After was not honoured: retried in %s", d)
	}
}

func TestThrottlesAreCountedWithTheirBackoff(t *testing.T) {
	fastThrottleBackoff(t)
	c, _ := throttlingClient(t, 3, "")
	before, retries := Throttles(), RequestRetries()
	if err := c.Put(context.Background(), "ckpt/lineage/sb/c/abc", []byte("v")); err != nil {
		t.Fatal(err)
	}
	got := Throttles().Sub(before)
	if got.Status429 != 3 || got.Status503 != 0 || got.Backoff <= 0 {
		t.Fatalf("throttles %+v, want three 429s and some backoff", got)
	}
	if n := RequestRetries() - retries; n != 3 {
		t.Fatalf("%d retries, want 3", n)
	}
	if k := keyCategory("ckpt/lineage/sb/c/abc"); k != "lineage-chunk" {
		t.Fatalf("key category %q", k)
	}
}

func TestThrottlingThatNeverEndsStillFails(t *testing.T) {
	fastThrottleBackoff(t)
	c, seen := throttlingClient(t, 1000, "")
	err := c.Put(context.Background(), "ckpt/k", []byte("v"))
	if slow, _ := throttled(err); !slow {
		t.Fatalf("want the 429 back, got %v", err)
	}
	if n := seen.Load(); int(n) != throttleAttempts {
		t.Fatalf("%d requests, want %d", n, throttleAttempts)
	}
}

func TestOtherFailuresKeepTheShortBudget(t *testing.T) {
	if _, again := retryWait(requestAttempts, &StatusError{Code: http.StatusInternalServerError}); again {
		t.Fatal("a 500 must not get the throttling budget")
	}
	if _, again := retryWait(requestAttempts, &StatusError{Code: http.StatusTooManyRequests}); !again {
		t.Fatal("a 429 must get more attempts than other failures")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"3":                             3 * time.Second,
		"Mon, 16 Nov 2026 12:00:05 GMT": 5 * time.Second,
		"":                              0,
		"soon":                          0,
		"-1":                            0,
	} {
		if got := parseRetryAfter(v, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", v, got, want)
		}
	}
}

func TestBandwidthLimitPacesEveryTransfer(t *testing.T) {
	t.Cleanup(func() { SetBandwidthLimit(0) })
	SetBandwidthLimit(8 << 20)
	if BandwidthLimit() != 8<<20 {
		t.Fatalf("limit %d", BandwidthLimit())
	}
	c, _ := newTestClient(t)
	ctx := context.Background()
	body := make([]byte, 4<<20)
	start := time.Now()
	// 12 MiB up and 12 MiB down at 8 MiB/s with a 4 MiB burst: at least
	// (24 - 4) / 8 = 2.5 s.
	for i := 0; i < 3; i++ {
		if err := c.Put(ctx, "ckpt/bw/"+string(rune('a'+i)), body); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Get(ctx, "ckpt/bw/"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 2400*time.Millisecond {
		t.Fatalf("24 MiB moved in %s under an 8 MiB/s limit", d)
	}
	SetBandwidthLimit(0)
	if BandwidthLimit() != 0 {
		t.Fatal("limit not removed")
	}
}

func TestLoadConfigBandwidth(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s3.cfg")
	base := "access_key_id=ak\nsecret_access_key=sk\nendpoint=e\nbucket=b\n"
	_ = os.WriteFile(p, []byte(base+"max_bandwidth_mbps=400\n"), 0o600)
	cfg, err := LoadConfig(p)
	if err != nil || cfg.BandwidthBytesPerSecond() != 50_000_000 {
		t.Fatalf("400 Mbit/s: %+v %v", cfg, err)
	}
	_ = os.WriteFile(p, []byte(base+"max_bandwidth_mbps=fast\n"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("a non-numeric max_bandwidth_mbps must be refused")
	}
}
