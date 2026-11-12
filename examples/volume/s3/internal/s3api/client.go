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
	"time"

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

// Options selects the bucket and credentials for one client. Dynamic volumes
// build their own credentials from a credential reference; legacy volumes use
// the plugin configuration.
type Options struct {
	Bucket          string
	Endpoint        string
	Region          string
	Addressing      config.Addressing
	AccessKeyID     string
	SecretAccessKey string
	InstanceRole    bool
}

// imdsEndpoint is where the instance-role provider asks for credentials;
// empty means the EC2 default. Tests point it at a fake IMDS.
var imdsEndpoint = ""

// imdsProbeTimeout bounds the startup credential fetch. minio-go's default
// HTTP client has no timeout of its own, so without this a host that cannot
// reach IMDS would hang instead of failing.
const imdsProbeTimeout = 5 * time.Second

// credentialsFor decides where the credentials come from: the configured
// static keys, or the EC2 instance role with CREDENTIALS=instance_role. The
// latter is minio-go's IAM provider, not the full AWS credential chain:
// AWS_ACCESS_KEY_ID and ~/.aws/credentials are ignored, so a node with stray
// keys cannot act as the wrong identity. Kept apart from New so the choice can
// be tested without a network.
func credentialsFor(cfg *config.Config) *credentials.Credentials {
	if cfg.UseInstanceRole() {
		return credentials.NewIAM(imdsEndpoint)
	}
	return credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, "")
}

// CheckInstanceRole fetches credentials once when cfg uses the instance role,
// so a host that is not on EC2, has IMDS disabled or has no role attached
// fails with a clear error up front instead of as an s3fs mount or S3 request
// failure later. It is a no-op with static keys.
func CheckInstanceRole(ctx context.Context, cfg *config.Config) error {
	if !cfg.UseInstanceRole() {
		return nil
	}
	return probeInstanceRole(ctx, credentialsFor(cfg))
}

// probeInstanceRole resolves creds within imdsProbeTimeout. On success the
// value stays cached in creds, so a client built on them does not ask again.
func probeInstanceRole(ctx context.Context, creds *credentials.Credentials) error {
	ctx, cancel := context.WithTimeout(ctx, imdsProbeTimeout)
	defer cancel()
	if _, err := creds.GetWithContext(&credentials.CredContext{Context: ctx}); err != nil {
		return fmt.Errorf("CREDENTIALS=%s: no credentials from the EC2 instance metadata service "+
			"(is this host on EC2, with IMDS enabled and an instance role attached?): %w",
			config.CredentialsInstanceRole, err)
	}
	return nil
}

// New builds an S3 client from plugin config. With the instance role it
// fetches credentials before returning; see CheckInstanceRole.
func New(ctx context.Context, cfg *config.Config) (*Client, error) {
	return NewFor(ctx, Options{
		Bucket:          cfg.Bucket,
		Endpoint:        cfg.Endpoint,
		Region:          cfg.Region,
		Addressing:      cfg.Addressing,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		InstanceRole:    cfg.UseInstanceRole(),
	})
}

// NewFor builds an S3 client for one bucket and credential set.
func NewFor(ctx context.Context, opts Options) (*Client, error) {
	if opts.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	host, secure, err := ParseEndpoint(opts.Endpoint)
	if err != nil {
		return nil, err
	}

	// minio-go's false value in the old options mapped to Auto; virtual must be
	// explicit (BucketLookupDNS), otherwise an endpoint that also supports
	// path-style could still be addressed the wrong way.
	lookup := minio.BucketLookupAuto
	switch opts.Addressing {
	case config.AddressingPath:
		lookup = minio.BucketLookupPath
	case config.AddressingVirtual:
		lookup = minio.BucketLookupDNS
	}

	var creds *credentials.Credentials
	if opts.InstanceRole {
		creds = credentials.NewIAM(imdsEndpoint)
		if err := probeInstanceRole(ctx, creds); err != nil {
			return nil, err
		}
	} else {
		if opts.AccessKeyID == "" || opts.SecretAccessKey == "" {
			return nil, fmt.Errorf("s3: access key and secret are required for bucket %q", opts.Bucket)
		}
		creds = credentials.NewStaticV4(opts.AccessKeyID, opts.SecretAccessKey, "")
	}

	inner, err := minio.New(host, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       opts.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client for %q: %w", opts.Endpoint, err)
	}
	return &Client{bucket: opts.Bucket, region: opts.Region, inner: inner}, nil
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

// BucketExists reports whether the bucket exists. Dynamic volumes use an
// existing user bucket, so create fails clearly instead of creating one by
// mistake when the name is wrong.
func (c *Client) BucketExists(ctx context.Context) (bool, error) {
	exists, err := c.inner.BucketExists(ctx, c.bucket)
	if err != nil {
		return false, fmt.Errorf("head bucket %q: %w", c.bucket, err)
	}
	return exists, nil
}

// EnsureBucket creates the bucket when the store does not have it yet (typical
// for a bundled MinIO). An existing bucket is left untouched, so the credentials
// do not need s3:CreateBucket in the common case. Only legacy volumes call
// this; dynamic buckets are user-owned and must already exist.
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

// CreatePrefix PUTs the trailing-slash directory object s3fs uses for mkdir.
//
// Object storage has no real directories; s3fs 1.91+ stats this key when
// mounting bucket:/<prefix>. A sibling ".keep" file is a different key and does
// not satisfy that stat.
func (c *Client) CreatePrefix(ctx context.Context, prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	if _, err := c.inner.PutObject(ctx, c.bucket, prefix, bytes.NewReader(nil), 0, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("put object %q: %w", prefix, err)
	}
	return nil
}

// PrefixHasData reports whether any object other than the empty directory
// marker itself exists under prefix.
//
// Dynamic create accepts only an empty prefix. CubeMaster can call Destroy with
// the binding from a create whose database write then failed; that rollback
// recursively deletes the prefix. Adopting objects that already lived there
// would turn that compensating delete into data loss, so create refuses a
// non-empty prefix instead. The zero-byte "prefix/" object that an earlier
// failed attempt left behind is not data and does not block the retry.
func (c *Client) PrefixHasData(ctx context.Context, prefix string) (bool, error) {
	if err := checkPrefix(prefix); err != nil {
		return false, err
	}

	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	objects := c.inner.ListObjects(listCtx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})
	for obj := range objects {
		if obj.Err != nil {
			if IsNotFound(obj.Err) {
				return false, nil
			}
			return false, fmt.Errorf("list prefix %q: %w", prefix, obj.Err)
		}
		if obj.Key == prefix && obj.Size == 0 {
			continue // the directory object Create itself writes
		}
		return true, nil
	}
	return false, nil
}

// RemovePrefix recursively deletes every object under prefix.
//
// Only an explicitly missing bucket or key is ignored; every other failure must
// propagate so CubeMaster does not drop the volume record while objects remain.
func (c *Client) RemovePrefix(ctx context.Context, prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}

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

// checkPrefix is the last line of defence before a recursive delete: only a
// non-empty key ending in "/" is accepted, so a malformed binding can never
// address the whole bucket.
func checkPrefix(prefix string) error {
	if prefix == "" || prefix == "/" {
		return fmt.Errorf("prefix is empty")
	}
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("prefix %q must end with /", prefix)
	}
	if strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("prefix %q must be relative", prefix)
	}
	return nil
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
