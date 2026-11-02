// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMaster serves GET /cube/volume/{id} the way CubeMaster does.
type fakeMaster struct {
	srv     *httptest.Server
	calls   atomic.Int64
	mu      sync.Mutex
	handler func(id string) (status int, body string)
}

func newFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	m := &fakeMaster{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		const prefix = "/cube/volume/"
		if r.Method != http.MethodGet || len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
			http.NotFound(w, r)
			return
		}
		m.mu.Lock()
		h := m.handler
		m.mu.Unlock()
		status, body := h(r.URL.Path[len(prefix):])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMaster) set(h func(id string) (int, string)) {
	m.mu.Lock()
	m.handler = h
	m.mu.Unlock()
}

func foundBody(id string) string {
	// CubeMaster also returns a freshly minted token; the checker must ignore it.
	return fmt.Sprintf(`{"ret":{"ret_code":0,"ret_msg":"ok"},"volume":{"volumeID":%q,"name":%q,"token":"secret-token"}}`, id, id)
}

const notFoundBody = `{"ret":{"ret_code":130404,"ret_msg":"volume not found"}}`

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

func newChecker(m *fakeMaster) (*MasterChecker, *clock) {
	clk := &clock{}
	clk.t.Store(time.Unix(1_700_000_000, 0).UnixNano())
	c := NewMasterChecker(func() string { return m.srv.URL })
	c.Now = clk.now
	return c, clk
}

func TestMasterCheckerFoundIsCachedThenExpires(t *testing.T) {
	m := newFakeMaster(t)
	m.set(func(id string) (int, string) { return 200, foundBody(id) })
	c, clk := newChecker(m)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := c.Exists(ctx, "vol-1"); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if got := m.calls.Load(); got != 1 {
		t.Fatalf("CubeMaster calls = %d, want 1 (cached)", got)
	}

	clk.advance(foundTTL + time.Second)
	if err := c.Exists(ctx, "vol-1"); err != nil {
		t.Fatal(err)
	}
	if got := m.calls.Load(); got != 2 {
		t.Fatalf("CubeMaster calls after expiry = %d, want 2", got)
	}
}

func TestMasterCheckerDeletedVolume(t *testing.T) {
	m := newFakeMaster(t)
	m.set(func(id string) (int, string) { return 200, foundBody(id) })
	c, clk := newChecker(m)
	ctx := context.Background()

	if err := c.Exists(ctx, "vol-1"); err != nil {
		t.Fatal(err)
	}
	// The volume is deleted. The cached "exists" answer is honoured only until it expires.
	m.set(func(string) (int, string) { return 200, notFoundBody })
	clk.advance(foundTTL + time.Second)

	if err := c.Exists(ctx, "vol-1"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("deleted volume: %v, want ErrVolumeNotFound", err)
	}
	// A burst for a deleted volume is absorbed by the short negative cache.
	before := m.calls.Load()
	for i := 0; i < 5; i++ {
		if err := c.Exists(ctx, "vol-1"); !errors.Is(err, ErrVolumeNotFound) {
			t.Fatalf("burst %d: %v", i, err)
		}
	}
	if m.calls.Load() != before {
		t.Fatal("negative answer was not cached")
	}
	// It is short-lived, so a re-created volume becomes reachable again.
	m.set(func(id string) (int, string) { return 200, foundBody(id) })
	clk.advance(missingTTL + time.Second)
	if err := c.Exists(ctx, "vol-1"); err != nil {
		t.Fatalf("re-created volume: %v", err)
	}
}

func TestMasterCheckerUnknownAnswerFailsClosedAndIsNotCached(t *testing.T) {
	cases := map[string]func(string) (int, string){
		"http 500":        func(string) (int, string) { return 500, `boom` },
		"http 404":        func(string) (int, string) { return 404, `not here` },
		"db error code":   func(string) (int, string) { return 200, `{"ret":{"ret_code":130500,"ret_msg":"db error"}}` },
		"garbage body":    func(string) (int, string) { return 200, `<html>` },
		"missing volume":  func(string) (int, string) { return 200, `{"ret":{"ret_code":0}}` },
		"other volume id": func(string) (int, string) { return 200, foundBody("someone-else") },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			m := newFakeMaster(t)
			m.set(h)
			c, _ := newChecker(m)
			err := c.Exists(context.Background(), "vol-1")
			if err == nil || errors.Is(err, ErrVolumeNotFound) {
				t.Fatalf("err = %v, want an unknown-answer error", err)
			}
			// Not cached: the next call asks again and can recover.
			m.set(func(id string) (int, string) { return 200, foundBody(id) })
			if err := c.Exists(context.Background(), "vol-1"); err != nil {
				t.Fatalf("after recovery: %v", err)
			}
		})
	}
}

func TestMasterCheckerUnreachableAndUnconfigured(t *testing.T) {
	m := newFakeMaster(t)
	url := m.srv.URL
	m.srv.Close()
	c := NewMasterChecker(func() string { return url })
	if err := c.Exists(context.Background(), "vol-1"); err == nil || errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("unreachable master: %v, want a non-NotFound error", err)
	}
	empty := NewMasterChecker(func() string { return "  " })
	if err := empty.Exists(context.Background(), "vol-1"); err == nil || errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("unconfigured master: %v", err)
	}
}

func TestMasterCheckerRejectsUnsafeVolumeIDsWithoutCalling(t *testing.T) {
	m := newFakeMaster(t)
	m.set(func(id string) (int, string) { return 200, foundBody(id) })
	c, _ := newChecker(m)
	for _, id := range []string{"", "../x", "a/b", "a?b=1", "a b", "a\x00b"} {
		if err := c.Exists(context.Background(), id); !errors.Is(err, ErrVolumeNotFound) {
			t.Errorf("id %q: %v, want ErrVolumeNotFound", id, err)
		}
	}
	if m.calls.Load() != 0 {
		t.Fatalf("unsafe ids reached CubeMaster %d times", m.calls.Load())
	}
}

func TestMasterCheckerCollapsesConcurrentMisses(t *testing.T) {
	m := newFakeMaster(t)
	release := make(chan struct{})
	m.set(func(id string) (int, string) { <-release; return 200, foundBody(id) })
	c, _ := newChecker(m)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Exists(context.Background(), "vol-1"); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := m.calls.Load(); got != 1 {
		t.Fatalf("CubeMaster calls = %d, want 1", got)
	}
}

func TestMasterCheckerCacheIsBounded(t *testing.T) {
	m := newFakeMaster(t)
	m.set(func(id string) (int, string) { return 200, foundBody(id) })
	c, _ := newChecker(m)
	for i := 0; i < maxCacheEntries+50; i++ {
		c.store(fmt.Sprintf("vol-%d", i), true)
	}
	c.mu.Lock()
	n := len(c.cache)
	c.mu.Unlock()
	if n > maxCacheEntries {
		t.Fatalf("cache grew to %d entries, limit %d", n, maxCacheEntries)
	}
}
