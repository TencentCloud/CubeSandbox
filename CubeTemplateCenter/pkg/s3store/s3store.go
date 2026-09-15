// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package s3store uploads, stats, and deletes template rootfs artifacts
// through pkgs/blobstore.
package s3store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	fsdriver "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/fs"
	s3driver "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/s3"
)

// Config holds S3/MinIO or fs connection parameters.
type Config struct {
	Driver         string // "s3" (default) or "fs"
	Endpoint       string
	Bucket         string
	AccessKey      string
	SecretKey      string
	Region         string
	UsePathStyle   bool
	UseSSL         bool
	PresignExpiry  time.Duration
	ArtifactPrefix string
	FSRoot         string
}

// DefaultPresignExpiry is the default presigned URL validity (7 days, the
// maximum allowed by AWS SigV4).
const DefaultPresignExpiry = 7 * 24 * time.Hour

// Client wraps a blobstore.Store for template artifacts.
type Client struct {
	cfg   Config
	store blobstore.Store
}

// NewClient creates a new artifact store client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.PresignExpiry <= 0 {
		cfg.PresignExpiry = DefaultPresignExpiry
	}
	cfg.Driver = normalizeDriver(cfg.Driver)
	st, err := openConfiguredStore(cfg)
	if err != nil {
		return nil, err
	}
	// fs must Prepare (dirs, rename probe, signer). s3 Prepare talks to the
	// bucket; keep today's lazy connect so a fully-specified config still
	// constructs a client when the endpoint is unreachable.
	if cfg.Driver == "fs" {
		if err := st.Prepare(context.Background()); err != nil {
			_ = st.Close()
			return nil, err
		}
	}
	return &Client{cfg: cfg, store: st}, nil
}

func normalizeDriver(driver string) string {
	driver = strings.ToLower(strings.TrimSpace(driver))
	if driver == "" {
		return "s3"
	}
	return driver
}

func openConfiguredStore(cfg Config) (blobstore.Store, error) {
	switch cfg.Driver {
	case "fs":
		root := strings.TrimSpace(cfg.FSRoot)
		if root == "" {
			return nil, fmt.Errorf("fs root is required")
		}
		return blobstore.Open(context.Background(), fsdriver.Options{
			Root: root,
			Sync: fsdriver.SyncAlways,
		}.Config(root, strings.Trim(cfg.ArtifactPrefix, "/")))
	case "s3":
		if strings.TrimSpace(cfg.Endpoint) == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("s3 endpoint, bucket, and credentials are required")
		}
		useSSL := cfg.UseSSL
		return blobstore.Open(context.Background(), s3driver.Options{
			Endpoint:           cfg.Endpoint,
			AccessKeyID:        cfg.AccessKey,
			SecretAccessKey:    cfg.SecretKey,
			Bucket:             cfg.Bucket,
			Region:             cfg.Region,
			PathStyle:          cfg.UsePathStyle,
			UseSSL:             &useSSL,
			PresignExpiry:      cfg.PresignExpiry,
			AttachmentBasename: true,
		}.Config(strings.Trim(cfg.ArtifactPrefix, "/")))
	default:
		return nil, fmt.Errorf("unknown artifact store driver %q", cfg.Driver)
	}
}

// ObjectKey returns the user key (artifactID.ext4). The store prefix is applied by blobstore.
func (c *Client) ObjectKey(artifactID string) string {
	return blobstore.ArtifactExt4Key("", artifactID)
}

// FullObjectKey is the on-backend key including the configured prefix.
func (c *Client) FullObjectKey(artifactID string) string {
	return blobstore.ArtifactExt4Key(c.cfg.ArtifactPrefix, artifactID)
}

// Upload uploads a local ext4 file and returns the object key.
func (c *Client) Upload(ctx context.Context, artifactID, localPath string) (string, error) {
	key := c.ObjectKey(artifactID)
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", localPath, err)
	}
	_, err = c.store.Put(ctx, key, f, blobstore.PutOptions{
		ContentType: "application/octet-stream",
		Size:        st.Size(),
	})
	if err != nil {
		return "", fmt.Errorf("put object %s: %w", key, err)
	}
	return c.FullObjectKey(artifactID), nil
}

// PresignedGetURL generates a download URL for the artifact.
func (c *Client) PresignedGetURL(ctx context.Context, artifactID string) (string, error) {
	return c.store.SignedGetURL(ctx, c.ObjectKey(artifactID), c.cfg.PresignExpiry)
}

// Delete removes the artifact object.
func (c *Client) Delete(ctx context.Context, artifactID string) error {
	return c.store.Delete(ctx, c.ObjectKey(artifactID))
}

// Stat checks whether the artifact object exists.
func (c *Client) Stat(ctx context.Context, artifactID string) (bool, error) {
	_, err := c.store.Stat(ctx, c.ObjectKey(artifactID))
	if err != nil {
		if blobstore.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// BackendName returns s3 or fs.
func (c *Client) BackendName() string {
	return normalizeDriver(c.cfg.Driver)
}
