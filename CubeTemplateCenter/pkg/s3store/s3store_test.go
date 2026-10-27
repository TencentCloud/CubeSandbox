// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package s3store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

// fakeStore is a minimal blobstore.Store that records the keys touched by Put /
// Stat / Delete / SignedGetURL and answers from an in-memory map. It lets the
// s3store key-derivation tests run without a real fs/s3 backend (and so without
// the Linux-only fs driver), asserting exactly which keys the client computes.
type fakeStore struct {
	putKeys    []string // keys handed to Put, in order
	signedKeys []string // keys handed to SignedGetURL, in order
	statKeys   []string // keys handed to Stat, in order
	delKeys    []string // keys handed to Delete, in order
	objects    map[string][]byte
	sign       bool // when true, SignedGetURL returns a URL embedding the key
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}}
}

func (s *fakeStore) Capabilities() blobstore.Capabilities {
	return blobstore.Capabilities{DirectURL: s.sign, Shared: true, RangeRead: true}
}

func (s *fakeStore) Put(_ context.Context, key string, r io.Reader, _ blobstore.PutOptions) (blobstore.ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return blobstore.ObjectInfo{}, err
	}
	s.putKeys = append(s.putKeys, key)
	s.objects[key] = data
	return blobstore.ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (s *fakeStore) Get(_ context.Context, key string, _ blobstore.GetOptions) (*blobstore.Object, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, blobstore.ErrNotExist
	}
	return &blobstore.Object{ObjectInfo: blobstore.ObjectInfo{Key: key, Size: int64(len(data))}, Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (s *fakeStore) Stat(_ context.Context, key string) (blobstore.ObjectInfo, error) {
	s.statKeys = append(s.statKeys, key)
	data, ok := s.objects[key]
	if !ok {
		return blobstore.ObjectInfo{}, blobstore.ErrNotExist
	}
	return blobstore.ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (s *fakeStore) Delete(_ context.Context, key string) error {
	s.delKeys = append(s.delKeys, key)
	delete(s.objects, key)
	return nil
}

func (s *fakeStore) List(_ context.Context, _ string, _ func(blobstore.ObjectInfo) error) error {
	return nil
}

// SignedGetURL mirrors the S3 driver's behavior, NOT the fs driver's: it signs
// the key UNCONDITIONALLY without checking the object exists (S3's
// PresignedGetObject is a pure local signature computation; the fs driver stats
// first). This is deliberate -- a fake that looked up objects[key] (like the fs
// driver) would hide the S3 presign bug, where a probe loop returned the first
// candidate URL for a key that was never written. Tests that need existence
// checks use Stat (which the real client calls before presigning); the fake's
// SignedGetURL just records the key it was asked to sign and returns a URL.
func (s *fakeStore) SignedGetURL(_ context.Context, key string, _ time.Duration) (string, error) {
	s.signedKeys = append(s.signedKeys, key)
	if !s.sign {
		return "", blobstore.ErrUnsupported
	}
	return "https://example.com/bucket/" + key, nil
}

func (s *fakeStore) Prepare(_ context.Context) error { return nil }
func (s *fakeStore) GC(_ context.Context, _ blobstore.GCOptions) (blobstore.GCResult, error) {
	return blobstore.GCResult{}, nil
}
func (s *fakeStore) Close() error { return nil }

func newTestClient(prefix string, store *fakeStore) *Client {
	return &Client{
		cfg:   Config{ArtifactPrefix: prefix, PresignExpiry: time.Hour},
		store: store,
	}
}

// mustWriteArtifact writes a temp file with the given basename + content and
// returns its path, standing in for the local artifact file Upload reads from.
func mustWriteArtifact(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestArtifactKeySuffix verifies the on-disk suffix is taken from the local
// file's name so an erofs upload is keyed <id>.erofs (matching BuildErofs),
// while an unknown/missing suffix falls back to the legacy .ext4.
func TestArtifactKeySuffix(t *testing.T) {
	cases := []struct {
		localPath string
		want      string
	}{
		{"/data/x/rfs-1.erofs", ".erofs"},
		{"/data/x/rfs-1.ext4", ".ext4"},
		{"rfs-1.EROFS", ".erofs"}, // lowercased
		{"rfs-1", ".ext4"},        // no suffix -> legacy
		{"rfs-1.tar", ".ext4"},    // unknown -> legacy
		{"", ".ext4"},             // empty -> legacy
	}
	for _, c := range cases {
		if got := artifactKeySuffix(c.localPath); got != c.want {
			t.Fatalf("artifactKeySuffix(%q) = %q, want %q", c.localPath, got, c.want)
		}
	}
}

// TestUploadErofsKeyIsErofs is the core regression: an erofs artifact must be
// PUT under <id>.erofs (not the old hard-coded <id>.ext4), and the returned
// full key (persisted as object_key) must carry the .erofs suffix too, so the
// key CubeMaster stores matches the key the bytes live under.
func TestUploadErofsKeyIsErofs(t *testing.T) {
	store := newFakeStore()
	c := newTestClient("template-artifacts", store)

	erofsPath := mustWriteArtifact(t, "rfs-erofs.erofs", []byte("erofs-bytes"))
	fullKey, err := c.Upload(context.Background(), "rfs-erofs", erofsPath, "")
	if err != nil {
		t.Fatalf("Upload erofs: %v", err)
	}
	if len(store.putKeys) != 1 || store.putKeys[0] != "rfs-erofs.erofs" {
		t.Fatalf("erofs PUT key = %v, want [rfs-erofs.erofs] (suffix from local file, not .ext4)", store.putKeys)
	}
	// FullObjectKey (returned, persisted as object_key) includes the prefix and
	// the .erofs suffix -- it MUST match the key Upload wrote under.
	wantFull := "template-artifacts/rfs-erofs.erofs"
	if fullKey != wantFull {
		t.Fatalf("Upload returned full key %q, want %q", fullKey, wantFull)
	}
}

// TestUploadExt4KeyUnchanged verifies the legacy ext4 path still keys <id>.ext4
// (backward compatible).
func TestUploadExt4KeyUnchanged(t *testing.T) {
	store := newFakeStore()
	c := newTestClient("template-artifacts", store)

	ext4Path := mustWriteArtifact(t, "rfs-ext4.ext4", []byte("ext4-bytes"))
	fullKey, err := c.Upload(context.Background(), "rfs-ext4", ext4Path, "")
	if err != nil {
		t.Fatalf("Upload ext4: %v", err)
	}
	if len(store.putKeys) != 1 || store.putKeys[0] != "rfs-ext4.ext4" {
		t.Fatalf("ext4 PUT key = %v, want [rfs-ext4.ext4]", store.putKeys)
	}
	if fullKey != "template-artifacts/rfs-ext4.ext4" {
		t.Fatalf("ext4 full key = %q, want template-artifacts/rfs-ext4.ext4", fullKey)
	}
}

// TestStatUsesFormatKey verifies Stat derives the single object key from the
// rootfs format (the exact key Upload wrote under), not by probing two
// candidate keys by artifactID. ext4 -> <id>.ext4, erofs -> <id>.erofs.
func TestStatUsesFormatKey(t *testing.T) {
	store := newFakeStore()
	c := newTestClient("", store)

	// Nothing uploaded for an ext4 build: Stat checks <id>.ext4 only, false.
	store.statKeys = nil
	got, err := c.Stat(context.Background(), "rfs-1", "ext4")
	if err != nil {
		t.Fatalf("Stat missing ext4: %v", err)
	}
	if got {
		t.Fatal("Stat should report false when no object exists")
	}
	if len(store.statKeys) != 1 || store.statKeys[0] != "rfs-1.ext4" {
		t.Fatalf("Stat ext4 probed = %v, want [rfs-1.ext4] (single key from format, no .erofs probe)", store.statKeys)
	}

	// erofs build: Stat checks <id>.erofs only.
	store.statKeys = nil
	got, err = c.Stat(context.Background(), "rfs-1", "erofs")
	if err != nil {
		t.Fatalf("Stat missing erofs: %v", err)
	}
	if got {
		t.Fatal("Stat erofs should report false when no object exists")
	}
	if len(store.statKeys) != 1 || store.statKeys[0] != "rfs-1.erofs" {
		t.Fatalf("Stat erofs probed = %v, want [rfs-1.erofs]", store.statKeys)
	}

	// An empty/legacy format (old rows whose RootfsFormat predates the column)
	// maps to <id>.ext4 -- the historical key -- so legacy artifacts still Stat.
	store.statKeys = nil
	store.objects["rfs-legacy.ext4"] = []byte("legacy")
	got, err = c.Stat(context.Background(), "rfs-legacy", "")
	if err != nil || !got {
		t.Fatalf("Stat legacy empty-format: got=%v err=%v, want true (empty format -> .ext4 key)", got, err)
	}
	if len(store.statKeys) != 1 || store.statKeys[0] != "rfs-legacy.ext4" {
		t.Fatalf("Stat legacy probed = %v, want [rfs-legacy.ext4]", store.statKeys)
	}
}

// TestStatFindsUploadedObject verifies Stat finds an object uploaded under the
// matching format key: upload an ext4 artifact, Stat with "ext4" finds it;
// Stat with "erofs" (wrong format) does NOT, proving Stat does not fall back to
// probing the other suffix -- it trusts the format the caller passed.
func TestStatFindsUploadedObject(t *testing.T) {
	store := newFakeStore()
	c := newTestClient("", store)

	ext4Path := mustWriteArtifact(t, "rfs-1.ext4", []byte("ext4"))
	if _, err := c.Upload(context.Background(), "rfs-1", ext4Path, ""); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// Correct format: Stat finds the .ext4 object.
	got, err := c.Stat(context.Background(), "rfs-1", "ext4")
	if err != nil || !got {
		t.Fatalf("Stat ext4 after ext4 upload: got=%v err=%v, want true", got, err)
	}
	// Wrong format: there is no .erofs object, and Stat does NOT probe .ext4 as a
	// fallback -- it returns false. This is the contract the build path relies
	// on: it passes the format the artifact was built in, so Stat and the
	// presigned URL that follows target the same single key.
	got, err = c.Stat(context.Background(), "rfs-1", "erofs")
	if err != nil {
		t.Fatalf("Stat wrong-format: %v", err)
	}
	if got {
		t.Fatal("Stat with erofs format must not find an ext4-only object (no cross-suffix fallback)")
	}
}

// TestDeleteUsesFormatKey verifies Delete removes the single object under the
// format key (not both suffixes).
func TestDeleteUsesFormatKey(t *testing.T) {
	store := newFakeStore()
	c := newTestClient("", store)
	store.objects["rfs-1.ext4"] = []byte("ext4")
	store.objects["rfs-1.erofs"] = []byte("erofs") // a stale object under the other suffix

	if err := c.Delete(context.Background(), "rfs-1", "ext4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(store.delKeys) != 1 || store.delKeys[0] != "rfs-1.ext4" {
		t.Fatalf("Delete keys = %v, want [rfs-1.ext4] (single key from format)", store.delKeys)
	}
	if _, ok := store.objects["rfs-1.ext4"]; ok {
		t.Fatal("ext4 object not deleted")
	}
	// The .erofs object is untouched: Delete targets the format key only.
	if _, ok := store.objects["rfs-1.erofs"]; !ok {
		t.Fatal("erofs object must not be deleted by an ext4-format Delete")
	}
}

// TestPresignedGetURLSignsExt4KeyForExt4Build is the core regression for the S3
// presign bug. The fake's SignedGetURL signs UNCONDITIONALLY (like the S3
// driver -- it does not verify the key exists), so the ONLY thing selecting the
// key is the rootfsFormat argument. An ext4 build must sign <id>.ext4, NOT
// <id>.erofs: before the fix, PresignedGetURL probed two candidate keys and the
// S3 backend signed the first (.erofs) unconditionally, handing cubelets a 404
// URL for an ext4 artifact.
func TestPresignedGetURLSignsExt4KeyForExt4Build(t *testing.T) {
	store := newFakeStore()
	store.sign = true // S3-like: SignedGetURL signs unconditionally
	c := newTestClient("", store)
	// Only the ext4 object exists (ext4 build uploaded under <id>.ext4).
	store.objects["rfs-ext.ext4"] = []byte("ext4")

	url, err := c.PresignedGetURL(context.Background(), "rfs-ext", "ext4")
	if err != nil {
		t.Fatalf("PresignedGetURL ext4: %v", err)
	}
	if !strings.Contains(url, "rfs-ext.ext4") {
		t.Fatalf("ext4 build must sign the .ext4 key, got %q", url)
	}
	if strings.Contains(url, "rfs-ext.erofs") {
		t.Fatalf("ext4 build must NOT sign the .erofs key (S3 would 404), got %q", url)
	}
	// Exactly one key was signed -- the format-derived one, no probe loop.
	if len(store.signedKeys) != 1 || store.signedKeys[0] != "rfs-ext.ext4" {
		t.Fatalf("signed keys = %v, want [rfs-ext.ext4] (single format key, no .erofs attempt)", store.signedKeys)
	}
}

// TestPresignedGetURLSignsErofsKeyForErofsBuild verifies the erofs counterpart:
// an erofs build signs <id>.erofs.
func TestPresignedGetURLSignsErofsKeyForErofsBuild(t *testing.T) {
	store := newFakeStore()
	store.sign = true
	c := newTestClient("", store)
	store.objects["rfs-ero.erofs"] = []byte("erofs")

	url, err := c.PresignedGetURL(context.Background(), "rfs-ero", "erofs")
	if err != nil {
		t.Fatalf("PresignedGetURL erofs: %v", err)
	}
	if !strings.Contains(url, "rfs-ero.erofs") {
		t.Fatalf("erofs build must sign the .erofs key, got %q", url)
	}
	if len(store.signedKeys) != 1 || store.signedKeys[0] != "rfs-ero.erofs" {
		t.Fatalf("signed keys = %v, want [rfs-ero.erofs]", store.signedKeys)
	}
}

// TestPresignedGetURLUnsupportedBackend verifies a non-signing backend surfaces
// ErrUnsupported from the single format key (not a probe loop).
func TestPresignedGetURLUnsupportedBackend(t *testing.T) {
	store := newFakeStore() // sign=false
	c := newTestClient("", store)

	_, err := c.PresignedGetURL(context.Background(), "rfs-1", "ext4")
	if err == nil {
		t.Fatal("expected ErrUnsupported from a non-signing backend, got nil")
	}
	if !errors.Is(err, blobstore.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

// TestFullObjectKeyHonorsSuffix verifies FullObjectKey (used to persist
// object_key for CubeMaster) carries the uploaded file's suffix, so the stored
// key matches what Upload wrote.
func TestFullObjectKeyHonorsSuffix(t *testing.T) {
	c := newTestClient("template-artifacts", newFakeStore())
	if got := c.FullObjectKey("rfs-1", "/x/rfs-1.erofs"); got != "template-artifacts/rfs-1.erofs" {
		t.Fatalf("erofs FullObjectKey = %q, want template-artifacts/rfs-1.erofs", got)
	}
	if got := c.FullObjectKey("rfs-1", "/x/rfs-1.ext4"); got != "template-artifacts/rfs-1.ext4" {
		t.Fatalf("ext4 FullObjectKey = %q, want template-artifacts/rfs-1.ext4", got)
	}
	// Unknown suffix -> legacy .ext4, matching Upload's fallback.
	if got := c.FullObjectKey("rfs-1", "/x/rfs-1.bin"); got != "template-artifacts/rfs-1.ext4" {
		t.Fatalf("unknown FullObjectKey = %q, want template-artifacts/rfs-1.ext4", got)
	}
}
