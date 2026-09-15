// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package s3 stores blobs in an S3-compatible bucket via minio-go.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

func init() {
	blobstore.Register(&driver{})
}

const (
	DriverName = "s3"

	// PutPartSize is the multipart part size. Must be explicit: minio-go
	// otherwise buffers 512MiB per concurrent unknown-length upload.
	PutPartSize = 64 << 20

	metaSHA256Key = "sha256"

	extraEndpoint      = "endpoint"
	extraNodeEndpoint  = "node_endpoint"
	extraAccessKey     = "access_key_id"
	extraSecretKey     = "secret_access_key"
	extraRegion        = "region"
	extraPathStyle     = "path_style"
	extraUseSSL        = "use_ssl"
	extraCreateBucket  = "create_bucket"
	extraPutTimeout    = "put_timeout"
	extraPresignExpiry = "presign_expiry"
	extraAttachment    = "attachment_basename"
)

// Options is the typed configuration for the s3 driver.
type Options struct {
	Endpoint        string
	NodeEndpoint    string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Region          string
	PathStyle       bool
	UseSSL          *bool
	CreateBucket    bool
	PutTimeout      time.Duration
	PresignExpiry   time.Duration
	// AttachmentBasename adds response-content-disposition using the key
	// basename when minting a signed GET URL.
	AttachmentBasename bool
}

func (o Options) Config(prefix string) blobstore.Config {
	extra := map[string]string{
		extraEndpoint:      o.Endpoint,
		extraNodeEndpoint:  o.NodeEndpoint,
		extraAccessKey:     o.AccessKeyID,
		extraSecretKey:     o.SecretAccessKey,
		extraRegion:        o.Region,
		extraPathStyle:     strconv.FormatBool(o.PathStyle),
		extraCreateBucket:  strconv.FormatBool(o.CreateBucket),
		extraPutTimeout:    o.PutTimeout.String(),
		extraPresignExpiry: o.PresignExpiry.String(),
		extraAttachment:    strconv.FormatBool(o.AttachmentBasename),
	}
	if o.UseSSL != nil {
		extra[extraUseSSL] = strconv.FormatBool(*o.UseSSL)
	}
	return blobstore.Config{
		Driver:    DriverName,
		Namespace: o.Bucket,
		Prefix:    prefix,
		Extra:     extra,
	}
}

type driver struct{}

func (d *driver) Name() string { return DriverName }

func (d *driver) Open(_ context.Context, cfg blobstore.Config) (blobstore.Store, error) {
	return open(cfg)
}

type store struct {
	client     *minio.Client
	presign    *minio.Client
	core       *minio.Core
	bucket     string
	region     string
	prefix     string
	create     bool
	putTimeout time.Duration
	presignExp time.Duration
	attachment bool
	retention  []blobstore.RetentionRule
}

func open(cfg blobstore.Config) (*store, error) {
	endpoint := extra(cfg, extraEndpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("blobstore/s3: endpoint is required")
	}
	ak := extra(cfg, extraAccessKey)
	sk := extra(cfg, extraSecretKey)
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("blobstore/s3: access_key_id and secret_access_key are required")
	}
	bucket := strings.TrimSpace(cfg.Namespace)
	if bucket == "" {
		return nil, fmt.Errorf("blobstore/s3: bucket (namespace) is required")
	}
	region := extra(cfg, extraRegion)
	if region == "" {
		region = "us-east-1"
	}
	pathStyle := extra(cfg, extraPathStyle) != "false"
	lookup := minio.BucketLookupAuto
	if pathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := newMinioClient(endpoint, ak, sk, region, lookup, extra(cfg, extraUseSSL))
	if err != nil {
		return nil, err
	}
	nodeEP := extra(cfg, extraNodeEndpoint)
	if nodeEP == "" {
		nodeEP = endpoint
	}
	presign, err := newMinioClient(nodeEP, ak, sk, region, lookup, extra(cfg, extraUseSSL))
	if err != nil {
		return nil, fmt.Errorf("blobstore/s3: presign client: %w", err)
	}
	putTO := 30 * time.Minute
	if raw := extra(cfg, extraPutTimeout); raw != "" && raw != "0s" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			putTO = d
		}
	}
	presignExp := 7 * 24 * time.Hour
	if raw := extra(cfg, extraPresignExpiry); raw != "" && raw != "0s" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			presignExp = d
		}
	}
	return &store{
		client:     client,
		presign:    presign,
		core:       &minio.Core{Client: client},
		bucket:     bucket,
		region:     region,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		create:     extra(cfg, extraCreateBucket) == "true",
		putTimeout: putTO,
		presignExp: presignExp,
		attachment: extra(cfg, extraAttachment) == "true",
		retention:  cfg.Retention,
	}, nil
}

