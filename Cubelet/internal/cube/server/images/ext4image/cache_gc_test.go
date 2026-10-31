// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package ext4image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
)

func cacheFixture(t *testing.T, id string, body []byte, origin string, age time.Duration) string {
	t.Helper()
	path := pmem.GetRawImageFilePath("cubebox", id)
	writeTestFile(t, path, body)
	sha := sha256.Sum256(body)
	entry := &cacheEntry{CacheSource: CacheSource{ArtifactID: id, InstanceType: "cubebox", URL: origin + "/cube/template/artifact/download?artifact_id=" + id,
		Token: "test-only", SHA256: hex.EncodeToString(sha[:]), SizeBytes: int64(len(body)), RemoteBacked: true}, LastUsed: time.Now().Add(-age)}
	if err := writeCacheEntry(path, entry); err != nil {
		t.Fatal(err)
	}
	return path
}

func testCacheGC(t *testing.T, c CacheGCConfig, inUse ArtifactInUseFunc) *CacheGC {
	t.Helper()
	c.Enabled = true
	if c.DryRun == nil {
		realEviction := false
		c.DryRun = &realEviction
	}
	if inUse == nil {
		inUse = func(string) (bool, error) { return false, nil }
	}
	g, err := NewCacheGC(c, inUse, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	g.space = func() (cacheDiskSpace, error) { return cacheDiskSpace{100000, 10000}, nil }
	return g
}

func cacheServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "test-only" {
			w.WriteHeader(403)
			return
		}
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
			w.WriteHeader(206)
			w.Write(body[:1])
			return
		}
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCacheGCEvictsLRUAndRehydrates(t *testing.T) {
	initTestPmemPaths(t, t.TempDir())
	body := bytes.Repeat([]byte("x"), 2048)
	server := cacheServer(t, body)
	old := cacheFixture(t, "old", body, server.URL, 3*time.Hour)
	newer := cacheFixture(t, "newer", body, server.URL, 2*time.Hour)
	companion := filepath.Join(filepath.Dir(old), "old.vm")
	writeTestFile(t, companion, []byte("kernel"))
	writeTestFile(t, filepath.Join(filepath.Dir(old), "version"), []byte("version"))
	g := testCacheGC(t, CacheGCConfig{}, nil)
	g.space = func() (cacheDiskSpace, error) {
		if _, err := os.Stat(old); os.IsNotExist(err) {
			return cacheDiskSpace{10000, 4000}, nil
		}
		return cacheDiskSpace{10000, 1000}, nil
	}
	result, err := g.collect(context.Background(), 0)
	if err != nil || result.Files != 1 || result.FreedBytes != 3000 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, path := range []string{newer, companion, filepath.Join(filepath.Dir(old), "version"), filepath.Join(filepath.Dir(old), cacheMetadataName)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	// No current spec and no old signed URL: the retained stable source suffices.
	if err := EnsurePmemRootfs(context.Background(), "cubebox", "old"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(old)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("rehydration failed: %v", err)
	}
	entry, err := readCacheEntry(old)
	if err != nil || time.Since(entry.LastUsed) > time.Minute {
		t.Fatal("last use was not refreshed")
	}
}

func TestCacheGCProtection(t *testing.T) {
	for _, mode := range []string{"runtime", "usage-error", "pin", "young", "dry-run", "recovery", "remote", "limit", "metadata", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			initTestPmemPaths(t, t.TempDir())
			body := bytes.Repeat([]byte("x"), 2048)
			server := cacheServer(t, body)
			path := cacheFixture(t, "protected", body, server.URL, 2*time.Hour)
			g := testCacheGC(t, CacheGCConfig{}, nil)
			switch mode {
			case "runtime":
				g.inUse = func(string) (bool, error) { return true, nil }
			case "usage-error":
				g.inUse = func(string) (bool, error) { return false, errors.New("index unavailable") }
			case "pin":
				release, _ := pmem.PinArtifact(context.Background(), "cubebox", "protected")
				defer release()
			case "young":
				cacheFixture(t, "protected", body, server.URL, 0)
			case "dry-run":
				dryRun := true
				g.config.DryRun = &dryRun
			case "recovery":
				g.ready = func() bool { return false }
			case "remote":
				server.Close()
			case "limit":
				g.config.MaxBytes = 1024
			case "metadata":
				os.Remove(filepath.Join(filepath.Dir(path), cacheMetadataName))
			case "symlink":
				target := filepath.Join(t.TempDir(), "external.ext4")
				writeTestFile(t, target, body)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			result, err := g.collect(context.Background(), 0)
			if mode != "recovery" && err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("protected file removed", err)
			}
			if mode == "dry-run" && (result.Files != 1 || result.FreedBytes != 0) {
				t.Fatalf("dry run result=%+v", result)
			}
		})
	}
}

