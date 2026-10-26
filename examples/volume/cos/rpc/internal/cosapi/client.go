// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package cosapi implements Controller Hook create/destroy via the COS Go SDK.
// See https://cloud.tencent.com/document/product/436/31215
package cosapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/tencentcloud/CubeSandbox/examples/volume/cos/rpc/internal/config"
	"github.com/tencentyun/cos-go-sdk-v5"
)

// Client wraps cos-go-sdk-v5 for volume lifecycle operations.
type Client struct {
	bucket string
	inner  *cos.Client
}

// New builds a COS client from plugin config.
func New(cfg *config.Config) (*Client, error) {
	bucketURL, err := url.Parse(fmt.Sprintf(
		"https://%s.cos.%s.myqcloud.com", cfg.Bucket, cfg.Region,
	))
	if err != nil {
		return nil, err
	}
	c := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  cfg.SecretID,
			SecretKey: cfg.SecretKey,
		},
	})
	return &Client{bucket: cfg.Bucket, inner: c}, nil
}

// CreateVolume uploads a zero-byte .keep object to provision the volume prefix.
func (c *Client) CreateVolume(ctx context.Context, volumeID string) error {
	key := config.VolumePrefix(volumeID) + ".keep"
	_, err := c.inner.Object.Put(ctx, key, strings.NewReader(""), nil)
	if err != nil {
		return fmt.Errorf("cos put %q: %w", key, err)
	}
	return nil
}

// DestroyVolume deletes all objects under volumes/<volumeID>/.
func (c *Client) DestroyVolume(ctx context.Context, volumeID string) error {
	prefix := config.VolumePrefix(volumeID)
	marker := ""
	for {
		opt := &cos.BucketGetOptions{
			Prefix:  prefix,
			Marker:  marker,
			MaxKeys: 1000,
		}
		result, _, err := c.inner.Bucket.Get(ctx, opt)
		if err != nil {
			return fmt.Errorf("cos list prefix %q: %w", prefix, err)
		}
		if len(result.Contents) == 0 {
			return nil
		}

		objects := make([]cos.Object, 0, len(result.Contents))
		for _, item := range result.Contents {
			objects = append(objects, cos.Object{Key: item.Key})
		}
		_, _, err = c.inner.Object.DeleteMulti(ctx, &cos.ObjectDeleteMultiOptions{
			Objects: objects,
			Quiet:   true,
		})
		if err != nil {
			return fmt.Errorf("cos delete multi under %q: %w", prefix, err)
		}
		if !result.IsTruncated {
			return nil
		}
		marker = result.NextMarker
	}
}
