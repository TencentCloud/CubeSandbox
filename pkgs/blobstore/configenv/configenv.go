// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package configenv parses the deployment-wide CUBE_S3_* / CUBE_ARTIFACT_*
// variables that CubeMaster and CubeTemplateCenter share.
//
// The blobstore core (Open / drivers) must not import this package: callers
// parse env here and pass a blobstore.Config.
package configenv

import (
	"os"
	"strings"
)

const (
	EnvS3Endpoint             = "CUBE_S3_ENDPOINT"
	EnvS3Bucket               = "CUBE_S3_BUCKET"
	EnvS3AccessKeyID          = "CUBE_S3_ACCESS_KEY_ID"
	LegacyEnvS3AccessKey      = "CUBE_S3_ACCESS_KEY"
	EnvS3SecretAccessKey      = "CUBE_S3_SECRET_ACCESS_KEY"
	LegacyEnvS3SecretKey      = "CUBE_S3_SECRET_KEY"
	EnvS3Region               = "CUBE_S3_REGION"
	EnvS3UsePathStyle         = "CUBE_S3_USE_PATH_STYLE"
	EnvS3UseSSL               = "CUBE_S3_USE_SSL"
	EnvS3ArtifactPrefix       = "CUBE_S3_ARTIFACT_PREFIX"
	EnvArtifactStoreBackend   = "CUBE_ARTIFACT_STORE_BACKEND"
	EnvArtifactStoreFSRoot    = "CUBE_ARTIFACT_STORE_FS_ROOT"
	EnvRootfsArtifactStoreDir = "CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR"

	// DefaultArtifactStoreDir is the on-disk artifact store when no env
	// override is set. Master and TC must resolve the same path.
	DefaultArtifactStoreDir = "/data/CubeMaster/storage"
)

// ArtifactS3 is the parsed CUBE_S3_* artifact-store configuration.
type ArtifactS3 struct {
	Enabled      bool
	Endpoint     string
	Bucket       string
	AccessKey    string
	SecretKey    string
	Region       string
	Prefix       string
	UsePathStyle bool
	UseSSL       bool
}

// ParseArtifactS3 reads CUBE_S3_*. Enabled is false and every field is empty
// when any of endpoint, bucket, access key, or secret key is missing.
func ParseArtifactS3() ArtifactS3 {
	cfg := ArtifactS3{
		Endpoint:     EnvOr(EnvS3Endpoint),
		Bucket:       EnvOr(EnvS3Bucket),
		Region:       EnvOr(EnvS3Region),
		Prefix:       EnvOr(EnvS3ArtifactPrefix),
		UsePathStyle: BoolEnv(EnvS3UsePathStyle),
		UseSSL:       BoolEnv(EnvS3UseSSL),
	}
	cfg.AccessKey, _ = Lookup(EnvS3AccessKeyID, LegacyEnvS3AccessKey)
	cfg.SecretKey, _ = Lookup(EnvS3SecretAccessKey, LegacyEnvS3SecretKey)
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return ArtifactS3{}
	}
	cfg.Enabled = true
	return cfg
}

// ArtifactStoreBackend returns "fs" or "s3" (default, including unknown values).
func ArtifactStoreBackend() string {
	if strings.ToLower(strings.TrimSpace(os.Getenv(EnvArtifactStoreBackend))) == "fs" {
		return "fs"
	}
	return "s3"
}

// ArtifactStoreFSRoot is CUBE_ARTIFACT_STORE_FS_ROOT, or empty.
func ArtifactStoreFSRoot() string {
	return EnvOr(EnvArtifactStoreFSRoot)
}

// ArtifactStoreDir is CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR, or DefaultArtifactStoreDir.
func ArtifactStoreDir() string {
	if root := EnvOr(EnvRootfsArtifactStoreDir); root != "" {
		return root
	}
	return DefaultArtifactStoreDir
}

// ResolveArtifactFSRoot is CUBE_ARTIFACT_STORE_FS_ROOT, else ArtifactStoreDir.
func ResolveArtifactFSRoot() string {
	if root := ArtifactStoreFSRoot(); root != "" {
		return root
	}
	return ArtifactStoreDir()
}

// EnvOr returns the first non-empty trimmed environment value.
func EnvOr(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// BoolEnv reports whether name is a truthy flag (1/true/yes/on).
func BoolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Lookup reads canonical, then legacy. Empty values count as unset.
func Lookup(canonical, legacy string) (string, bool) {
	v := EnvOr(canonical, legacy)
	return v, v != ""
}
