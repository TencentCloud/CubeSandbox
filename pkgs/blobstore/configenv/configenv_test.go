// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package configenv

import "testing"

func clearArtifactEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvS3Endpoint, EnvS3Bucket, EnvS3AccessKeyID, LegacyEnvS3AccessKey,
		EnvS3SecretAccessKey, LegacyEnvS3SecretKey, EnvS3Region, EnvS3UsePathStyle,
		EnvS3UseSSL, EnvS3ArtifactPrefix, EnvArtifactStoreBackend, EnvArtifactStoreFSRoot,
		EnvRootfsArtifactStoreDir,
	} {
		t.Setenv(name, "")
	}
}

func TestParseArtifactS3Complete(t *testing.T) {
	clearArtifactEnv(t)
	t.Setenv(EnvS3Endpoint, "http://minio:9000")
	t.Setenv(EnvS3Bucket, "artifacts")
	t.Setenv(EnvS3AccessKeyID, "ak")
	t.Setenv(EnvS3SecretAccessKey, "sk")
	t.Setenv(EnvS3Region, "us-east-1")
	t.Setenv(EnvS3ArtifactPrefix, "template-artifacts")
	t.Setenv(EnvS3UsePathStyle, "true")
	t.Setenv(EnvS3UseSSL, "1")
	got := ParseArtifactS3()
	want := ArtifactS3{
		Enabled:      true,
		Endpoint:     "http://minio:9000",
		Bucket:       "artifacts",
		AccessKey:    "ak",
		SecretKey:    "sk",
		Region:       "us-east-1",
		Prefix:       "template-artifacts",
		UsePathStyle: true,
		UseSSL:       true,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseArtifactS3IncompleteClears(t *testing.T) {
	clearArtifactEnv(t)
	t.Setenv(EnvS3Endpoint, "http://minio:9000")
	t.Setenv(EnvS3Bucket, "artifacts")
	t.Setenv(EnvS3ArtifactPrefix, "keep-me")
	got := ParseArtifactS3()
	if got != (ArtifactS3{}) {
		t.Fatalf("incomplete config must clear fields, got %+v", got)
	}
}

func TestParseArtifactS3LegacyKeys(t *testing.T) {
	clearArtifactEnv(t)
	t.Setenv(EnvS3Endpoint, "http://minio:9000")
	t.Setenv(EnvS3Bucket, "artifacts")
	t.Setenv(LegacyEnvS3AccessKey, "legacy-ak")
	t.Setenv(LegacyEnvS3SecretKey, "legacy-sk")
	got := ParseArtifactS3()
	if !got.Enabled || got.AccessKey != "legacy-ak" || got.SecretKey != "legacy-sk" {
		t.Fatalf("%+v", got)
	}
}

func TestParseArtifactS3CanonicalWins(t *testing.T) {
	clearArtifactEnv(t)
	t.Setenv(EnvS3Endpoint, "http://minio:9000")
	t.Setenv(EnvS3Bucket, "artifacts")
	t.Setenv(EnvS3AccessKeyID, "ak-id")
	t.Setenv(EnvS3SecretAccessKey, "sk-id")
	t.Setenv(LegacyEnvS3AccessKey, "legacy-ak")
	t.Setenv(LegacyEnvS3SecretKey, "legacy-sk")
	got := ParseArtifactS3()
	if got.AccessKey != "ak-id" || got.SecretKey != "sk-id" {
		t.Fatalf("canonical must win: %+v", got)
	}
}

func TestArtifactStoreBackendNormalize(t *testing.T) {
	clearArtifactEnv(t)
	if got := ArtifactStoreBackend(); got != "s3" {
		t.Fatalf("default=%q", got)
	}
	t.Setenv(EnvArtifactStoreBackend, "FS")
	if got := ArtifactStoreBackend(); got != "fs" {
		t.Fatalf("fs=%q", got)
	}
	t.Setenv(EnvArtifactStoreBackend, "memory")
	if got := ArtifactStoreBackend(); got != "s3" {
		t.Fatalf("unknown=%q", got)
	}
}

func TestResolveArtifactFSRoot(t *testing.T) {
	clearArtifactEnv(t)
	if got := ResolveArtifactFSRoot(); got != DefaultArtifactStoreDir {
		t.Fatalf("default=%q", got)
	}
	t.Setenv(EnvRootfsArtifactStoreDir, "/data/store")
	if got := ArtifactStoreDir(); got != "/data/store" {
		t.Fatalf("store dir=%q", got)
	}
	if got := ResolveArtifactFSRoot(); got != "/data/store" {
		t.Fatalf("resolve store dir=%q", got)
	}
	t.Setenv(EnvArtifactStoreFSRoot, "/var/blobs")
	if got := ResolveArtifactFSRoot(); got != "/var/blobs" {
		t.Fatalf("fs root wins=%q", got)
	}
}

func TestLookupEmptyDoesNotShadow(t *testing.T) {
	clearArtifactEnv(t)
	t.Setenv(EnvS3AccessKeyID, "   ")
	t.Setenv(LegacyEnvS3AccessKey, "legacy-ak")
	got, ok := Lookup(EnvS3AccessKeyID, LegacyEnvS3AccessKey)
	if !ok || got != "legacy-ak" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}