func extra(cfg blobstore.Config, k string) string {
	if cfg.Extra == nil {
		return ""
	}
	return cfg.Extra[k]
}

func newMinioClient(endpoint, accessKey, secret, region string, lookup minio.BucketLookupType, useSSL string) (*minio.Client, error) {
	host, secure, err := parseEndpoint(endpoint, useSSL)
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
		return nil, fmt.Errorf("blobstore/s3: client for %q: %w", endpoint, err)
	}
	return c, nil
}

func parseEndpoint(endpoint, useSSL string) (host string, secure bool, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, fmt.Errorf("blobstore/s3: endpoint is empty")
	}
	if !strings.Contains(endpoint, "://") {
		switch strings.ToLower(useSSL) {
		case "1", "true", "yes", "on":
			endpoint = "https://" + endpoint
		case "0", "false", "no", "off":
			endpoint = "http://" + endpoint
		default:
			endpoint = "https://" + endpoint
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("blobstore/s3: parse endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "https":
		secure = true
	case "http":
		secure = false
	default:
		return "", false, fmt.Errorf("blobstore/s3: endpoint %q: unsupported scheme %q", endpoint, u.Scheme)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("blobstore/s3: endpoint %q has no host", endpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("blobstore/s3: endpoint %q: path-prefixed endpoints are not supported", endpoint)
	}
	return u.Host, secure, nil
}

func (s *store) objKey(key string) string {
	return blobstore.JoinKey(s.prefix, key)
}

func (s *store) Capabilities() blobstore.Capabilities {
	return blobstore.Capabilities{
		DirectURL:       true,
		Shared:          true,
		RangeRead:       true,
		ConditionalPut:  true,
		NativeRetention: true,
	}
}

func (s *store) putContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.putTimeout)
}

func (s *store) Put(ctx context.Context, key string, r io.Reader, opts blobstore.PutOptions) (blobstore.ObjectInfo, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	putCtx, cancel := s.putContext(ctx)
	defer cancel()
	full := s.objKey(key)
	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(r, h)}
	ct := opts.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	size := opts.Size
	if size == 0 {
		size = -1
	}
	putOpts := minio.PutObjectOptions{ContentType: ct, PartSize: PutPartSize}
	wantSum := strings.TrimPrefix(opts.SHA256, "sha256:")
	if md := sha256UserMetadata(opts.SHA256); md != nil {
		putOpts.UserMetadata = md
	}
	// If-None-Match on multipart PutObject is weak: minio-go only applies
	// the condition header to the complete call, and some S3 implementations
	// ignore it on MPU. Stat first when the object is large enough (or of
	// unknown size) that PutObject will use multiple parts.
	if opts.IfNotExists && (size < 0 || size > PutPartSize) {
		info, statErr := s.Stat(putCtx, key)
		if statErr == nil {
			return info, blobstore.ErrAlreadyExists
		}
		if !blobstore.IsNotExist(statErr) {
			return blobstore.ObjectInfo{}, statErr
		}
	}
	if opts.IfNotExists {
		putOpts.SetMatchETagExcept("*")
	}
	_, err := s.client.PutObject(putCtx, s.bucket, full, cr, size, putOpts)
	if err != nil {
		if isPreconditionFailed(err) {
			info, statErr := s.Stat(putCtx, key)
			if statErr != nil {
				return blobstore.ObjectInfo{}, blobstore.ErrAlreadyExists
			}
			return info, blobstore.ErrAlreadyExists
		}
		return blobstore.ObjectInfo{}, fmt.Errorf("blobstore/s3: put %q: %w", full, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSum != "" && !strings.EqualFold(wantSum, sum) {
		return blobstore.ObjectInfo{}, fmt.Errorf("blobstore/s3: sha256 mismatch for %q", full)
	}
	// SHA256 UserMetadata is only set on the original PutObject when the
	// caller supplies a digest. CopyObject-to-attach after an empty digest
	// used to rewrite the whole object and is intentionally not done.
	return blobstore.ObjectInfo{Key: key, Size: cr.n, SHA256: sum, ETag: sum, ContentType: ct, LastModified: time.Now()}, nil
}

func sha256UserMetadata(wantSum string) map[string]string {
	wantSum = strings.TrimPrefix(wantSum, "sha256:")
	if wantSum == "" {
		return nil
	}
	return map[string]string{metaSHA256Key: wantSum}
}

func (s *store) Get(ctx context.Context, key string, opts blobstore.GetOptions) (*blobstore.Object, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return nil, err
	}
	full := s.objKey(key)
	getOpts := minio.GetObjectOptions{}
	var cr *blobstore.ByteRange
	if opts.Range != nil {
		start, end := opts.Range.Start, opts.Range.End
		switch {
		case end < 0 && start == 0:
			// Whole object. minio SetRange(0, -1) means "last 1 byte".
		case end < 0 && start > 0:
			if err := getOpts.SetRange(start, 0); err != nil {
				return nil, err
			}
		default:
			if err := getOpts.SetRange(start, end); err != nil {
				return nil, err
			}
		}
		cr = opts.Range
	}
	obj, err := s.client.GetObject(ctx, s.bucket, full, getOpts)
	if err != nil {
		return nil, fmt.Errorf("blobstore/s3: get %q: %w", full, err)
	}
	st, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isS3NotFound(err) {
			return nil, blobstore.ErrNotExist
		}
		return nil, fmt.Errorf("blobstore/s3: stat %q: %w", full, err)
	}
	return &blobstore.Object{
		ObjectInfo:   objectInfo(key, st),
		Body:         obj,
		ContentRange: cr,
	}, nil
}

