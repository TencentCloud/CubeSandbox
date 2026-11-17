// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"net/url"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
)

func TestSharedS3ClientAddressingStyle(t *testing.T) {
	resetSharedS3(t)
	t.Cleanup(func() { resetSharedS3(t) })
	t.Setenv(configenv.EnvArtifactStoreBackend, "s3")
	t.Setenv(configenv.EnvS3Endpoint, "https://storage.example.test")
	t.Setenv(configenv.EnvS3Bucket, "artifacts")
	t.Setenv(configenv.EnvS3AccessKeyID, "test-access-key")
	t.Setenv(configenv.EnvS3SecretAccessKey, "test-secret-key")
	t.Setenv(configenv.EnvS3Region, "us-east-1")
	t.Setenv(configenv.EnvS3UseSSL, "true")
	t.Setenv(configenv.EnvS3UsePathStyle, "true")
	t.Setenv(configenv.EnvS3AddressingStyle, "virtual")
	t.Setenv(configenv.EnvS3ArtifactPrefix, "templates")
	client, enabled := SharedS3Client()
	if !enabled || client == nil {
		t.Fatal("expected configured S3 client")
	}
	raw, err := client.PresignedGetURL(t.Context(), "rfs-test")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "artifacts.storage.example.test" || u.Path != "/templates/rfs-test.ext4" {
		t.Fatalf("CUBE_S3_ADDRESSING_STYLE was not forwarded to artifact uploads/downloads: host=%q path=%q", u.Host, u.Path)
	}
}

// resetSharedS3 resets the package-level singleton so each test starts clean.
func resetSharedS3(t *testing.T) {
	t.Helper()
	sharedS3 = s3ClientFactory{}
}

func setS3Env(t *testing.T, endpoint, bucket, ak, sk string) {
	t.Helper()
	t.Setenv(tcconfig.EnvS3Endpoint, endpoint)
	t.Setenv(tcconfig.EnvS3Bucket, bucket)
	t.Setenv(tcconfig.EnvS3AccessKey, ak)
	t.Setenv(tcconfig.EnvS3SecretKey, sk)
}

func TestSharedS3ClientDisabled(t *testing.T) {
	resetSharedS3(t)
	// No env set -> disabled.
	t.Setenv(tcconfig.EnvS3Endpoint, "")
	t.Setenv(tcconfig.EnvS3Bucket, "")
	t.Setenv(tcconfig.EnvS3AccessKey, "")
	t.Setenv(tcconfig.EnvS3SecretKey, "")

	client, enabled := SharedS3Client()
	if enabled {
		t.Fatalf("expected disabled, got enabled")
	}
	if client != nil {
		t.Fatalf("expected nil client, got %v", client)
	}
}

func TestSharedS3ClientIncompleteConfig(t *testing.T) {
	resetSharedS3(t)
	// Missing bucket/keys -> S3Config reports disabled.
	setS3Env(t, "minio:9000", "", "", "")

	client, enabled := SharedS3Client()
	if enabled {
		t.Fatalf("expected disabled for incomplete config")
	}
	if client != nil {
		t.Fatalf("expected nil client")
	}
}

func TestSharedS3ClientEnabled(t *testing.T) {
	resetSharedS3(t)
	setS3Env(t, "minio:9000", "artifacts", "ak", "sk")

	client, enabled := SharedS3Client()
	if !enabled {
		t.Fatalf("expected enabled with full config")
	}
	if client == nil {
		t.Fatalf("expected non-nil client with full config")
	}
}

func TestSharedS3ClientSingleton(t *testing.T) {
	resetSharedS3(t)
	setS3Env(t, "minio:9000", "artifacts", "ak", "sk")

	c1, _ := SharedS3Client()
	c2, _ := SharedS3Client()
	if c1 != c2 {
		t.Fatalf("expected same client pointer (singleton), got different")
	}
}

func TestSharedS3ClientConcurrent(t *testing.T) {
	resetSharedS3(t)
	setS3Env(t, "minio:9000", "artifacts", "ak", "sk")

	// Concurrent first-calls must not race or construct multiple clients.
	const n = 16
	var wg sync.WaitGroup
	clients := make([]interface{}, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			c, _ := SharedS3Client()
			clients[idx] = c
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if clients[i] != clients[0] {
			t.Fatalf("expected all concurrent calls to return the same client")
		}
	}
}

func TestS3ClientFactoryOnceSemantics(t *testing.T) {
	// Test the factory directly (not the package singleton) to verify env is
	// read exactly once: changing env after first call has no effect.
	setS3Env(t, "minio:9000", "artifacts", "ak", "sk")
	f := &s3ClientFactory{}
	c1, e1 := f.instance()
	if !e1 || c1 == nil {
		t.Fatalf("expected enabled on first call")
	}
	// Change env to empty; the Once must not re-read.
	t.Setenv(tcconfig.EnvS3Endpoint, "")
	t.Setenv(tcconfig.EnvS3Bucket, "")
	c2, e2 := f.instance()
	if !e2 || c2 != c1 {
		t.Fatalf("expected instance cached after first call regardless of env change")
	}
}
