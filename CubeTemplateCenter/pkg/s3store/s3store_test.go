// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionedObjectKeyBindsContent(t *testing.T) {
	client := &Client{}
	first, err := client.VersionedObjectKey("artifact-1", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.VersionedObjectKey("artifact-1", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasSuffix(first, ".ext4") {
		t.Fatalf("versioned keys = %q and %q", first, second)
	}
}

func TestStorageRefUsesNodeEndpointWithoutSecrets(t *testing.T) {
	client := &Client{cfg: Config{
		Driver:         "s3",
		Endpoint:       "http://control-minio:9000",
		NodeEndpoint:   "http://node-minio:9000",
		Bucket:         "artifact-bucket",
		AccessKey:      "access-key-must-not-appear",
		SecretKey:      "secret-key-must-not-appear",
		ArtifactPrefix: "/template-artifacts/",
	}}
	got := client.StorageRef()
	want := "s3://artifact-bucket/template-artifacts?endpoint=http%3A%2F%2Fnode-minio%3A9000"
	if got != want {
		t.Fatalf("storage ref = %q, want %q", got, want)
	}
	if strings.Contains(got, "access-key") || strings.Contains(got, "secret-key") {
		t.Fatalf("storage ref leaked credentials: %q", got)
	}
}

func TestStoredUserKeyRequiresConfiguredPrefix(t *testing.T) {
	client := &Client{cfg: Config{ArtifactPrefix: "templates"}}
	got, err := client.storedUserKey("templates/artifact-1.ext4", "artifact-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "artifact-1.ext4" {
		t.Fatalf("key = %q", got)
	}
	for _, key := range []string{"old-prefix/artifact-1.ext4", "artifact-1.ext4", "templates-other/artifact-1.ext4"} {
		if _, err := client.storedUserKey(key, "artifact-1"); err == nil {
			t.Fatalf("mismatched prefix unexpectedly accepted: %q", key)
		}
	}
}

func TestFSStorageRefUsesActualAbsoluteRoot(t *testing.T) {
	root := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(wd, root)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{Driver: "fs", FSRoot: relative, ArtifactPrefix: "templates"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.store.Close() })
	if client.cfg.FSRoot != root || !filepath.IsAbs(client.cfg.FSRoot) {
		t.Fatalf("configured root = %q, want absolute %q", client.cfg.FSRoot, root)
	}
	file := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := os.WriteFile(file, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := client.Upload(t.Context(), "artifact-1", file, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ValidateStored("fs", client.StorageRef(), key, "artifact-1"); err != nil {
		t.Fatal(err)
	}
	if exists, err := client.StatStored(t.Context(), key, "artifact-1"); err != nil || !exists {
		t.Fatalf("uploaded object exists = %v, error = %v", exists, err)
	}
	if err := client.ValidateStored("fs", "file://"+t.TempDir(), key, "artifact-1"); err == nil {
		t.Fatal("foreign filesystem root unexpectedly accepted")
	}
}

func TestValidateStoredS3UsesStableLocatorWithoutNetwork(t *testing.T) {
	cfg := Config{
		Driver: "s3", Endpoint: "http://control.example.invalid:9000",
		NodeEndpoint: "http://node.example.invalid:9000", Bucket: "artifact-bucket",
		AccessKey: "test-access", SecretKey: "test-secret", ArtifactPrefix: "templates",
	}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.store.Close() })
	key := "templates/artifact-1.ext4"
	for _, tc := range []struct {
		name       string
		backend    string
		storageRef string
		objectKey  string
		wantError  bool
	}{
		{"matching", "s3", client.StorageRef(), key, false},
		{"normalized-backend", " S3 ", client.StorageRef(), key, false},
		{"missing-backend", "", client.StorageRef(), key, true},
		{"wrong-backend", "fs", client.StorageRef(), key, true},
		{"legacy-missing-ref", "s3", "", key, true},
		{"missing-key", "s3", client.StorageRef(), "", true},
		{"missing-prefix", "s3", client.StorageRef(), "artifact-1.ext4", true},
		{"wrong-prefix", "s3", client.StorageRef(), "other/artifact-1.ext4", true},
		{"invalid-key", "s3", client.StorageRef(), "templates/../artifact-1.ext4", true},
		{"wrong-bucket", "s3", strings.Replace(client.StorageRef(), "artifact-bucket", "foreign-bucket", 1), key, true},
		{"wrong-node-endpoint", "s3", strings.Replace(client.StorageRef(), "node.example.invalid", "foreign.example.invalid", 1), key, true},
		{"wrong-ref-prefix", "s3", strings.Replace(client.StorageRef(), "/templates?", "/other?", 1), key, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := client.ValidateStored(tc.backend, tc.storageRef, tc.objectKey, "artifact-1")
			if (err != nil) != tc.wantError {
				t.Fatalf("ValidateStored error = %v, wantError = %v", err, tc.wantError)
			}
		})
	}
	cfg.NodeEndpoint = ""
	controlOnly, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlOnly.store.Close() })
	if err := controlOnly.ValidateStored("s3", controlOnly.StorageRef(), key, "artifact-1"); err != nil {
		t.Fatal(err)
	}
	foreignRef := strings.Replace(controlOnly.StorageRef(), "control.example.invalid", "foreign.example.invalid", 1)
	if err := controlOnly.ValidateStored("s3", foreignRef, key, "artifact-1"); err == nil {
		t.Fatal("foreign control endpoint unexpectedly accepted")
	}
	var missing *Client
	if err := missing.ValidateStored("s3", client.StorageRef(), key, "artifact-1"); err == nil {
		t.Fatal("missing client unexpectedly accepted")
	}
}
