// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an in-memory path-style bucket. It checks that every request is
// signed and that the signed payload hash matches the body.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	// conditional is how PUT treats If-None-Match: "" honours it, "ignore"
	// overwrites anyway, "reject" answers 501.
	conditional string
	// afterPut, when set, runs with the lock held after a PUT is stored.
	afterPut func(key string)
	// mtimes records when each object was last written.
	mtimes map[string]time.Time
	// batchDelete is how POST ?delete is treated: "" deletes, "reject"
	// answers 501. deleteErrors names keys a batch reports it could not
	// delete, with the error code; throttlePosts answers that many batches
	// 429 first.
	batchDelete   string
	deleteErrors  map[string]string
	throttlePosts int
	// beforeDelete, when set, runs without the lock before a DELETE or a
	// batch delete is applied; a test blocks in it to interleave.
	beforeDelete func()
	// Requests served, by kind.
	batchDeletes, singleDeletes, lists int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/") {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/bkt")
	key := strings.TrimPrefix(path, "/")
	if r.Method == http.MethodDelete || r.Method == http.MethodPost {
		f.mu.Lock()
		hook := f.beforeDelete
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		f.serveBatchDelete(w, r)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("If-None-Match") != "" && f.conditional == "reject" {
			http.Error(w, "NotImplemented", http.StatusNotImplemented)
			return
		}
		if _, exists := f.objects[key]; exists && r.Header.Get("If-None-Match") == "*" && f.conditional != "ignore" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.objects[key] = body
		if f.mtimes == nil {
			f.mtimes = map[string]time.Time{}
		}
		f.mtimes[key] = time.Now()
		f.puts++
		if f.afterPut != nil {
			f.afterPut(key)
		}
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			f.lists++
			prefix := r.URL.Query().Get("prefix")
			var keys []string
			for k := range f.objects {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			type content struct {
				Key          string    `xml:"Key"`
				Size         int       `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			}
			res := struct {
				XMLName  xml.Name  `xml:"ListBucketResult"`
				Contents []content `xml:"Contents"`
			}{}
			for _, k := range keys {
				res.Contents = append(res.Contents, content{Key: k, Size: len(f.objects[k]), LastModified: f.mtimes[k]})
			}
			_ = xml.NewEncoder(w).Encode(res)
			return
		}
		data, ok := f.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
	case http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	case http.MethodDelete:
		f.singleDeletes++
		if code, ok := f.deleteErrors[key]; ok {
			http.Error(w, "<Error><Code>"+code+"</Code></Error>", http.StatusForbidden)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

// serveBatchDelete answers DeleteObjects; f.mu is held.
func (f *fakeS3) serveBatchDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["delete"]; !ok {
		http.Error(w, "unknown POST", http.StatusBadRequest)
		return
	}
	if f.batchDelete == "reject" {
		http.Error(w, "<Error><Code>NotImplemented</Code></Error>", http.StatusNotImplemented)
		return
	}
	if f.throttlePosts > 0 {
		f.throttlePosts--
		http.Error(w, "<Error><Code>SlowDown</Code></Error>", http.StatusTooManyRequests)
		return
	}
	body, _ := io.ReadAll(r.Body)
	sum := md5.Sum(body)
	if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
		http.Error(w, "<Error><Code>InvalidDigest</Code></Error>", http.StatusBadRequest)
		return
	}
	var req struct {
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, "<Error><Code>MalformedXML</Code></Error>", http.StatusBadRequest)
		return
	}
	f.batchDeletes++
	type keyError struct {
		Key     string `xml:"Key"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	res := struct {
		XMLName xml.Name   `xml:"DeleteResult"`
		Errors  []keyError `xml:"Error"`
	}{}
	for _, o := range req.Objects {
		if code, ok := f.deleteErrors[o.Key]; ok {
			res.Errors = append(res.Errors, keyError{Key: o.Key, Code: code, Message: "refused"})
			continue
		}
		delete(f.objects, o.Key)
	}
	_ = xml.NewEncoder(w).Encode(res)
}

func newTestClient(t *testing.T) (*Client, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "ak", SecretKey: "sk",
		PathStyle: true, Prefix: "ckpt"})
	if err != nil {
		t.Fatal(err)
	}
	return c, fake
}

