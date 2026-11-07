// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/volumecontent"
)

const (
	testVolume = "vol-1"
	testSecret = "0123456789abcdef0123456789abcdef"
)

type dirLeaser struct {
	dir      string
	refs     *fakeRefs // when set, records how many references are held around the lease
	acquired atomic.Int64
	released atomic.Int64
	gotDrv   atomic.Value
	gotPD    atomic.Value

	heldAtAcquire atomic.Int64
	heldAtRelease atomic.Int64
}

func (l *dirLeaser) Acquire(_ context.Context, _, driver, privateData string) (string, func(), error) {
	l.acquired.Add(1)
	l.gotDrv.Store(driver)
	l.gotPD.Store(privateData)
	if l.refs != nil {
		l.heldAtAcquire.Store(l.refs.held())
	}
	return l.dir, func() {
		if l.refs != nil {
			l.heldAtRelease.Store(l.refs.held())
		}
		l.released.Add(1)
	}, nil
}

// fakeRefs is a VolumeRefs whose answer tests can change.
type fakeRefs struct {
	err      atomic.Value // errBox; a nil error means the volume can be held
	attempts atomic.Int64 // Hold calls, successful or not
	holds    atomic.Int64 // successful Hold calls
	releases atomic.Int64
}

type errBox struct{ err error }

func (v *fakeRefs) set(err error) { v.err.Store(errBox{err}) }

func (v *fakeRefs) held() int64 { return v.holds.Load() - v.releases.Load() }

func (v *fakeRefs) Hold(context.Context, string) (func(), error) {
	v.attempts.Add(1)
	if b, ok := v.err.Load().(errBox); ok && b.err != nil {
		return nil, b.err
	}
	v.holds.Add(1)
	var once sync.Once
	return func() { once.Do(func() { v.releases.Add(1) }) }, nil
}

type fixture struct {
	t      *testing.T
	srv    *httptest.Server
	dir    string
	outer  string // sibling directory outside the volume
	cfg    *volumecontent.Config
	leaser *dirLeaser
	refs   *fakeRefs
	rw, ro string
}

func newFixture(t *testing.T, maxUpload int64) *fixture {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "volume")
	outer := filepath.Join(base, "outside")
	for _, d := range []string{dir, outer} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := volumecontent.NewConfig(testSecret, "", "https://vol.example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := &fakeRefs{}
	leaser := &dirLeaser{dir: dir, refs: refs}
	h, err := NewHandler(Options{Tokens: cfg, Leaser: leaser, Refs: refs, MaxUpload: maxUpload, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f := &fixture{t: t, srv: srv, dir: dir, outer: outer, cfg: cfg, leaser: leaser, refs: refs}
	f.rw = f.token(testVolume, volumecontent.PermReadWrite)
	f.ro = f.token(testVolume, volumecontent.PermRead)
	return f
}

func (f *fixture) token(volume, perm string) string {
	tok, err := f.cfg.Mint(volume, "s3", "volumes/"+volume+"/", perm, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func (f *fixture) do(method, route, token string, query url.Values, body []byte) (*http.Response, []byte) {
	f.t.Helper()
	target := f.srv.URL + "/volumecontent/" + testVolume + "/" + route
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func q(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func (f *fixture) expect(resp *http.Response, want int, body []byte) {
	f.t.Helper()
	if resp.StatusCode != want {
		f.t.Fatalf("status %d, want %d; body=%s", resp.StatusCode, want, body)
	}
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t, 0)
	expired, _ := f.cfg.Mint(testVolume, "s3", "", volumecontent.PermReadWrite, time.Now().Add(-2*time.Hour))
	cases := []struct {
		name   string
		method string
		route  string
		token  string
		want   int
	}{
		{"missing token", "GET", "path", "", 401},
		{"garbage token", "GET", "path", "not.a.jwt", 401},
		{"expired token", "GET", "path", expired, 401},
		{"other volume", "GET", "path", f.token("vol-2", volumecontent.PermReadWrite), 403},
		{"read-only cannot write", "PUT", "file", f.ro, 403},
		{"read-only cannot mkdir", "POST", "dir", f.ro, 403},
		{"read-only cannot delete", "DELETE", "path", f.ro, 403},
		{"read-only cannot patch", "PATCH", "path", f.ro, 403},
	}
	for _, c := range cases {
		resp, body := f.do(c.method, c.route, c.token, q("path", "/x"), []byte("{}"))
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, resp.StatusCode, c.want, body)
		}
	}
	if f.leaser.acquired.Load() != 0 {
		t.Fatal("volume must not be leased for unauthenticated or unauthorised requests")
	}
	resp, _ := f.do("GET", "path", f.ro, q("path", "/"), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("read-only token must read: %d", resp.StatusCode)
	}
	if f.leaser.gotDrv.Load() != "s3" || f.leaser.gotPD.Load() != "volumes/vol-1/" {
		t.Fatalf("lease got driver=%v privateData=%v", f.leaser.gotDrv.Load(), f.leaser.gotPD.Load())
	}
}

// A request holds the volume before the plugin is ever asked to attach (which
// could recreate a deleted volume), on every route and for both read and write
// tokens, and a failed hold never leaves a reference behind.
func TestDeletedOrUnconfirmedVolumeNeverAttaches(t *testing.T) {
	routes := []struct{ method, route string }{
		{"GET", "file"}, {"PUT", "file"}, {"GET", "dir"}, {"POST", "dir"},
		{"GET", "path"}, {"PATCH", "path"}, {"DELETE", "path"},
	}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"deleted", ErrVolumeNotFound, http.StatusNotFound},
		{"control plane unreachable", errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, 0)
			f.refs.set(c.err)
			for _, r := range routes {
				resp, body := f.do(r.method, r.route, f.rw, q("path", "/x"), []byte("{}"))
				if resp.StatusCode != c.want {
					t.Errorf("%s %s: status %d, want %d (%s)", r.method, r.route, resp.StatusCode, c.want, body)
				}
				if c.want == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") == "" {
					t.Errorf("%s %s: 503 without Retry-After", r.method, r.route)
				}
			}
			if f.leaser.acquired.Load() != 0 {
				t.Fatalf("plugin Attach was reached %d times for a volume that could not be held", f.leaser.acquired.Load())
			}
			if f.refs.attempts.Load() != int64(len(routes)) {
				t.Fatalf("hold attempts = %d, want %d", f.refs.attempts.Load(), len(routes))
			}
			if f.refs.held() != 0 {
				t.Fatalf("%d references leaked", f.refs.held())
			}

			// Once the volume can be held again the same token works.
			f.refs.set(nil)
			resp, body := f.do("GET", "path", f.rw, q("path", "/"), nil)
			f.expect(resp, http.StatusOK, body)
			if f.leaser.acquired.Load() != 1 {
				t.Fatalf("acquired = %d, want 1", f.leaser.acquired.Load())
			}
		})
	}
}

