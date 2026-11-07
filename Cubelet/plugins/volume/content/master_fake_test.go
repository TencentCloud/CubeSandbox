// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMaster behaves like CubeMaster's volume endpoints that Cubelet uses:
//
//	POST   /cube/volume/{id}/ref   take a reference (404 ret if the volume is gone)
//	DELETE /cube/volume/{id}/ref   give one back (never below zero)
//	deleteVolume                   what DELETE /cube/volume/{id} does: refused
//	                               while the reference count is not zero
//
// Like CubeMaster it answers handled errors with HTTP 200 and a ret code. The
// failure switches let tests reproduce the cases the client must survive.
type fakeMaster struct {
	srv *httptest.Server

	mu   sync.Mutex
	refs map[string]int64 // existing volumes -> reference count

	// Requests received, and changes actually applied.
	acquires, releases              atomic.Int64
	appliedAcquires, appliedRelease atomic.Int64

	down          atomic.Bool // nothing is listening: connections are refused
	rejectRelease atomic.Bool // release is answered with an error and NOT applied
	loseAcquire   atomic.Bool // acquire is applied but the response is lost (502)
	loseRelease   atomic.Bool // release is applied but the response is lost (502)
}

func newFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	m := &fakeMaster{refs: map[string]int64{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

// base is what Cubelet is configured with. While m.down is set it points at a
// port nothing listens on, so connecting fails outright.
func (m *fakeMaster) base() string {
	if m.down.Load() {
		return "http://127.0.0.1:1"
	}
	return m.srv.URL
}

func (m *fakeMaster) addVolume(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[id] = 0
}

func (m *fakeMaster) refcount(id string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refs[id]
}

func (m *fakeMaster) exists(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.refs[id]
	return ok
}

// sandboxRef adjusts the count the way a sandbox on some node does through
// ext_info events.
func (m *fakeMaster) sandboxRef(id string, delta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.refs[id]; ok {
		m.refs[id] += delta
	}
}

// deleteVolume is DELETE /cube/volume/{id}: it fails while the volume is
// referenced, otherwise removes it.
func (m *fakeMaster) deleteVolume(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.refs[id]
	switch {
	case !ok:
		return fmt.Errorf("volume not found: %s", id)
	case n != 0:
		return fmt.Errorf("volume %s is in use by %d node(s)", id, n)
	}
	delete(m.refs, id)
	return nil
}

func (m *fakeMaster) serve(w http.ResponseWriter, r *http.Request) {
	const prefix = "/cube/volume/"
	path := r.URL.Path
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/ref") {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/ref")

	switch r.Method {
	case http.MethodPost:
		m.acquires.Add(1)
		m.mu.Lock()
		_, ok := m.refs[id]
		if ok {
			m.refs[id]++
			m.appliedAcquires.Add(1)
		}
		m.mu.Unlock()
		switch {
		case !ok:
			reply(w, 130404, "volume not found", "")
		case m.loseAcquire.Load():
			http.Error(w, "bad gateway", http.StatusBadGateway)
		default:
			reply(w, 0, "ok", id)
		}
	case http.MethodDelete:
		m.releases.Add(1)
		if m.rejectRelease.Load() {
			reply(w, 130500, "db error", "")
			return
		}
		m.mu.Lock()
		if n, ok := m.refs[id]; ok && n > 0 {
			m.refs[id]--
			m.appliedRelease.Add(1)
		}
		m.mu.Unlock()
		if m.loseRelease.Load() {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		reply(w, 0, "ok", id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func reply(w http.ResponseWriter, code int, msg, volumeID string) {
	w.Header().Set("Content-Type", "application/json")
	if volumeID == "" {
		fmt.Fprintf(w, `{"ret":{"ret_code":%d,"ret_msg":%q}}`, code, msg)
		return
	}
	fmt.Fprintf(w, `{"ret":{"ret_code":%d,"ret_msg":%q},"volumeID":%q}`, code, msg, volumeID)
}

// clock is a settable time source for the client's caches.
type clock struct{ t atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.t.Store(time.Unix(1_700_000_000, 0).UnixNano())
	return c
}

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

// newRefs builds a MasterRefs against m with a controllable clock and fast
// release retries.
func newRefs(t *testing.T, m *fakeMaster) (*MasterRefs, *clock) {
	t.Helper()
	clk := newClock()
	r := NewMasterRefs(m.base)
	r.Now = clk.now
	r.Logf = t.Logf
	r.RetryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	t.Cleanup(r.Close)
	return r, clk
}

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
