// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package s3store provides S3/MinIO object storage operations for template
// rootfs artifacts: upload, presigned URL generation, and deletion.
package s3store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config holds S3/MinIO connection parameters.
type Config struct {
	Endpoint        string // e.g. "play.min.io" or "s3.amazonaws.com"
	Bucket          string
	AccessKey       string
	SecretKey       string
	Region          string // optional; some S3-compatible stores ignore it
	UsePathStyle    bool   // true for MinIO; false for AWS S3 virtual-hosted style
	UseSSL          bool   // true for https, false for http
	PresignExpiry   time.Duration
	ArtifactPrefix  string // object key prefix, e.g. "template-artifacts/"
}

// DefaultPresignExpiry is the default presigned URL validity (7 days, the
// maximum allowed by AWS SigV4).
const DefaultPresignExpiry = 7 * 24 * time.Hour

// Client wraps minio-go operations for template artifacts.
type Client struct {
	cfg    Config
	client *minio.Client
}

// NewClient creates a new S3/MinIO client. Returns nil when config is invalid.
func NewClient(cfg Config) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("s3 endpoint is required")
	}
	// Strip scheme if present; minio-go takes host only.
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimRight(endpoint, "/")

	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3 access key and secret key are required")
	}
	if cfg.PresignExpiry <= 0 {
		cfg.PresignExpiry = DefaultPresignExpiry
	}

	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	}
	if cfg.UsePathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}

	return &Client{cfg: cfg, client: client}, nil
}

// ObjectKey returns the full object key for an artifact.
func (c *Client) ObjectKey(artifactID string) string {
	prefix := strings.TrimRight(c.cfg.ArtifactPrefix, "/")
	if prefix == "" {
		return artifactID + ".ext4"
	}
	return prefix + "/" + artifactID + ".ext4"
}

// Upload uploads a local ext4 file to S3/MinIO and returns the object key.
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

	_, err = c.client.PutObject(ctx, c.cfg.Bucket, key, f, st.Size(), minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return "", fmt.Errorf("put object %s/%s: %w", c.cfg.Bucket, key, err)
	}
	return key, nil
}

// PresignedGetURL generates a presigned GET URL for the artifact.
func (c *Client) PresignedGetURL(ctx context.Context, artifactID string) (string, error) {
	key := c.ObjectKey(artifactID)
	reqParams := make(url.Values)
	// Allow the response to carry the original filename for debugging.
	reqParams.Set("response-content-disposition", fmt.Sprintf("attachment; filename=\"%s.ext4\"", artifactID))
	u, err := c.client.PresignedGetObject(ctx, c.cfg.Bucket, key, c.cfg.PresignExpiry, reqParams)
	if err != nil {
		return "", fmt.Errorf("presign get %s/%s: %w", c.cfg.Bucket, key, err)
	}
	return u.String(), nil
}

// Delete removes the artifact object from S3/MinIO.
func (c *Client) Delete(ctx context.Context, artifactID string) error {
	key := c.ObjectKey(artifactID)
	err := c.client.RemoveObject(ctx, c.cfg.Bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("remove object %s/%s: %w", c.cfg.Bucket, key, err)
	}
	return nil
}

// Stat checks whether the artifact object exists.
func (c *Client) Stat(ctx context.Context, artifactID string) (bool, error) {
	key := c.ObjectKey(artifactID)
	_, err := c.client.StatObject(ctx, c.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