// The reference spans the whole lease: taken before the plugin attaches and
// given back only after it has detached, so the volume cannot be deleted while
// any part of a request still touches it.
func TestReferenceSpansTheLease(t *testing.T) {
	f := newFixture(t, 0)
	resp, body := f.do("PUT", "file", f.rw, q("path", "/a.txt"), []byte("x"))
	f.expect(resp, http.StatusCreated, body)
	if f.leaser.heldAtAcquire.Load() != 1 {
		t.Fatalf("references held when the lease was taken = %d, want 1", f.leaser.heldAtAcquire.Load())
	}
	if f.leaser.heldAtRelease.Load() != 1 {
		t.Fatalf("references held when the lease was released = %d, want 1", f.leaser.heldAtRelease.Load())
	}
	if f.refs.held() != 0 {
		t.Fatalf("%d references leaked after the request", f.refs.held())
	}
}

// Unauthenticated and malformed requests must not cost a control-plane call.
func TestRejectedRequestsDoNotQueryTheControlPlane(t *testing.T) {
	f := newFixture(t, 0)
	f.do("GET", "path", "", q("path", "/x"), nil)
	f.do("GET", "path", "garbage", q("path", "/x"), nil)
	f.do("GET", "path", f.token("vol-2", volumecontent.PermReadWrite), q("path", "/x"), nil)
	f.do("PUT", "file", f.ro, q("path", "/x"), nil)
	f.do("GET", "path", f.rw, q("path", "relative"), nil)
	if f.refs.attempts.Load() != 0 {
		t.Fatalf("control plane queried %d times for rejected requests", f.refs.attempts.Load())
	}
}

func TestNewHandlerRequiresVolumeRefs(t *testing.T) {
	cfg, err := volumecontent.NewConfig(testSecret, "", "https://vol.example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(Options{Tokens: cfg, Leaser: &dirLeaser{dir: t.TempDir()}}); err == nil {
		t.Fatal("a handler without a volume reference holder must not be constructible")
	}
}

func TestUnsupportedMethodIsNot404(t *testing.T) {
	f := newFixture(t, 0)
	resp, _ := f.do("DELETE", "file", f.rw, q("path", "/x"), nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d: unsupported methods must not look like a missing path", resp.StatusCode)
	}
}