func (s *store) Stat(ctx context.Context, key string) (blobstore.ObjectInfo, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return blobstore.ObjectInfo{}, err
	}
	full := s.objKey(key)
	info, err := s.client.StatObject(ctx, s.bucket, full, minio.StatObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return blobstore.ObjectInfo{}, blobstore.ErrNotExist
		}
		return blobstore.ObjectInfo{}, fmt.Errorf("blobstore/s3: stat %q: %w", full, err)
	}
	return objectInfo(key, info), nil
}

func (s *store) Delete(ctx context.Context, key string) error {
	if err := blobstore.ValidateKey(key); err != nil {
		return err
	}
	full := s.objKey(key)
	err := s.client.RemoveObject(ctx, s.bucket, full, minio.RemoveObjectOptions{})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("blobstore/s3: delete %q: %w", full, err)
	}
	return nil
}

func (s *store) List(ctx context.Context, prefix string, fn func(blobstore.ObjectInfo) error) error {
	if err := blobstore.ValidatePrefix(prefix); err != nil {
		return err
	}
	fullPrefix := s.objKey(prefix)
	if prefix == "" {
		fullPrefix = s.prefix
		if fullPrefix != "" {
			fullPrefix += "/"
		}
	}
	prefixSlash := ""
	if s.prefix != "" {
		prefixSlash = s.prefix + "/"
	}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    fullPrefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("blobstore/s3: list %q: %w", fullPrefix, obj.Err)
		}
		userKey := obj.Key
		if prefixSlash != "" {
			userKey = strings.TrimPrefix(obj.Key, prefixSlash)
		}
		if err := fn(objectInfo(userKey, obj)); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := blobstore.ValidateKey(key); err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = s.presignExp
	}
	full := s.objKey(key)
	params := make(url.Values)
	if s.attachment {
		params.Set("response-content-disposition", fmt.Sprintf("attachment; filename=%q", blobstore.BaseName(key)))
	}
	u, err := s.presign.PresignedGetObject(ctx, s.bucket, full, ttl, params)
	if err != nil {
		return "", fmt.Errorf("blobstore/s3: presign %q: %w", full, err)
	}
	return u.String(), nil
}

func (s *store) Prepare(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		if isAccessDenied(err) {
			return nil
		}
		return fmt.Errorf("blobstore/s3: head bucket %q: %w", s.bucket, err)
	}
	if !exists {
		if !s.create {
			return fmt.Errorf("blobstore/s3: bucket %q does not exist", s.bucket)
		}
		err = s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: bucketCreateRegion(s.region)})
		if err != nil && !isBucketAlreadyExists(err) && !isAccessDenied(err) {
			return fmt.Errorf("blobstore/s3: create bucket %q: %w", s.bucket, err)
		}
	}
	return s.ensureLifecycle(ctx)
}