func writeFile(t *testing.T, path string, size int64, writes map[int64][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, data := range writes {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestExportMaterializeRoundTrip(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	src := t.TempDir()
	chunk := minChunkSize
	repeated := randBytes(int(chunk))
	writeFile(t, filepath.Join(src, "rootfs"), 6*chunk+123, map[int64][]byte{
		0:           randBytes(4096),
		chunk - 100: randBytes(300), // straddles a chunk boundary
		3 * chunk:   repeated,
		6 * chunk:   randBytes(123),
	})
	writeFile(t, filepath.Join(src, "memory"), 4*chunk, map[int64][]byte{
		0:     repeated, // same content as a rootfs chunk: uploaded once
		chunk: make([]byte, chunk),
	})
	writeFile(t, filepath.Join(src, "meta", "snapshot", "config.json"), 17, map[int64][]byte{0: []byte(`{"vm":"config!!"}`)})
	writeFile(t, filepath.Join(src, "meta", "empty.json"), 0, nil)

	files := []SourceFile{
		{Role: RoleVolume, Name: "tpl-snap-x-rootfs", Path: filepath.Join(src, "rootfs")},
		{Role: RoleVolume, Name: "tpl-snap-x-memory", Path: filepath.Join(src, "memory")},
		{Role: RoleMeta, Name: "snapshot/config.json", Path: filepath.Join(src, "meta", "snapshot", "config.json"), Mode: 0o644},
		{Role: RoleMeta, Name: "empty.json", Path: filepath.Join(src, "meta", "empty.json"), Mode: 0o644},
	}
	cand, m, err := Export(ctx, c, "snap-x", "sess1", files, ExportOptions{ChunkSize: chunk, Workers: 3,
		Attributes: map[string]string{"spec_dir": "2C4096M"}})
	if err != nil {
		t.Fatal(err)
	}
	if cand.Session != "sess1" || m.Attributes["spec_dir"] != "2C4096M" {
		t.Fatalf("unexpected candidate %+v", cand)
	}
	if _, ok := fake.objects[CandidateKey("ckpt", "snap-x")]; !ok {
		t.Fatal("candidate was not committed")
	}
	for _, f := range m.Files {
		if f.Role == RoleVolume && f.Name == "tpl-snap-x-memory" && len(f.Chunks) != 1 {
			t.Fatalf("all-zero memory chunks must not be stored: %+v", f.Chunks)
		}
	}

	_, loaded, err := Load(ctx, c, "snap-x", cand.ManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := Readback(ctx, c, loaded, 2, nil); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	err = Materialize(ctx, c, loaded, 2, nil, func(f *File) (string, error) {
		p := filepath.Join(dst, f.Role, filepath.FromSlash(f.Name))
		return p, os.MkdirAll(filepath.Dir(p), 0o755)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		want, _ := os.ReadFile(f.Path)
		got, err := os.ReadFile(filepath.Join(dst, f.Role, filepath.FromSlash(f.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("%s differs after round trip", f.Name)
		}
	}
}

func TestLoadRejectsUnverifiedSessionAndCorruption(t *testing.T) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "rootfs"), 2*minChunkSize, map[int64][]byte{0: randBytes(1000)})
	files := []SourceFile{{Role: RoleVolume, Name: "r", Path: filepath.Join(src, "rootfs")}}
	cand, m, err := Export(ctx, c, "snap-y", "s1", files, ExportOptions{ChunkSize: minChunkSize})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(ctx, c, "snap-y", strings.Repeat("0", 64)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a different verified digest must be refused, got %v", err)
	}
	if _, _, err := Load(ctx, c, "snap-none", ""); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("missing candidate must be ErrNoCandidate, got %v", err)
	}
	key := m.Files[0].Chunks[0].Key
	fake.objects[key][10] ^= 0xff
	if err := Readback(ctx, c, m, 1, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered chunk must be corrupt, got %v", err)
	}
	delete(fake.objects, key)
	if err := Readback(ctx, c, m, 1, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing chunk must be corrupt, got %v", err)
	}
	fake.objects[cand.ManifestKey] = append([]byte(nil), fake.objects[cand.ManifestKey]...)
	fake.objects[cand.ManifestKey][0] = ' '
	if _, err := LoadManifest(ctx, c, cand); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("manifest digest mismatch must be corrupt, got %v", err)
	}
}

func TestManifestValidateRejectsUnsafeInput(t *testing.T) {
	sum := strings.Repeat("a", 64)
	key := ChunkKey("p", "snap-z", "s", sum)
	base := func() *Manifest {
		return &Manifest{SchemaVersion: SchemaVersion, SnapshotID: "snap-z", Session: "s", ChunkSize: minChunkSize,
			Files: []File{{Role: RoleMeta, Name: "a/b.json", Size: 100, Chunks: []Chunk{{Offset: 0, Length: 100, SHA256: sum, Key: key}}}}}
	}
	if err := base().Validate("p", "snap-z"); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	cases := map[string]func(m *Manifest){
		"traversal":     func(m *Manifest) { m.Files[0].Name = "../etc/passwd" },
		"absolute":      func(m *Manifest) { m.Files[0].Name = "/etc/passwd" },
		"unclean":       func(m *Manifest) { m.Files[0].Name = "a//b" },
		"past end":      func(m *Manifest) { m.Files[0].Chunks[0].Length = 101 },
		"foreign key":   func(m *Manifest) { m.Files[0].Chunks[0].Key = "p/other/" + sum },
		"bad digest":    func(m *Manifest) { m.Files[0].Chunks[0].SHA256 = "xyz" },
		"other snap":    func(m *Manifest) { m.SnapshotID = "snap-q" },
		"unknown role":  func(m *Manifest) { m.Files[0].Role = "exec" },
		"bad volume":    func(m *Manifest) { m.Files[0].Role = RoleVolume; m.Files[0].Name = "a/b" },
		"tiny chunks":   func(m *Manifest) { m.ChunkSize = 4096 },
		"overlap":       func(m *Manifest) { m.Files[0].Chunks = append(m.Files[0].Chunks, m.Files[0].Chunks[0]) },
		"negative size": func(m *Manifest) { m.Files[0].Size = -1 },
		"duplicate file": func(m *Manifest) {
			m.Files = append(m.Files, m.Files[0])
		},
	}
	for name, mutate := range cases {
		m := base()
		mutate(m)
		if err := m.Validate("p", "snap-z"); err == nil {
			t.Errorf("%s: manifest accepted", name)
		}
	}
	raw, _ := json.Marshal(base())
	if _, err := ParseManifest(raw, &Candidate{Session: "s", SnapshotID: "snap-z", ManifestSize: 1}, "p"); err == nil {
		t.Error("manifest with a size other than the candidate's accepted")
	}
}

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s3.cfg")
	_ = os.WriteFile(p, []byte("# c\naccess_key_id=ak\nsecret_access_key=sk\nendpoint=10.0.0.1:9000\n"+
		"region=r1\nbuckets=b1,b2\npath_style=true\nno_tls=true\n"), 0o600)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bucket != "b1" || !cfg.NoTLS || cfg.Prefix != DefaultPrefix || cfg.Region != "r1" ||
		cfg.ConditionalWrites != ConditionalRequired {
		t.Fatalf("unexpected config %+v", cfg)
	}
	_ = os.WriteFile(p, []byte("access_key_id=ak\nsecret_access_key=sk\nendpoint=e\nbucket=b\nconditional_writes=Optional\n"), 0o600)
	if cfg, err := LoadConfig(p); err != nil || cfg.ConditionalWrites != ConditionalOptional {
		t.Fatalf("conditional_writes=optional: %+v %v", cfg, err)
	}
	_ = os.WriteFile(p, []byte("access_key_id=ak\nsecret_access_key=sk\nendpoint=e\nbucket=b\nconditional_writes=maybe\n"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("an unknown conditional_writes value must be refused")
	}
	_ = os.WriteFile(p, []byte("access_key_id=ak\nsecret_access_key=sk\nendpoint=e\nbucket=b\nimport_verify=FIEMAP\n"), 0o600)
	if cfg, err := LoadConfig(p); err != nil || cfg.ImportVerify != ImportVerifyFIEMAP {
		t.Fatalf("import_verify=fiemap: %+v %v", cfg, err)
	}
	_ = os.WriteFile(p, []byte("access_key_id=ak\nsecret_access_key=sk\nendpoint=e\nbucket=b\nimport_verify=never\n"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("an unknown import_verify value must be refused")
	}
	for raw, want := range map[string]string{`["cube-s3lvol"]`: "cube-s3lvol", `["a", "b"]`: "a", "x": "x", `"q"`: "q"} {
		if got := firstBucket(raw); got != want {
			t.Errorf("firstBucket(%s)=%q, want %q", raw, got, want)
		}
	}
	c, err := NewClient(*cfg)
	if err != nil || c.base.Scheme != "http" {
		t.Fatalf("no_tls endpoint must use http: %v %v", c, err)
	}
}

func TestDeletePrefixRefusesRoot(t *testing.T) {
	c, fake := newTestClient(t)
	fake.objects["ckpt/snap-a/candidate.json"] = []byte("x")
	fake.objects["ckpt/snap-b/candidate.json"] = []byte("x")
	if _, err := c.DeletePrefix(context.Background(), "ckpt/", SweepOptions{}); err == nil {
		t.Fatal("deleting the whole checkpoint prefix must be refused")
	}
	n, err := c.DeletePrefix(context.Background(), SnapshotPrefix("ckpt", "snap-a"), SweepOptions{})
	if err != nil || n != 1 || len(fake.objects) != 1 {
		t.Fatalf("delete snap-a: n=%d err=%v left=%v", n, err, fake.objects)
	}
}

func TestCommittedSessionIsNeverRewritten(t *testing.T) {
	t.Run("store honours If-None-Match", func(t *testing.T) { committedSessionIsNeverRewritten(t, "") })
	// The degraded commit must keep the same guarantees against every
	// sequential misuse: some S3-compatible stores overwrite silently.
	t.Run("store ignores If-None-Match", func(t *testing.T) { committedSessionIsNeverRewritten(t, "ignore") })
}

func committedSessionIsNeverRewritten(t *testing.T, conditional string) {
	ctx := context.Background()
	c, fake := newTestClient(t)
	fake.conditional = conditional
	c.UseConditionalWrites(conditional == "")
	src := t.TempDir()
	path := filepath.Join(src, "rootfs")
	writeFile(t, path, 2*minChunkSize, map[int64][]byte{0: randBytes(4096)})
	files := []SourceFile{{Role: RoleVolume, Name: "r", Path: path}}
	started := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	opt := ExportOptions{ChunkSize: minChunkSize, Now: func() time.Time { return started }}

	cand, _, err := Export(ctx, c, "snap-c", "s1", files, opt)
	if err != nil {
		t.Fatal(err)
	}
	committed := append([]byte(nil), fake.objects[CandidateKey("ckpt", "snap-c")]...)

	// A restarted job re-exports the same session from the same start time:
	// every commit object comes out byte-identical and the export succeeds.
	again, _, err := Export(ctx, c, "snap-c", "s1", files, opt)
	if err != nil {
		t.Fatalf("idempotent re-export failed: %v", err)
	}
	if again.ManifestSHA256 != cand.ManifestSHA256 {
		t.Fatalf("re-export changed the manifest digest %s -> %s", cand.ManifestSHA256, again.ManifestSHA256)
	}

	// Different content for the same session must not replace what readers
	// may already have verified.
	writeFile(t, path, 2*minChunkSize, map[int64][]byte{0: randBytes(4096)})
	if _, _, err := Export(ctx, c, "snap-c", "s1", files, opt); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting re-export: want ErrConflict, got %v", err)
	}
	// Neither may another session replace the committed candidate.
	if _, _, err := Export(ctx, c, "snap-c", "s2", files, opt); !errors.Is(err, ErrConflict) {
		t.Fatalf("second session: want ErrConflict, got %v", err)
	}
	if !bytes.Equal(fake.objects[CandidateKey("ckpt", "snap-c")], committed) {
		t.Fatal("the committed candidate was rewritten")
	}
	if _, _, err := Load(ctx, c, "snap-c", cand.ManifestSHA256); err != nil {
		t.Fatalf("the verified session no longer loads: %v", err)
	}
}

func TestProbeConditionalWrite(t *testing.T) {
	ctx := context.Background()
	for mode, want := range map[string]error{
		"":       nil,
		"ignore": ErrConditionalWriteUnsupported,
		"reject": ErrConditionalWriteUnsupported,
	} {
		c, fake := newTestClient(t)
		fake.conditional = mode
		err := c.ProbeConditionalWrite(ctx)
		if want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Fatalf("mode %q: got %v, want %v", mode, err, want)
		}
		fake.mu.Lock()
		left := len(fake.objects)
		fake.mu.Unlock()
		if left != 0 {
			t.Fatalf("mode %q: probe left %d objects behind", mode, left)
		}
	}
}

func TestProbeConditionalWriteClassifiesHeaderRejections(t *testing.T) {
	for _, code := range []int{400, 403, 405, 501, 401, 404, 500} {
		for _, rejectAt := range []int{1, 2} {
			t.Run(fmt.Sprintf("http-%d-put-%d", code, rejectAt), func(t *testing.T) {
				puts := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPut {
						puts++
						if puts >= rejectAt {
							w.WriteHeader(code)
						}
					}
				}))
				defer srv.Close()
				c, err := NewClient(Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "ak", SecretKey: "sk", Prefix: "ckpt", PathStyle: true})
				if err != nil {
					t.Fatal(err)
				}
				err = c.ProbeConditionalWrite(context.Background())
				wantUnsupported := code == 400 || code == 403 || code == 405 || code == 501
				if err == nil || errors.Is(err, ErrConditionalWriteUnsupported) != wantUnsupported {
					t.Fatalf("got %v, want unsupported=%v", err, wantUnsupported)
				}
			})
		}
	}
}

