// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/volumecontent"
)

const e2eDriver = "e2e-direct"

var (
	e2ePlugin = &leasePlugin{name: e2eDriver, hostPath: func(r *volume.AttachRequest) string {
		return filepath.Join(r.VolumeBaseDir, "e2e-"+r.VolumeID)
	}}
	registerE2EPlugin sync.Once
)

// e2eEnv wires every Cubelet-side component of the direct volume path with
// nothing mocked except CubeMaster (an HTTP server) and the storage backend
// (a plugin that "mounts" a local directory):
//
//	HTTP client -> content.Handler -> token verify -> MasterChecker -> fake CubeMaster
//	            -> ManagerLeaser -> volume.Manager (+ bbolt ref-count) -> plugin -> os.Root file ops
type e2eEnv struct {
	t      *testing.T
	srv    *httptest.Server
	master *fakeMaster
	clk    *clock
	store  *refcount.Store
	cfg    *volumecontent.Config

	// The plugin is process-wide, so counts are relative to this baseline.
	baseAttached, baseDetached int64

	mu      sync.Mutex
	volumes map[string]bool // volumes CubeMaster knows about
	down    atomic.Bool     // CubeMaster answers 500
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	registerE2EPlugin.Do(func() { volume.Register(e2ePlugin) })

	db, err := bolt.Open(filepath.Join(t.TempDir(), "rc.db"), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := refcount.New(db)
	if err != nil {
		t.Fatal(err)
	}
	volume.SetRefCountStore(store)
	t.Cleanup(func() { volume.SetRefCountStore(nil) })

	e := &e2eEnv{t: t, store: store, volumes: map[string]bool{}}
	e.baseAttached, e.baseDetached = e2ePlugin.attached.Load(), e2ePlugin.detached.Load()
	e.master = newFakeMaster(t)
	e.master.set(func(id string) (int, string) {
		if e.down.Load() {
			return http.StatusInternalServerError, "boom"
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.volumes[id] {
			return http.StatusOK, foundBody(id)
		}
		return http.StatusOK, notFoundBody
	})

	checker, clk := newChecker(e.master)
	e.clk = clk

	cfg, err := volumecontent.NewConfig(testSecret, "", "https://vol.example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg

	h, err := NewHandler(Options{
		Tokens:  cfg,
		Leaser:  &ManagerLeaser{Mgr: volume.Global(), Namespace: "default", BaseDir: t.TempDir()},
		Volumes: checker,
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *e2eEnv) createVolume(id string) string {
	e.mu.Lock()
	e.volumes[id] = true
	e.mu.Unlock()
	tok, err := e.cfg.Mint(id, e2eDriver, "volumes/"+id+"/", volumecontent.PermReadWrite, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *e2eEnv) deleteVolume(id string) {
	e.mu.Lock()
	delete(e.volumes, id)
	e.mu.Unlock()
}

func (e *e2eEnv) do(vol, method, route, token, path string, body []byte) (*http.Response, []byte) {
	e.t.Helper()
	target := e.srv.URL + "/volumecontent/" + vol + "/" + route + "?" + url.Values{"path": {path}}.Encode()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (e *e2eEnv) want(resp *http.Response, body []byte, status int) {
	e.t.Helper()
	if resp.StatusCode != status {
		e.t.Fatalf("status %d, want %d; body=%s", resp.StatusCode, status, body)
	}
}

// mounts is how many times the storage plugin was asked to attach and detach.
func (e *e2eEnv) mounts() (attached, detached int64) {
	return e2ePlugin.attached.Load() - e.baseAttached, e2ePlugin.detached.Load() - e.baseDetached
}

func (e *e2eEnv) refs(vol string) int64 {
	e.t.Helper()
	n, err := e.store.Get("default", vol)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestDirectVolumeAccessEndToEnd follows one volume through its whole life on
// the direct path: normal use, deletion, control-plane outage and recovery.
func TestDirectVolumeAccessEndToEnd(t *testing.T) {
	e := newE2EEnv(t)
	const vol = "vol-e2e"
	token := e.createVolume(vol)

	t.Run("read and write without a sandbox", func(t *testing.T) {
		resp, body := e.do(vol, "POST", "dir", token, "/docs", nil)
		e.want(resp, body, http.StatusCreated)
		resp, body = e.do(vol, "PUT", "file", token, "/docs/a.txt", []byte("hello"))
		e.want(resp, body, http.StatusCreated)
		resp, body = e.do(vol, "GET", "file", token, "/docs/a.txt", nil)
		e.want(resp, body, http.StatusOK)
		if string(body) != "hello" {
			t.Fatalf("read back %q", body)
		}
		resp, body = e.do(vol, "GET", "dir", token, "/docs", nil)
		e.want(resp, body, http.StatusOK)
		if !strings.Contains(string(body), "a.txt") {
			t.Fatalf("listing: %s", body)
		}
		resp, body = e.do(vol, "GET", "path", token, "/missing", nil)
		e.want(resp, body, http.StatusNotFound)

		// One CubeMaster lookup served all five requests.
		if got := e.master.calls.Load(); got != 1 {
			t.Fatalf("CubeMaster lookups = %d, want 1", got)
		}
		attached, detached := e.mounts()
		if attached != 5 || attached != detached {
			t.Fatalf("plugin attach/detach = %d/%d, want 5/5", attached, detached)
		}
		if n := e.refs(vol); n != 0 {
			t.Fatalf("ref-count leaked: %d", n)
		}
	})

	t.Run("content holders are not sandboxes", func(t *testing.T) {
		att := e2ePlugin.lastAttach.Load().(*volume.AttachRequest)
		if !strings.HasPrefix(att.SandboxID, refcount.TransientHolderPrefix) || att.Driver != e2eDriver ||
			att.PrivateData != "volumes/"+vol+"/" {
			t.Fatalf("attach request: %+v", att)
		}
		if att.NodeRefFirstAttach {
			t.Fatal("a content request must not report a node 0->1 transition to CubeMaster")
		}
		det := e2ePlugin.lastDetach.Load().(*volume.DetachRequest)
		if det.NodeRefLastDetach || det.RefCount != 0 {
			t.Fatalf("detach request: %+v", det)
		}
	})

	t.Run("deleted volume stays readable only within the cache window", func(t *testing.T) {
		e.deleteVolume(vol)
		resp, body := e.do(vol, "GET", "file", token, "/docs/a.txt", nil)
		e.want(resp, body, http.StatusOK)

		e.clk.advance(foundTTL + time.Second)
		attachedBefore, _ := e.mounts()
		for _, r := range []struct{ method, route string }{
			{"GET", "file"}, {"PUT", "file"}, {"GET", "dir"}, {"POST", "dir"},
			{"GET", "path"}, {"PATCH", "path"}, {"DELETE", "path"},
		} {
			resp, body := e.do(vol, r.method, r.route, token, "/docs/a.txt", []byte("{}"))
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s after delete: status %d, want 404 (%s)", r.method, r.route, resp.StatusCode, body)
			}
		}
		if attachedAfter, _ := e.mounts(); attachedAfter != attachedBefore {
			t.Fatalf("plugin Attach ran %d times for a deleted volume", attachedAfter-attachedBefore)
		}
		if got := e.master.calls.Load(); got != 2 {
			t.Fatalf("CubeMaster lookups = %d, want 2 (the 404 answer is cached briefly)", got)
		}
		if n := e.refs(vol); n != 0 {
			t.Fatalf("ref-count leaked: %d", n)
		}
	})

	t.Run("unconfirmed volume fails closed and recovers", func(t *testing.T) {
		const other = "vol-other"
		otherToken := e.createVolume(other)
		e.down.Store(true)

		attachedBefore, _ := e.mounts()
		resp, body := e.do(other, "GET", "path", otherToken, "/", nil)
		e.want(resp, body, http.StatusServiceUnavailable)
		if resp.Header.Get("Retry-After") == "" {
			t.Fatal("503 without Retry-After")
		}
		resp, body = e.do(other, "PUT", "file", otherToken, "/x", []byte("data"))
		e.want(resp, body, http.StatusServiceUnavailable)
		if attachedAfter, _ := e.mounts(); attachedAfter != attachedBefore {
			t.Fatal("plugin Attach ran while CubeMaster could not confirm the volume")
		}

		// Errors are not cached: the next request after recovery succeeds.
		e.down.Store(false)
		resp, body = e.do(other, "PUT", "file", otherToken, "/x", []byte("data"))
		e.want(resp, body, http.StatusCreated)
		resp, body = e.do(other, "GET", "file", otherToken, "/x", nil)
		e.want(resp, body, http.StatusOK)
		if n := e.refs(other); n != 0 {
			t.Fatalf("ref-count leaked: %d", n)
		}
	})
}
