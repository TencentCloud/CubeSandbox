// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

// S3BlobStore stores warehouse blobs in an S3-compatible bucket.
type S3BlobStore struct {
	client     *minio.Client
	presign    *minio.Client
	core       *minio.Core
	bucket     string
	region     string
	create     bool
	putTimeout time.Duration
}

func NewS3BlobStore(cfg config.S3Config, putTimeout time.Duration) (*S3BlobStore, error) {
	if strings.TrimSpace(cfg.Region) == "" {
		cfg.Region = "us-east-1"
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		cfg.Bucket = config.DefaultS3Bucket
	}
	if putTimeout <= 0 {
		putTimeout = 30 * time.Minute
	}

	lookup := minio.BucketLookupPath
	if !cfg.UsePathStyle() {
		lookup = minio.BucketLookupAuto
	}

	client, err := newMinioClient(cfg.Endpoint, cfg.AccessKeyID, cfg.SecretAccessKey, cfg.Region, lookup)
	if err != nil {
		return nil, err
	}
	nodeEndpoint := strings.TrimSpace(cfg.NodeEndpoint)
	if nodeEndpoint == "" {
		nodeEndpoint = cfg.Endpoint
	}
	presign, err := newMinioClient(nodeEndpoint, cfg.AccessKeyID, cfg.SecretAccessKey, cfg.Region, lookup)
	if err != nil {
		return nil, fmt.Errorf("presign client: %w", err)
	}
	return &S3BlobStore{
		client:     client,
		presign:    presign,
		core:       &minio.Core{Client: client},
		bucket:     cfg.Bucket,
		region:     cfg.Region,
		create:     cfg.ShouldCreateBucket(),
		putTimeout: putTimeout,
	}, nil
}

func newMinioClient(endpoint, accessKey, secret, region string, lookup minio.BucketLookupType) (*minio.Client, error) {
	host, secure, err := parseS3Endpoint(endpoint)
	if err != nil {
		return nil, err
	}
	c, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secret, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client for %q: %w", endpoint, err)
	}
	return c, nil
}

func parseS3Endpoint(endpoint string) (host string, secure bool, err error) {
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
	if u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("endpoint %q: path-prefixed endpoints are not supported", endpoint)
	}
	return u.Host, secure, nil
}

func (s *S3BlobStore) putContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.putTimeout)
}

func (s *S3BlobStore) Put(ctx context.Context, key string, r io.Reader, contentType string) (ObjectInfo, error) {
	putCtx, cancel := s.putContext(ctx)
	defer cancel()

	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(r, h)}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opts := minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    PutPartSize,
	}
	if strings.HasPrefix(key, blobsPrefix) {
		opts.SetMatchETagExcept("*")
	}
	_, err := s.client.PutObject(putCtx, s.bucket, key, cr, -1, opts)
	if err != nil {
		if isPreconditionFailed(err) {
			return s.Stat(putCtx, key)
		}
		return ObjectInfo{}, fmt.Errorf("put %q: %w", key, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.attachSHA256(putCtx, key, sum)
	return s.statAfterPut(putCtx, key, cr.n, sum), nil
}

func (s *S3BlobStore) statAfterPut(ctx context.Context, key string, size int64, sum string) ObjectInfo {
	info, err := s.Stat(ctx, key)
	if err != nil {
		return ObjectInfo{Key: key, Size: size, SHA256: sum}
	}
	if info.SHA256 == "" {
		info.SHA256 = sum
	}
	if info.Size == 0 {
		info.Size = size
	}
	return info
}

func (s *S3BlobStore) attachSHA256(ctx context.Context, key, sum string) {
	src := minio.CopySrcOptions{Bucket: s.bucket, Object: key}
	dst := minio.CopyDestOptions{
		Bucket:          s.bucket,
		Object:          key,
		UserMetadata:    map[string]string{metaSHA256Key: sum},
		ReplaceMetadata: true,
	}
	if _, err := s.client.CopyObject(ctx, dst, src); err != nil {
		slog.Warn("warehouse failed to attach sha256 metadata", "key", key, "error", err)
	}
}

func (s *S3BlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", key, err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isS3NotFound(err) {
			return nil, objectNotFoundError{key: key}
		}
		return nil, fmt.Errorf("stat %q: %w", key, err)
	}
	return obj, nil
}

func (s *S3BlobStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return ObjectInfo{}, objectNotFoundError{key: key}
		}
		return ObjectInfo{}, fmt.Errorf("stat %q: %w", key, err)
	}
	return ObjectInfo{
		Key:          key,
		Size:         info.Size,
		SHA256:       objectSHA256(info),
		LastModified: info.LastModified,
	}, nil
}

func (s *S3BlobStore) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

