// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package s3api implements the Controller hooks (create / destroy) against any
// S3-compatible endpoint using the MinIO Go client.
package s3api

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

// Client wraps minio-go for volume lifecycle operations.
type Client struct {
	bucket string
	region string
	inner  *minio.Client
}

// New builds an S3 client from plugin config.
func New(cfg *config.Config) (*Client, error) {
	host, secure, err := ParseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}

	inner, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client for %q: %w", cfg.Endpoint, err)
	}
	return &Client{bucket: cfg.Bucket, region: cfg.Region, inner: inner}, nil
}

// ParseEndpoint splits an endpoint URL into the host:port minio-go expects and
// whether TLS is used. A bare host without scheme is treated as https.
func ParseEndpoint(endpoint string) (host string, secure bool, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, fmt.Errorf("endpoint is empty")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "https":
		secure = true
	case "http":
		secure = false
	default:
		return "", false, fmt.Errorf("endpoint %q: unsupported scheme %q", endpoint, u.Scheme)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("endpoint %q has no host", endpoint)
	}
	// minio-go addresses the endpoint by host:port only; it cannot carry a URL
	// path. s3fs still receives the full cfg.Endpoint via -ourl=, so a
	// path-prefixed endpoint (e.g. https://host/s3proxy) would silently split
	// the control plane (Create/Destroy) and data plane (Attach/Detach) onto
	// different roots. Reject it up front instead.
	if u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("endpoint %q: path-prefixed endpoints are not supported (minio-go cannot carry a URL path); use a bare host:port", endpoint)
	}
	return u.Host, secure, nil
}

// BucketCreateRegion maps the configured signing region onto the location
// minio-go should use when creating a bucket.
//
// minio-go omits the CreateBucketConfiguration/LocationConstraint element
// exactly when the location is "us-east-1". AWS rejects the element for
// us-east-1, and Cloudflare R2 (which uses REGION=auto) rejects it outright, so
// both cases must resolve to "us-east-1" here.
func BucketCreateRegion(region string) string {
	switch region {
	case "", "us-east-1", "auto":
		return "us-east-1"
	default:
		return region
	}
}

// EnsureBucket creates the bucket when the store does not have it yet (typical
// for a bundled MinIO). An existing bucket is left untouched, so the credentials
// do not need s3:CreateBucket in the common case.
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.inner.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("head bucket %q: %w", c.bucket, err)
	}
	if exists {
		return nil
	}

	err = c.inner.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{
		Region: BucketCreateRegion(c.region),
	})
	if err != nil {
		// Concurrent first creates: the loser sees already-exists, which is not
		// a real failure.
		if IsBucketAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create bucket %q: %w", c.bucket, err)
	}
	return nil
}

// CreateVolumeDir PUTs the trailing-slash directory object s3fs uses for mkdir
// (key "volumes/<volumeID>/").
//
// Object storage has no real directories; s3fs 1.91+ stats this key when
// mounting bucket:/volumes/<volumeID>. A sibling ".keep" file is a different key
// and does not satisfy that stat.
func (c *Client) CreateVolumeDir(ctx context.Context, volumeID string) error {
	key := config.VolumePrefix(volumeID)
	_, err := c.inner.PutObject(ctx, c.bucket, key, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// RemoveVolumeDir recursively deletes every object under volumes/<volumeID>/.
//
// Only an explicitly missing bucket or key is ignored; every other failure must
// propagate so CubeMaster does not drop the volume record while objects remain.
func (c *Client) RemoveVolumeDir(ctx context.Context, volumeID string) error {
	prefix := config.VolumePrefix(volumeID)

	listCtx, cancelList := context.WithCancel(ctx)
	defer cancelList()

	objects := c.inner.ListObjects(listCtx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	// ListObjects reports failures through the channel rather than a return
	// value, so funnel entries into RemoveObjects and report a listing failure
	// once the delete stream has drained.
	listErrCh := make(chan error, 1)
	keys := make(chan minio.ObjectInfo)
	go func() {
		defer close(keys)
		for obj := range objects {
			if obj.Err != nil {
				if !IsNotFound(obj.Err) {
					listErrCh <- obj.Err
				}
				return
			}
			select {
			case keys <- obj:
			case <-listCtx.Done():
				return
			}
		}
	}()

	// Drain the whole result stream even after the first failure: abandoning it
	// would leave the producer goroutines blocked on an unread channel.
	var delErr error
	for rmErr := range c.inner.RemoveObjects(ctx, c.bucket, keys, minio.RemoveObjectsOptions{}) {
		if rmErr.Err == nil || IsNotFound(rmErr.Err) || delErr != nil {
			continue
		}
		delErr = fmt.Errorf("delete object %q under %q: %w", rmErr.ObjectName, prefix, rmErr.Err)
		cancelList()
	}
	if delErr != nil {
		return delErr
	}

	select {
	case listErr := <-listErrCh:
		return fmt.Errorf("list prefix %q: %w", prefix, listErr)
	default:
		return nil
	}
}

// IsBucketAlreadyExists reports whether err means the bucket is already there.
func IsBucketAlreadyExists(err error) bool {
	switch minio.ToErrorResponse(err).Code {
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return true
	default:
		return false
	}
}

// IsNotFound reports whether err means the bucket or key does not exist, which
// destroy treats as success (the prefix is already gone).
func IsNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchBucket", "NoSuchKey", "NotFound":
		return true
	}
	return resp.StatusCode == 404
}