func TestFileLifecycle(t *testing.T) {
	f := newFixture(t, 0)
	binary := []byte{0x00, 0xff, 'h', 'i', 0x00}

	resp, body := f.do("POST", "dir", f.rw, q("path", "/data", "mode", "493"), nil)
	f.expect(resp, 201, body)
	var dir entryStat
	if err := json.Unmarshal(body, &dir); err != nil || dir.Type != "directory" || dir.Path != "/data" || dir.Name != "data" {
		t.Fatalf("mkdir stat: %+v err=%v", dir, err)
	}

	resp, body = f.do("PUT", "file", f.rw, q("path", "/data/a.bin", "mode", "420"), binary)
	f.expect(resp, 201, body)
	var st entryStat
	if err := json.Unmarshal(body, &st); err != nil || st.Type != "file" || st.Size != int64(len(binary)) || st.Mode != 0o644 {
		t.Fatalf("put stat: %+v err=%v", st, err)
	}
	for _, ts := range []string{st.Atime, st.Mtime, st.Ctime} {
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			t.Fatalf("timestamp %q is not ISO-8601: %v", ts, err)
		}
	}

	resp, body = f.do("GET", "file", f.ro, q("path", "/data/a.bin"), nil)
	f.expect(resp, 200, body)
	if !bytes.Equal(body, binary) || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download mismatch: %v %q", body, resp.Header.Get("Content-Type"))
	}

	resp, body = f.do("GET", "dir", f.ro, q("path", "/"), nil)
	f.expect(resp, 200, body)
	var top []entryStat
	_ = json.Unmarshal(body, &top)
	if len(top) != 1 || top[0].Path != "/data" {
		t.Fatalf("depth 1 listing: %s", body)
	}
	resp, body = f.do("GET", "dir", f.ro, q("path", "/", "depth", "2"), nil)
	f.expect(resp, 200, body)
	var deep []entryStat
	_ = json.Unmarshal(body, &deep)
	if len(deep) != 2 || deep[1].Path != "/data/a.bin" {
		t.Fatalf("depth 2 listing: %s", body)
	}
	resp, body = f.do("GET", "dir", f.ro, q("path", "/data"), nil)
	f.expect(resp, 200, body)
	if strings.Contains(string(body), ".cube-upload-") {
		t.Fatalf("temporary upload leaked into listing: %s", body)
	}

	resp, body = f.do("PATCH", "path", f.rw, q("path", "/data/a.bin"), []byte(`{"mode":384}`))
	f.expect(resp, 200, body)
	_ = json.Unmarshal(body, &st)
	if st.Mode != 0o600 {
		t.Fatalf("patched mode %o", st.Mode)
	}

	resp, body = f.do("DELETE", "path", f.rw, q("path", "/data"), nil)
	f.expect(resp, 204, body)
	resp, body = f.do("GET", "path", f.ro, q("path", "/data/a.bin"), nil)
	f.expect(resp, 404, body)
	if f.leaser.acquired.Load() != f.leaser.released.Load() {
		t.Fatalf("leases leaked: acquired=%d released=%d", f.leaser.acquired.Load(), f.leaser.released.Load())
	}
}

func TestWriteSemantics(t *testing.T) {
	f := newFixture(t, 16)

	resp, body := f.do("PUT", "file", f.rw, q("path", "/missing/a"), []byte("x"))
	f.expect(resp, 404, body)

	resp, body = f.do("PUT", "file", f.rw, q("path", "/a"), []byte("one"))
	f.expect(resp, 201, body)
	resp, body = f.do("PUT", "file", f.rw, q("path", "/a", "force", "false"), []byte("two"))
	f.expect(resp, 409, body)
	resp, body = f.do("PUT", "file", f.rw, q("path", "/a"), []byte("three"))
	f.expect(resp, 201, body)
	if got, _ := os.ReadFile(filepath.Join(f.dir, "a")); string(got) != "three" {
		t.Fatalf("overwrite content %q", got)
	}

	resp, body = f.do("PUT", "file", f.rw, q("path", "/big"), bytes.Repeat([]byte("z"), 64))
	f.expect(resp, 413, body)
	if _, err := os.Stat(filepath.Join(f.dir, "big")); err == nil {
		t.Fatal("oversized upload must not be created")
	}
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cube-upload-") {
			t.Fatalf("temporary upload left behind: %s", e.Name())
		}
	}

	resp, body = f.do("POST", "dir", f.rw, q("path", "/x/y"), nil)
	f.expect(resp, 404, body)
	resp, body = f.do("POST", "dir", f.rw, q("path", "/x/y", "force", "true"), nil)
	f.expect(resp, 201, body)
	resp, body = f.do("POST", "dir", f.rw, q("path", "/x/y"), nil)
	f.expect(resp, 409, body)

	resp, body = f.do("PUT", "file", f.rw, q("path", "/a", "mode", "2048"), []byte("x"))
	f.expect(resp, 400, body) // setgid
	resp, body = f.do("PATCH", "path", f.rw, q("path", "/a"), []byte(`{"mode":2048}`))
	f.expect(resp, 400, body)
	resp, body = f.do("PATCH", "path", f.rw, q("path", "/a"), []byte(`{"bogus":1}`))
	f.expect(resp, 400, body)

	resp, body = f.do("DELETE", "path", f.rw, q("path", "/"), nil)
	f.expect(resp, 400, body)
	resp, body = f.do("DELETE", "path", f.rw, q("path", "/nope"), nil)
	f.expect(resp, 404, body)
	resp, body = f.do("GET", "file", f.ro, q("path", "/x"), nil)
	f.expect(resp, 400, body) // a directory is not a readable file
}

