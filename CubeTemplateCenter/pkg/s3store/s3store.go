// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package s3store uploads, stats, and deletes template rootfs artifacts
// through pkgs/blobstore.
package s3store

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	NodeEndpoint   string
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
	if cfg.Driver == "fs" {
		root := strings.TrimSpace(cfg.FSRoot)
		if root == "" {
			return nil, fmt.Errorf("fs root is required")
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve fs root: %w", err)
		}
		cfg.FSRoot = abs
	}
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
			NodeEndpoint:       cfg.NodeEndpoint,
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

// VersionedObjectKey binds an object name to immutable content. Rebuilding the
// same artifact ID with different bytes must not overwrite an object still
// reachable through an older presigned URL.
func (c *Client) VersionedObjectKey(artifactID, sha256 string) (string, error) {
	digest := strings.ToLower(strings.TrimSpace(sha256))
	if len(digest) != 64 {
		return "", fmt.Errorf("artifact sha256 must contain 64 hex characters")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("artifact sha256 is invalid: %w", err)
	}
	return blobstore.ArtifactExt4Key("", artifactID+"-"+digest), nil
}

// Upload uploads a local ext4 file and returns the object key.
// sha256 is optional; when set it is written as UserMetadata on the original PUT.
func (c *Client) Upload(ctx context.Context, artifactID, localPath, sha256 string) (string, error) {
	key := c.ObjectKey(artifactID)
	return c.uploadKey(ctx, key, localPath, sha256)
}

func (c *Client) UploadVersioned(ctx context.Context, artifactID, localPath, sha256 string) (string, error) {
	key, err := c.VersionedObjectKey(artifactID, sha256)
	if err != nil {
		return "", err
	}
	return c.uploadKey(ctx, key, localPath, sha256)
}

func (c *Client) uploadKey(ctx context.Context, key, localPath, sha256 string) (string, error) {
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
		SHA256:      sha256,
	})
	if err != nil {
		return "", fmt.Errorf("put object %s: %w", key, err)
	}
	prefix := strings.Trim(c.cfg.ArtifactPrefix, "/")
	if prefix == "" {
		return key, nil
	}
	return prefix + "/" + key, nil
}

// PresignedGetURL generates a download URL for the artifact.
func (c *Client) PresignedGetURL(ctx context.Context, artifactID string) (string, error) {
	return c.store.SignedGetURL(ctx, c.ObjectKey(artifactID), c.cfg.PresignExpiry)
}

// Delete removes the artifact object.
func (c *Client) Delete(ctx context.Context, artifactID string) error {
	return c.store.Delete(ctx, c.ObjectKey(artifactID))
}

// ValidateStored verifies that a persisted locator belongs to this client.
// Legacy rows without a stable locator require explicit migration, not guessing.
func (c *Client) ValidateStored(backend, storageRef, objectKey, artifactID string) error {
	if c == nil || c.store == nil {
		return fmt.Errorf("artifact store client is not configured")
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" || backend != c.BackendName() {
		return fmt.Errorf("stored artifact backend %q does not match configured backend %q", backend, c.BackendName())
	}
	if strings.TrimSpace(storageRef) == "" {
		return fmt.Errorf("stored artifact storage_ref is required; explicit migration is needed")
	}
	if storageRef != c.StorageRef() {
		return fmt.Errorf("stored artifact storage_ref does not match configured store")
	}
	if objectKey == "" {
		return fmt.Errorf("stored artifact object_key is required")
	}
	if err := blobstore.ValidateKey(objectKey); err != nil {
		return fmt.Errorf("stored artifact object key is invalid: %w", err)
	}
	_, err := c.storedUserKey(objectKey, artifactID)
	return err
}

func (c *Client) DeleteStored(ctx context.Context, objectKey, artifactID string) error {
	key, err := c.storedUserKey(objectKey, artifactID)
	if err != nil {
		return err
	}
	return c.store.Delete(ctx, key)
}

// Stat checks whether the artifact object exists.
func (c *Client) Stat(ctx context.Context, artifactID string) (bool, error) {
	return c.statKey(ctx, c.ObjectKey(artifactID))
}

func (c *Client) StatStored(ctx context.Context, objectKey, artifactID string) (bool, error) {
	key, err := c.storedUserKey(objectKey, artifactID)
	if err != nil {
		return false, err
	}
	return c.statKey(ctx, key)
}

func (c *Client) statKey(ctx context.Context, key string) (bool, error) {
	_, err := c.store.Stat(ctx, key)
	if err != nil {
		if blobstore.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Client) storedUserKey(objectKey, artifactID string) (string, error) {
	stored := strings.Trim(strings.TrimSpace(objectKey), "/")
	if stored == "" {
		return c.ObjectKey(artifactID), nil
	}
	prefix := strings.Trim(c.cfg.ArtifactPrefix, "/")
	if prefix != "" {
		if strings.HasPrefix(stored, prefix+"/") {
			stored = strings.TrimPrefix(stored, prefix+"/")
		} else {
			return "", fmt.Errorf("stored object key %q does not match configured prefix %q", objectKey, prefix)
		}
	}
	if err := blobstore.ValidateKey(stored); err != nil {
		return "", fmt.Errorf("stored artifact object key is invalid: %w", err)
	}
	return stored, nil
}

// BackendName returns s3 or fs.
func (c *Client) BackendName() string {
	return normalizeDriver(c.cfg.Driver)
}

// StorageRef returns a stable, non-secret locator suitable for persistence.
// Access keys and presigned query parameters are deliberately excluded.
func (c *Client) StorageRef() string {
	if c == nil {
		return ""
	}
	if c.BackendName() == "fs" {
		return (&url.URL{Scheme: "file", Path: filepath.Clean(c.cfg.FSRoot)}).String()
	}
	u := &url.URL{Scheme: "s3", Host: c.cfg.Bucket, Path: "/" + strings.Trim(c.cfg.ArtifactPrefix, "/")}
	q := u.Query()
	endpoint := strings.TrimSpace(c.cfg.NodeEndpoint)
	if endpoint == "" {
		endpoint = c.cfg.Endpoint
	}
	q.Set("endpoint", endpoint)
	u.RawQuery = q.Encode()
	return u.String()
}
