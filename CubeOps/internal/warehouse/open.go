// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	fsdriver "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/fs"
	s3driver "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/s3"
)

func warehouseRetention() []blobstore.RetentionRule {
	return []blobstore.RetentionRule{
		{Prefix: Prefix, AbortIncompleteAfter: 24 * time.Hour},
		{Prefix: uploadsPrefix, ExpireAfter: 24 * time.Hour},
	}
}

// OpenS3 opens the shared S3-compatible warehouse store.
func OpenS3(cfg config.S3Config, putTimeout time.Duration) (BlobStore, error) {
	c := s3driver.Options{
		Endpoint:        cfg.Endpoint,
		NodeEndpoint:    cfg.NodeEndpoint,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		Bucket:          cfg.Bucket,
		Region:          cfg.Region,
		PathStyle:       cfg.UsePathStyle(),
		CreateBucket:    cfg.ShouldCreateBucket(),
		PutTimeout:      putTimeout,
		PresignExpiry:   5 * time.Minute,
	}.Config("")
	return openAdapted(c)
}

// OpenFS opens the local-directory warehouse store.
func OpenFS(cfg config.StoreFSBackendConfig, presignTTL time.Duration) (BlobStore, error) {
	key, err := decodeFSSigningKey(cfg.SigningKey)
	if err != nil {
		return nil, err
	}
	c := fsdriver.Options{
		Root:             cfg.Root,
		PublicBaseURL:    cfg.PublicURL,
		SigningKey:       key,
		SignTTL:          presignTTL,
		RequirePublicURL: true,
		Sync:             fsdriver.SyncAlways,
		MountPath:        ObjectMountPath,
		Shared:           cfg.Shared,
	}.Config(cfg.Root, "")
	return openAdapted(c)
}

func decodeFSSigningKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("CUBE_OPS_STORE_FS_SIGNING_KEY: %w", err)
	}
	return key, nil
}

func openAdapted(c blobstore.Config) (BlobStore, error) {
	c.Retention = warehouseRetention()
	st, err := blobstore.Open(context.Background(), c)
	if err != nil {
		return nil, err
	}
	return Adapt(st), nil
}