func TestPathValidation(t *testing.T) {
	f := newFixture(t, 0)
	for _, p := range []string{"", "relative", "/a/../b", "/a/./b", `/a\b`, "/a\x00b", "/.."} {
		resp, body := f.do("GET", "path", f.ro, q("path", p), nil)
		if resp.StatusCode != 400 {
			t.Errorf("path %q: status %d (%s), want 400", p, resp.StatusCode, body)
		}
	}
	resp, body := f.do("GET", "path", f.ro, nil, nil)
	f.expect(resp, 400, body)
}

// A volume writer can plant symlinks. None of them may reach outside the
// volume, whether read, written through, listed or removed.
func TestSymlinkEscapeIsContained(t *testing.T) {
	f := newFixture(t, 0)
	secret := filepath.Join(f.outer, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"abs-file": secret,
		"abs-dir":  f.outer,
		"rel-file": "../outside/secret.txt",
		"rel-dir":  "../outside",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(f.dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	for _, route := range [][2]string{
		{"GET", "file"}, {"GET", "dir"},
	} {
		for _, name := range []string{"abs-file", "abs-dir", "rel-file", "rel-dir"} {
			resp, body := f.do(route[0], route[1], f.rw, q("path", "/"+name), nil)
			if resp.StatusCode == 200 || strings.Contains(string(body), "top secret") {
				t.Errorf("%s %s via %s escaped the volume: %d %s", route[0], route[1], name, resp.StatusCode, body)
			}
		}
	}
	// Reading through a symlinked directory component.
	resp, body := f.do("GET", "file", f.rw, q("path", "/rel-dir/secret.txt"), nil)
	if resp.StatusCode == 200 {
		t.Fatalf("read through symlinked directory escaped: %s", body)
	}

	// Writing, creating and deleting through symlinks must not touch outside.
	f.do("PUT", "file", f.rw, q("path", "/abs-dir/new.txt"), []byte("pwn"))
	f.do("PUT", "file", f.rw, q("path", "/rel-dir/new.txt"), []byte("pwn"))
	f.do("POST", "dir", f.rw, q("path", "/rel-dir/sub"), nil)
	f.do("PATCH", "path", f.rw, q("path", "/rel-file"), []byte(`{"mode":511}`))
	f.do("DELETE", "path", f.rw, q("path", "/rel-dir/secret.txt"), nil)
	f.do("DELETE", "path", f.rw, q("path", "/abs-dir/secret.txt"), nil)

	if got, err := os.ReadFile(secret); err != nil || string(got) != "top secret" {
		t.Fatalf("outside file was modified or removed: %q %v", got, err)
	}
	if info, _ := os.Stat(secret); info.Mode().Perm() != 0o644 {
		t.Fatalf("outside file mode changed: %v", info.Mode())
	}
	left, _ := os.ReadDir(f.outer)
	if len(left) != 1 {
		t.Fatalf("outside directory was modified: %v", left)
	}

	// Stat reports the link itself; absolute targets are not disclosed.
	resp, body = f.do("GET", "path", f.ro, q("path", "/abs-file"), nil)
	f.expect(resp, 200, body)
	var st entryStat
	_ = json.Unmarshal(body, &st)
	if st.Type != "symlink" || st.Target != "" || strings.Contains(string(body), f.outer) {
		t.Fatalf("symlink stat leaks host path: %s", body)
	}
	resp, body = f.do("GET", "path", f.ro, q("path", "/rel-file"), nil)
	f.expect(resp, 200, body)
	_ = json.Unmarshal(body, &st)
	if st.Type != "symlink" || st.Target != "../outside/secret.txt" {
		t.Fatalf("relative symlink stat: %s", body)
	}
}

func TestFIFODoesNotBlock(t *testing.T) {
	f := newFixture(t, 0)
	if err := syscall.Mkfifo(filepath.Join(f.dir, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan int, 1)
	go func() {
		resp, _ := f.do("GET", "file", f.ro, q("path", "/pipe"), nil)
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != 400 {
			t.Fatalf("status %d, want 400", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked the request")
	}
}