func (s *store) ensureLifecycle(ctx context.Context) error {
	rules := s.lifecycleRules()
	if len(rules) == 0 {
		return nil
	}
	if err := s.setLifecycle(ctx, rules); err != nil {
		if !isLifecycleUnsupported(err) {
			return fmt.Errorf("blobstore/s3: set lifecycle: %w", err)
		}
		slog.Warn("blobstore/s3: abort-incomplete lifecycle unsupported; applying expire rules only", "error", err)
		var expire []lifecycle.Rule
		for _, r := range rules {
			if r.Expiration.Days > 0 {
				expire = append(expire, lifecycle.Rule{
					ID:         r.ID,
					Status:     r.Status,
					RuleFilter: r.RuleFilter,
					Expiration: r.Expiration,
				})
			}
		}
		if len(expire) == 0 {
			return nil
		}
		if err := s.setLifecycle(ctx, expire); err != nil {
			return fmt.Errorf("blobstore/s3: set lifecycle: %w", err)
		}
	}
	return nil
}

func (s *store) lifecycleRules() []lifecycle.Rule {
	var rules []lifecycle.Rule
	for i, r := range s.retention {
		id := fmt.Sprintf("blobstore-%d", i)
		prefix := blobstore.JoinKey(s.prefix, r.Prefix)
		rule := lifecycle.Rule{ID: id, Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: prefix}}
		has := false
		if r.ExpireAfter > 0 {
			days := int(r.ExpireAfter / (24 * time.Hour))
			if days < 1 {
				days = 1
			}
			rule.Expiration = lifecycle.Expiration{Days: lifecycle.ExpirationDays(days)}
			has = true
		}
		if r.AbortIncompleteAfter > 0 {
			days := int(r.AbortIncompleteAfter / (24 * time.Hour))
			if days < 1 {
				days = 1
			}
			rule.AbortIncompleteMultipartUpload = lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: lifecycle.ExpirationDays(days)}
			has = true
		}
		if has {
			rules = append(rules, rule)
		}
	}
	return rules
}

func (s *store) setLifecycle(ctx context.Context, rules []lifecycle.Rule) error {
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = rules
	return s.client.SetBucketLifecycle(ctx, s.bucket, cfg)
}

func (s *store) GC(ctx context.Context, opts blobstore.GCOptions) (blobstore.GCResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var result blobstore.GCResult
	prefix := s.objKey(opts.Prefix)
	if opts.Prefix == "" {
		prefix = s.prefix
	}
	ttl := 24 * time.Hour
	for _, r := range s.retention {
		if r.AbortIncompleteAfter > 0 {
			ttl = r.AbortIncompleteAfter
			break
		}
	}
	for u := range s.client.ListIncompleteUploads(ctx, s.bucket, prefix, true) {
		if u.Err != nil {
			return result, fmt.Errorf("blobstore/s3: list incomplete: %w", u.Err)
		}
		if now.Sub(u.Initiated) < ttl {
			continue
		}
		if err := s.core.AbortMultipartUpload(ctx, s.bucket, u.Key, u.UploadID); err != nil && !isS3NotFound(err) {
			slog.Warn("blobstore/s3: abort multipart", "key", u.Key, "error", err)
			continue
		}
		result.IncompleteAborted++
	}
	return result, nil
}

func (s *store) Close() error { return nil }

func objectInfo(key string, info minio.ObjectInfo) blobstore.ObjectInfo {
	sum := objectSHA256(info)
	return blobstore.ObjectInfo{
		Key:          key,
		Size:         info.Size,
		SHA256:       sum,
		ETag:         strings.Trim(info.ETag, `"`),
		ContentType:  info.ContentType,
		LastModified: info.LastModified,
	}
}

func objectSHA256(info minio.ObjectInfo) string {
	if info.UserMetadata != nil {
		for k, v := range info.UserMetadata {
			if strings.EqualFold(k, metaSHA256Key) || strings.EqualFold(k, "X-Amz-Meta-Sha256") || strings.EqualFold(k, "Sha256") {
				if v = strings.TrimSpace(v); v != "" {
					return strings.TrimPrefix(v, "sha256:")
				}
			}
		}
	}
	if info.Metadata != nil {
		return strings.TrimPrefix(strings.TrimSpace(info.Metadata.Get("X-Amz-Meta-Sha256")), "sha256:")
	}
	return ""
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

func isLifecycleUnsupported(err error) bool {
	switch minio.ToErrorResponse(err).Code {
	case "MalformedXML", "InvalidRequest", "InvalidArgument":
		return true
	default:
		return false
	}
}

func bucketCreateRegion(region string) string {
	switch region {
	case "", "us-east-1", "auto":
		return "us-east-1"
	default:
		return region
	}
}
