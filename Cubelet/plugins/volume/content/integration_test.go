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

const integDriver = "integ-direct"

// integAttachHook runs inside the storage plugin's Attach. Tests use it to see
// the control-plane state at that moment. It must not block: the Manager holds
// a per-volume lock for the duration of Attach.
var (
	integAttachHook atomic.Pointer[func(*volume.AttachRequest)]
	integPlugin     = &leasePlugin{name: integDriver, hostPath: func(r *volume.AttachRequest) string {
		if h := integAttachHook.Load(); h != nil {
			(*h)(r)
		}
		return filepath.Join(r.VolumeBaseDir, "integ-"+r.VolumeID)
	}}
	registerIntegPlugin sync.Once
)

// integEnv wires every Cubelet-side component of the direct volume path with
// nothing mocked except CubeMaster (a stateful HTTP stand-in) and the storage
// backend (a plugin that "mounts" a local directory):
//
//	HTTP client -> content.Handler -> token verify -> MasterRefs -> CubeMaster
//	            -> ManagerLeaser -> volume.Manager (+ bbolt ref-count) -> plugin -> os.Root file ops
type integEnv struct {
	t      *testing.T
	srv    *httptest.Server
	master *fakeMaster
	clk    *clock
	store  *refcount.Store
	cfg    *volumecontent.Config
	refs   *MasterRefs

	// The plugin is process-wide, so counts are relative to this baseline.
	baseAttached, baseDetached int64
}