func TestCacheDownloadReservations(t *testing.T) {
	initTestPmemPaths(t, t.TempDir())
	if err := os.MkdirAll(pmem.GetPmemBasePath("cubebox"), 0755); err != nil {
		t.Fatal(err)
	}
	g := testCacheGC(t, CacheGCConfig{}, nil)
	g.space = func() (cacheDiskSpace, error) { return cacheDiskSpace{10000, 4000}, nil }
	var succeeded atomic.Int32
	var releases [2]func()
	done := make(chan struct{}, 2)
	for i := range releases {
		go func() {
			release, err := g.reserve(context.Background(), 1500)
			if err == nil {
				releases[i] = release
				succeeded.Add(1)
			}
			done <- struct{}{}
		}()
	}
	<-done
	<-done
	if succeeded.Load() != 1 {
		t.Fatalf("overcommitted download capacity: %d", succeeded.Load())
	}
	for _, release := range releases {
		if release != nil {
			release()
			release()
		}
	}
	if g.reserved != 0 {
		t.Fatal("reservation leaked")
	}
	if _, err := g.reserve(context.Background(), -1); err == nil {
		t.Fatal("unbounded download accepted")
	}
}

func TestCacheManifestImportIsConservative(t *testing.T) {
	initTestPmemPaths(t, t.TempDir())
	body := bytes.Repeat([]byte("x"), 2048)
	server := cacheServer(t, body)
	path := cacheFixture(t, "adopt", body, server.URL, 3*time.Hour)
	entry, _ := readCacheEntry(path)
	manifest := filepath.Join(t.TempDir(), "sources.json")
	input := struct {
		Version   int           `json:"version"`
		Artifacts []CacheSource `json:"artifacts"`
	}{1, []CacheSource{entry.CacheSource}}
	data, _ := json.Marshal(input)
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "projected-sources.json")
	if err := os.Symlink(manifest, link); err != nil {
		t.Fatal(err)
	}
	manifest = link
	if err := ImportCacheSources(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	got, _ := readCacheEntry(path)
	if !got.LastUsed.Equal(entry.LastUsed) {
		t.Fatal("import reset LRU age")
	}
	if err := os.Remove(filepath.Join(filepath.Dir(path), cacheMetadataName)); err != nil {
		t.Fatal(err)
	}
	if err := ImportCacheSources(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	got, _ = readCacheEntry(path)
	if time.Since(got.LastUsed) > time.Minute {
		t.Fatal("legacy adoption must start idle grace now")
	}
	info, _ := os.Stat(filepath.Join(filepath.Dir(path), cacheMetadataName))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("metadata permissions %o", info.Mode().Perm())
	}
	invalid := entry.CacheSource
	invalid.URL = server.URL + "/expired?X-Amz-Signature=secret"
	if err := invalid.validate(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("signed URL accepted or leaked")
	}
	invalid = entry.CacheSource
	invalid.RemoteBacked = false
	if err := invalid.validate(); err == nil {
		t.Fatal("local-only source accepted")
	}
}

func TestCacheRehydrateRejectsCorruptionAndCleansTemporaryFile(t *testing.T) {
	initTestPmemPaths(t, t.TempDir())
	body := bytes.Repeat([]byte("x"), 2048)
	server := cacheServer(t, bytes.Repeat([]byte("y"), 2048))
	path := cacheFixture(t, "corrupt", body, server.URL, 2*time.Hour)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePmemRootfs(context.Background(), "cubebox", "corrupt"); err == nil {
		t.Fatal("corrupt download accepted")
	}
	for _, file := range []string{path, path + ".download"} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("failed download left file: %s", file)
		}
	}
}

func TestCacheGCDefaultsToDryRun(t *testing.T) {
	g, err := NewCacheGC(CacheGCConfig{Enabled: true}, func(string) (bool, error) { return false, nil }, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !g.config.dryRun() {
		t.Fatal("omitted dry_run enabled destructive eviction")
	}
}

func TestCacheGCProbeDoesNotBlockCreatePin(t *testing.T) {
	initTestPmemPaths(t, t.TempDir())
	probing := make(chan struct{})
	resume := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probing)
		<-resume
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	defer server.Close()
	path := cacheFixture(t, "probe-pin", []byte("x"), server.URL, 2*time.Hour)
	g := testCacheGC(t, CacheGCConfig{}, nil)
	finished := make(chan error, 1)
	go func() { _, err := g.collect(context.Background(), 0); finished <- err }()
	<-probing
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := pmem.PinArtifact(ctx, "cubebox", "probe-pin")
	close(resume)
	if err != nil {
		<-finished
		t.Fatalf("network probe held the exclusive artifact lease: %v", err)
	}
	defer release()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("creator's pinned artifact was evicted: %v", err)
	}
}

func TestImportCacheSourcesRejectsPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest-pipe")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ImportCacheSources(context.Background(), path); err == nil {
		t.Fatal("accepted a non-regular manifest")
	}
}

func TestDisabledCacheGCDoesNotRecordMetadata(t *testing.T) {
	previous := activeCacheGC.Swap(nil)
	defer activeCacheGC.Store(previous)
	// Even an invalid path must not be inspected when the feature is disabled.
	if err := recordCacheUse(context.Background(), "cubebox", "../disabled"); err != nil {
		t.Fatal(err)
	}
	dryRun := true
	g := testCacheGC(t, CacheGCConfig{DryRun: &dryRun}, nil)
	g.Activate()
	if activeCacheGC.Load() != g {
		t.Fatal("dry-run did not enable metadata tracking")
	}
	release, err := reserveCacheDownload(context.Background(), 1<<60)
	if err != nil {
		t.Fatalf("dry-run enforced capacity: %v", err)
	}
	release()
}