func TestProbeConditionalWriteOnAnUnreachableStore(t *testing.T) {
	c, err := NewClient(Config{Endpoint: "http://127.0.0.1:9", Bucket: "bkt", AccessKey: "ak", SecretKey: "sk", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.ProbeConditionalWrite(ctx); err == nil || errors.Is(err, ErrConditionalWriteUnsupported) {
		t.Fatalf("an unreachable store says nothing about conditional writes: %v", err)
	}
}

func TestUnconditionalCommitDetectsAnotherWriter(t *testing.T) {
	// Another writer lands between our PUT and the read-back: the commit
	// must not report success over bytes it did not write.
	ctx := context.Background()
	c, fake := newTestClient(t)
	fake.conditional = "ignore"
	c.UseConditionalWrites(false)
	cand := CandidateKey("ckpt", "snap-w")
	fake.afterPut = func(key string) {
		if key == cand {
			fake.objects[key] = []byte(`{"session":"someone-else"}`)
		}
	}
	src := t.TempDir()
	path := filepath.Join(src, "rootfs")
	writeFile(t, path, minChunkSize, map[int64][]byte{0: randBytes(4096)})
	_, _, err := Export(ctx, c, "snap-w", "s1", []SourceFile{{Role: RoleVolume, Name: "r", Path: path}},
		ExportOptions{ChunkSize: minChunkSize})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestUnconditionalCommitChecksBeforeWriting(t *testing.T) {
	// The degraded path looks before it writes: a commit object that already
	// holds other bytes is never overwritten.
	ctx := context.Background()
	c, fake := newTestClient(t)
	fake.conditional = "ignore"
	c.UseConditionalWrites(false)
	key := "ckpt/snap-h/candidate.json"
	fake.objects[key] = []byte("committed earlier")
	puts := fake.puts
	if err := putImmutable(ctx, c, key, []byte("different")); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if fake.puts != puts || string(fake.objects[key]) != "committed earlier" {
		t.Fatal("an existing commit object was overwritten")
	}
	if err := putImmutable(ctx, c, key, []byte("committed earlier")); err != nil {
		t.Fatalf("identical bytes are the same commit: %v", err)
	}
}

func TestDeletePrefixRejectsOutsideClientNamespace(t *testing.T) {
	c, fake := newTestClient(t)
	fake.objects["other/snap/candidate.json"] = []byte("keep")
	fake.objects["ckpt-other/snap/candidate.json"] = []byte("keep")
	for _, prefix := range []string{"", "/", "ckpt", "/ckpt/", "other/", "ckpt-other/"} {
		if n, err := c.DeletePrefix(context.Background(), prefix, SweepOptions{}); err == nil || n != 0 {
			t.Errorf("prefix %q: deleted=%d err=%v", prefix, n, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.lists != 0 || fake.batchDeletes != 0 || fake.singleDeletes != 0 || len(fake.objects) != 2 {
		t.Fatalf("unsafe prefix reached the store: %+v", fake)
	}
}

func TestManifestValidateVolumeKinds(t *testing.T) {
	for _, schema := range []int{SchemaVersion, SchemaVersionIncremental} {
		for _, kind := range []string{"", KindRootfs, KindMemory, "../outside", "../../rootfs", "/rootfs", "a/b", "unknown"} {
			t.Run(fmt.Sprintf("v%d/%s", schema, kind), func(t *testing.T) {
				m := &Manifest{SchemaVersion: schema, SnapshotID: "snap-kind", Session: "s", SandboxID: "sb-kind", ChunkSize: minChunkSize,
					Files: []File{{Role: RoleVolume, Name: "disk", Kind: kind}}}
				wantOK := kind == "" || kind == KindRootfs || kind == KindMemory
				if err := m.Validate("ckpt", m.SnapshotID); (err == nil) != wantOK {
					t.Fatalf("kind %q: %v, want accepted=%v", kind, err, wantOK)
				}
			})
		}
	}
}