func newIntegEnv(t *testing.T) *integEnv {
	t.Helper()
	registerIntegPlugin.Do(func() { volume.Register(integPlugin) })

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
	t.Cleanup(func() { integAttachHook.Store(nil) })

	e := &integEnv{t: t, store: store}
	e.baseAttached, e.baseDetached = integPlugin.attached.Load(), integPlugin.detached.Load()
	e.master = newFakeMaster(t)
	e.refs, e.clk = newRefs(t, e.master)

	cfg, err := volumecontent.NewConfig(testSecret, "", "https://vol.example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg

	h, err := NewHandler(Options{
		Tokens: cfg,
		Leaser: &ManagerLeaser{Mgr: volume.Global(), Namespace: "default", BaseDir: t.TempDir()},
		Refs:   e.refs,
		Logf:   t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	return e
}

// createVolume is what CubeMaster does on volume create/get: the volume exists
// and a content token is minted for it.
func (e *integEnv) createVolume(id string) string {
	e.master.addVolume(id)
	tok, err := e.cfg.Mint(id, integDriver, "volumes/"+id+"/", volumecontent.PermReadWrite, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *integEnv) do(vol, method, route, token, path string, body []byte) (*http.Response, []byte) {
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

func (e *integEnv) want(resp *http.Response, body []byte, status int) {
	e.t.Helper()
	if resp.StatusCode != status {
		e.t.Fatalf("status %d, want %d; body=%s", resp.StatusCode, status, body)
	}
}

// mounts is how many times the storage plugin was asked to attach and detach.
func (e *integEnv) mounts() (attached, detached int64) {
	return integPlugin.attached.Load() - e.baseAttached, integPlugin.detached.Load() - e.baseDetached
}

// nodeRefs is the Cubelet-local ref-count for a volume.
func (e *integEnv) nodeRefs(vol string) int64 {
	e.t.Helper()
	n, err := e.store.Get("default", vol)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// inFlight starts a PUT whose body is not finished, so a request stays in
// progress after the volume is attached (blocking inside the plugin would hold
// the Manager's per-volume lock and stall every other request). It returns once
// the plugin has attached. masterRefsAtAttach is what CubeMaster counted at that
// moment.
func (e *integEnv) inFlight(vol, token string) (finish func() int, masterRefsAtAttach *atomic.Int64) {
	attached := make(chan struct{})
	seen := &atomic.Int64{}
	var once sync.Once
	hook := func(r *volume.AttachRequest) {
		once.Do(func() {
			seen.Store(e.master.refcount(r.VolumeID))
			close(attached)
		})
	}
	integAttachHook.Store(&hook)

	pr, pw := io.Pipe()
	// If the test fails before finish runs, the open body would keep the server
	// from closing and turn the failure into a hang.
	e.t.Cleanup(func() { _ = pw.Close() })
	status := make(chan int, 1)
	go func() {
		target := e.srv.URL + "/volumecontent/" + vol + "/file?" + url.Values{"path": {"/slow.txt"}}.Encode()
		req, _ := http.NewRequest(http.MethodPut, target, pr)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- -1
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	go func() { _, _ = pw.Write([]byte("da")) }()
	select {
	case <-attached:
	case <-time.After(3 * time.Second):
		e.t.Fatal("request never reached the plugin")
	}
	return func() int {
		_, _ = pw.Write([]byte("ta"))
		_ = pw.Close()
		select {
		case s := <-status:
			return s
		case <-time.After(3 * time.Second):
			e.t.Fatal("in-flight request never finished")
			return 0
		}
	}, seen
}

// A volume's life on the direct path: files are read and written without a
// sandbox, the volume cannot be deleted while a request uses it, and once it
// is deleted its token no longer reaches the backend.
func TestDirectVolumeAccessIntegration(t *testing.T) {
	e := newIntegEnv(t)
	const vol = "vol-integ"
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

		attached, detached := e.mounts()
		if attached != 5 || attached != detached {
			t.Fatalf("plugin attach/detach = %d/%d, want 5/5", attached, detached)
		}
		if n := e.nodeRefs(vol); n != 0 {
			t.Fatalf("node ref-count leaked: %d", n)
		}
		if n := e.master.refcount(vol); n != 0 {
			t.Fatalf("CubeMaster refcount = %d after the requests, want 0 (nothing may stay pinned)", n)
		}
		if e.master.acquires.Load() != 5 || e.master.appliedRelease.Load() != 5 {
			t.Fatalf("CubeMaster acquires/releases = %d/%d, want 5/5",
				e.master.acquires.Load(), e.master.appliedRelease.Load())
		}
	})

	t.Run("content holders are not sandboxes on the node", func(t *testing.T) {
		att := integPlugin.lastAttach.Load().(*volume.AttachRequest)
		if !strings.HasPrefix(att.SandboxID, refcount.TransientHolderPrefix) || att.Driver != integDriver ||
			att.PrivateData != "volumes/"+vol+"/" {
			t.Fatalf("attach request: %+v", att)
		}
		// The node-level sandbox event channel (ext_info) stays untouched: the
		// direct path reports through its own reference instead.
		if att.NodeRefFirstAttach {
			t.Fatal("a content request must not emit a sandbox ext_info transition")
		}
		det := integPlugin.lastDetach.Load().(*volume.DetachRequest)
		if det.NodeRefLastDetach || det.RefCount != 0 {
			t.Fatalf("detach request: %+v", det)
		}
	})

	t.Run("a running request pins the volume and takes the reference before attaching", func(t *testing.T) {
		finish, refsAtAttach := e.inFlight(vol, token)
		if got := refsAtAttach.Load(); got != 1 {
			t.Fatalf("CubeMaster refcount when the plugin attached = %d, want 1: the reference must be taken first", got)
		}
		if err := e.master.deleteVolume(vol); err == nil {
			t.Fatal("DELETE /volumes must be refused while a content request is running")
		}
		if status := finish(); status != http.StatusCreated {
			t.Fatalf("in-flight request finished with %d", status)
		}
		if n := e.master.refcount(vol); n != 0 {
			t.Fatalf("CubeMaster refcount = %d after the request, want 0", n)
		}
	})

	t.Run("sandbox and direct access share one count", func(t *testing.T) {
		e.master.sandboxRef(vol, 1) // a sandbox on another node mounts the volume
		resp, body := e.do(vol, "GET", "file", token, "/docs/a.txt", nil)
		e.want(resp, body, http.StatusOK)
		if n := e.master.refcount(vol); n != 1 {
			t.Fatalf("refcount = %d: finishing the request must leave the sandbox's reference alone", n)
		}
		if err := e.master.deleteVolume(vol); err == nil {
			t.Fatal("the sandbox still uses the volume, so it must not be deletable")
		}
		e.master.sandboxRef(vol, -1) // the sandbox is destroyed
	})

	t.Run("a deleted volume is never attached", func(t *testing.T) {
		if err := e.master.deleteVolume(vol); err != nil {
			t.Fatalf("an idle volume must be deletable: %v", err)
		}
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
		if e.master.exists(vol) {
			t.Fatal("the request must not bring the volume back")
		}
	})
}

// Requests that overlap share a single CubeMaster reference, and the volume is
// released only when the last of them is done.
func TestOverlappingRequestsShareOneReference(t *testing.T) {
	e := newIntegEnv(t)
	const vol = "vol-overlap"
	token := e.createVolume(vol)

	finish, _ := e.inFlight(vol, token)
	// A second request is served while the first is still running.
	resp, body := e.do(vol, "GET", "path", token, "/", nil)
	e.want(resp, body, http.StatusOK)
	if n := e.master.refcount(vol); n != 1 {
		t.Fatalf("refcount = %d while the first request still runs, want 1", n)
	}
	if got := e.master.acquires.Load(); got != 1 {
		t.Fatalf("CubeMaster acquires = %d, want 1 shared reference", got)
	}
	if status := finish(); status != http.StatusCreated {
		t.Fatalf("first request finished with %d", status)
	}
	if n := e.master.refcount(vol); n != 0 {
		t.Fatalf("refcount = %d after both requests, want 0", n)
	}
}

// When CubeMaster cannot confirm a volume the request is refused and the
// backend is not touched; the next request after it recovers works.
func TestControlPlaneOutageFailsClosedAndRecovers(t *testing.T) {
	e := newIntegEnv(t)
	const vol = "vol-outage"
	token := e.createVolume(vol)

	e.master.down.Store(true)
	attachedBefore, _ := e.mounts()
	resp, body := e.do(vol, "GET", "path", token, "/", nil)
	e.want(resp, body, http.StatusServiceUnavailable)
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
	resp, body = e.do(vol, "PUT", "file", token, "/x", []byte("data"))
	e.want(resp, body, http.StatusServiceUnavailable)
	if attachedAfter, _ := e.mounts(); attachedAfter != attachedBefore {
		t.Fatal("plugin Attach ran while CubeMaster could not confirm the volume")
	}

	e.master.down.Store(false)
	e.clk.advance(failedTTL + time.Second)
	resp, body = e.do(vol, "PUT", "file", token, "/x", []byte("data"))
	e.want(resp, body, http.StatusCreated)
	if n := e.master.refcount(vol); n != 0 {
		t.Fatalf("refcount = %d, want 0", n)
	}
}

// A volume that is already being served keeps working through an outage, and
// the reference is still returned afterwards.
func TestVolumeInUseSurvivesControlPlaneOutage(t *testing.T) {
	e := newIntegEnv(t)
	const vol = "vol-survive"
	token := e.createVolume(vol)

	finish, _ := e.inFlight(vol, token)
	e.master.down.Store(true)
	resp, body := e.do(vol, "GET", "path", token, "/", nil)
	e.want(resp, body, http.StatusOK)

	if status := finish(); status != http.StatusCreated {
		t.Fatalf("in-flight request finished with %d", status)
	}
	if n := e.master.refcount(vol); n != 1 {
		t.Fatalf("refcount = %d: the release could not be delivered yet, it must be kept", n)
	}
	e.master.down.Store(false)
	eventually(t, "the reference to be released once CubeMaster is back", func() bool {
		return e.master.refcount(vol) == 0
	})
	if err := e.master.deleteVolume(vol); err != nil {
		t.Fatalf("volume must be deletable once the request is done: %v", err)
	}
}

// A release whose outcome is unknown is not repeated, so a reference that
// belongs to a sandbox is never taken away by a direct request.
func TestLostReleaseResponseDoesNotStealASandboxReference(t *testing.T) {
	e := newIntegEnv(t)
	const vol = "vol-lost"
	token := e.createVolume(vol)

	e.master.sandboxRef(vol, 1) // a sandbox uses the volume
	e.master.loseRelease.Store(true)
	resp, body := e.do(vol, "GET", "path", token, "/", nil)
	e.want(resp, body, http.StatusOK)
	time.Sleep(100 * time.Millisecond)

	if n := e.master.refcount(vol); n != 1 {
		t.Fatalf("refcount = %d, want 1: the sandbox's reference must remain", n)
	}
	if err := e.master.deleteVolume(vol); err == nil {
		t.Fatal("the volume is in use by a sandbox and must not be deletable")
	}
}