func (s *S3BlobStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	u, err := s.presign.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("presign %q: %w", key, err)
	}
	return u.String(), nil
}

func (s *S3BlobStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list %q: %w", prefix, obj.Err)
		}
		out = append(out, ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			SHA256:       objectSHA256(obj),
		})
	}
	return out, nil
}

func (s *S3BlobStore) ListIncompleteUploads(ctx context.Context, prefix string) ([]IncompleteUpload, error) {
	var out []IncompleteUpload
	for u := range s.client.ListIncompleteUploads(ctx, s.bucket, prefix, true) {
		if u.Err != nil {
			return nil, fmt.Errorf("list incomplete uploads %q: %w", prefix, u.Err)
		}
		out = append(out, IncompleteUpload{
			Key:       u.Key,
			UploadID:  u.UploadID,
			Initiated: u.Initiated,
		})
	}
	return out, nil
}

func (s *S3BlobStore) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	err := s.core.AbortMultipartUpload(ctx, s.bucket, key, uploadID)
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("abort multipart %q %s: %w", key, uploadID, err)
	}
	return nil
}

func (s *S3BlobStore) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		if isAccessDenied(err) {
			return nil
		}
		return fmt.Errorf("head bucket %q: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if !s.create {
		return fmt.Errorf("bucket %q does not exist", s.bucket)
	}
	err = s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: bucketCreateRegion(s.region)})
	if err != nil {
		if isBucketAlreadyExists(err) || isAccessDenied(err) {
			return nil
		}
		return fmt.Errorf("create bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *S3BlobStore) expireUploadsRules() []lifecycle.Rule {
	return []lifecycle.Rule{
		{
			ID:         "warehouse-expire-uploads",
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: uploadsPrefix},
			Expiration: lifecycle.Expiration{Days: 1},
		},
	}
}

func (s *S3BlobStore) lifecycleRules() []lifecycle.Rule {
	abort := lifecycle.Rule{
		ID:                             "warehouse-abort-incomplete-mpu",
		Status:                         "Enabled",
		RuleFilter:                     lifecycle.Filter{Prefix: Prefix},
		AbortIncompleteMultipartUpload: lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: 1},
	}
	return append([]lifecycle.Rule{abort}, s.expireUploadsRules()...)
}

func (s *S3BlobStore) setLifecycle(ctx context.Context, rules []lifecycle.Rule) error {
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = rules
	return s.client.SetBucketLifecycle(ctx, s.bucket, cfg)
}

func (s *S3BlobStore) EnsureLifecycle(ctx context.Context) error {
	err := s.setLifecycle(ctx, s.lifecycleRules())
	if err == nil {
		return nil
	}
	if !isLifecycleUnsupported(err) {
		return fmt.Errorf("set bucket lifecycle: %w", err)
	}
	slog.Warn("warehouse abort-incomplete lifecycle unsupported; applying expire-uploads only",
		"error", err)
	if err := s.setLifecycle(ctx, s.expireUploadsRules()); err != nil {
		return fmt.Errorf("set bucket lifecycle: %w", err)
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func objectSHA256(info minio.ObjectInfo) string {
	if info.UserMetadata != nil {
		for _, k := range []string{metaSHA256Key, "X-Amz-Meta-Sha256", "Sha256"} {
			if v := strings.TrimSpace(info.UserMetadata[k]); v != "" {
				return strings.TrimPrefix(v, "sha256:")
			}
		}
		for k, v := range info.UserMetadata {
			if strings.EqualFold(k, metaSHA256Key) || strings.EqualFold(k, "X-Amz-Meta-Sha256") {
				return normalizeSHA256(v)
			}
		}
	}
	if info.Metadata != nil {
		return normalizeSHA256(info.Metadata.Get("X-Amz-Meta-Sha256"))
	}
	return ""
}

func normalizeSHA256(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "sha256:")
}

func isLifecycleUnsupported(err error) bool {
	switch minio.ToErrorResponse(err).Code {
	case "MalformedXML", "InvalidRequest", "InvalidArgument":
		return true
	default:
		return false
	}
}

func isS3NotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchBucket", "NoSuchKey", "NotFound", "NoSuchUpload":
		return true
	}
	return resp.StatusCode == http.StatusNotFound
}

func isAccessDenied(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "AccessDenied" || resp.StatusCode == http.StatusForbidden
}

func isBucketAlreadyExists(err error) bool {
	switch minio.ToErrorResponse(err).Code {
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return true
	default:
		return false
	}
}

func isPreconditionFailed(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "PreconditionFailed" || resp.StatusCode == http.StatusPreconditionFailed
}

func bucketCreateRegion(region string) string {
	switch region {
	case "", "us-east-1", "auto":
		return "us-east-1"
	default:
		return region
	}
}
