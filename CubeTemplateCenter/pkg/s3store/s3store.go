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

// artifactKeySuffix picks the on-disk suffix for an artifact from the local
// file's own name, so an erofs artifact is stored as <id>.erofs -- matching the
// build path's naming (BuildErofs writes <id>.erofs) instead of being mislabeled
// <id>.ext4, which is confusing when inspecting an erofs object in the bucket.
// An unknown or missing suffix falls back to .ext4, the historical storage name,
// so a legacy or oddly-named upload keeps landing where it always did.
func artifactKeySuffix(localPath string) string {
	suffix := strings.ToLower(filepath.Ext(localPath))
	if suffix != ".erofs" && suffix != ".ext4" {
		return ".ext4"
	}
	return suffix
}

// objectKeyWithSuffix returns the user key <artifactID><suffix> (prefix applied
// by blobstore). suffix must be ".ext4" or ".erofs".
func (c *Client) objectKeyWithSuffix(artifactID, suffix string) string {
	return blobstore.JoinKey("", artifactID+suffix)
}

// fullKeyWithSuffix returns the on-backend key including the configured prefix.
func (c *Client) fullKeyWithSuffix(artifactID, suffix string) string {
	return blobstore.JoinKey(c.cfg.ArtifactPrefix, artifactID+suffix)
}

// artifactKeyFromFormat returns the single user key <artifactID><suffix> for
// the given rootfs format, where suffix is derived from rootfsFormat the same
// way Upload derives it from the local file's name (erofs -> .erofs, ext4 /
// empty / unknown -> .ext4). Callers pass the format the artifact was BUILT in
// (req.RootfsFormat on the build path, existing.RootfsFormat on the reuse
// path), so the key read here is the exact key Upload wrote under -- there is
// no probing. The store prefix is applied by blobstore.
func (c *Client) artifactKeyFromFormat(artifactID, rootfsFormat string) string {
	return c.objectKeyWithSuffix(artifactID, artifactSuffixForFormat(rootfsFormat))
}

// artifactSuffixForFormat maps a rootfs format string to the on-disk artifact
// suffix, mirroring artifactKeySuffix (which reads the suffix off a local file
// path). erofs -> ".erofs"; ext4, empty, or any unknown value -> ".ext4" (the
// historical default, which also matches legacy rows whose RootfsFormat column
// predates the erofs feature).
func artifactSuffixForFormat(rootfsFormat string) string {
	if strings.EqualFold(strings.TrimSpace(rootfsFormat), "erofs") {
		return ".erofs"
	}
	return ".ext4"
}

// ObjectKey returns the legacy user key (artifactID.ext4). The store prefix is
// applied by blobstore. Prefer Upload/FullObjectKey (which honor the uploaded
// file's suffix) for writes; this remains for callers that only need a stable
// legacy key and do not have a local path.
func (c *Client) ObjectKey(artifactID string) string {
	return c.objectKeyWithSuffix(artifactID, ".ext4")
}

// FullObjectKey is the on-backend key including the configured prefix. It uses
// the uploaded file's suffix so an erofs artifact's stored object_key matches
// the key Upload actually wrote (the two MUST agree: Upload writes under this
// key, CubeMaster persists it in rootfs_artifacts.object_key, and later reads
// locate the object by that stored key).
func (c *Client) FullObjectKey(artifactID, localPath string) string {
	return c.fullKeyWithSuffix(artifactID, artifactKeySuffix(localPath))
}

// Upload uploads a local rootfs artifact file (ext4 or erofs) and returns the
// full object key. The object is stored under <artifactID><suffix> where suffix
// is taken from localPath, so an erofs artifact becomes <id>.erofs in the
// bucket. sha256 is optional; when set it is written as UserMetadata on the PUT.
func (c *Client) Upload(ctx context.Context, artifactID, localPath, sha256 string) (string, error) {
	suffix := artifactKeySuffix(localPath)
	key := c.objectKeyWithSuffix(artifactID, suffix)
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
	return c.fullKeyWithSuffix(artifactID, suffix), nil
}

// PresignedGetURL generates a download URL for the artifact built in
// rootfsFormat. The key is derived from rootfsFormat (not probed by artifactID),
// so it is the exact key Upload wrote under -- this matters on the S3 backend,
// whose SignedGetURL signs a key WITHOUT checking it exists: probing two
// candidate keys would always return the first (.erofs) URL even for an ext4
// artifact, handing cubelets a 404. Passing the known format yields one correct
// key. Returns ErrUnsupported when the backend cannot sign.
func (c *Client) PresignedGetURL(ctx context.Context, artifactID, rootfsFormat string) (string, error) {
	key := c.artifactKeyFromFormat(artifactID, rootfsFormat)
	url, err := c.store.SignedGetURL(ctx, key, c.cfg.PresignExpiry)
	if err != nil {
		return "", err
	}
	return url, nil
}

// Delete removes the artifact object built in rootfsFormat. The key is derived
// from rootfsFormat (the exact key Upload wrote), not probed. A missing key is
// not an error.
func (c *Client) Delete(ctx context.Context, artifactID, rootfsFormat string) error {
	key := c.artifactKeyFromFormat(artifactID, rootfsFormat)
	if err := c.store.Delete(ctx, key); err != nil && !blobstore.IsNotExist(err) {
		return err
	}
	return nil
}

// Stat checks whether the artifact object built in rootfsFormat exists. The
// key is derived from rootfsFormat (the exact key Upload wrote), not probed, so
// the Stat guard and the PresignedGetURL that follows it target the SAME key
// (the pre-fix code probed two keys in Stat but only one in the buggy
// PresignedGetURL, so the guard could pass on .ext4 while the URL pointed at
// .erofs).
func (c *Client) Stat(ctx context.Context, artifactID, rootfsFormat string) (bool, error) {
	key := c.artifactKeyFromFormat(artifactID, rootfsFormat)
	_, err := c.store.Stat(ctx, key)
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
