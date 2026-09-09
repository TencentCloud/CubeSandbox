// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// SharedS3Client returns the process-wide S3 client, initializing it lazily on
// first call. Both the build path (upload) and the artifact deleter (delete)
// share this instance so S3 config is parsed exactly once and credentials are
// not duplicated across call sites.
//
// Returns (nil, false) when S3 is not configured or the client cannot be
// constructed — callers fall back to local-disk behavior in that case.
func SharedS3Client() (*s3store.Client, bool) {
	return sharedS3.instance()
}

// WarnIfS3Unreachable forces the shared client initialization and probes the
// artifact bucket at process startup. Without this check a misconfigured or
// unreachable store (wrong CUBE_S3_USE_PATH_STYLE for the endpoint, bad
// credentials, network partition) only surfaces as a one-line Warn at the
// first failed upload — after which the artifact silently lands on
// node-local disk and multi-replica artifact downloads 404. The WARN is
// deliberately loud about the consequence; it does not abort startup
// because a single-replica deployment still works in the degraded mode.
func WarnIfS3Unreachable(ctx context.Context) {
	enabled, endpoint, bucket, _, _, _, _, _, _ := tcconfig.S3Config()
	if !enabled {
		return
	}
	client, ok := SharedS3Client()
	if !ok {
		CubeLog.WithContext(ctx).Errorf(
			"CUBE_S3_* is configured (endpoint=%s bucket=%s) but the artifact S3 client could not be built; "+
				"artifact uploads will fall back to NODE-LOCAL storage — artifact downloads will 404 on any pod that did not build the artifact",
			endpoint, bucket)
		return
	}
	if err := client.Probe(ctx); err != nil {
		CubeLog.WithContext(ctx).Errorf(
			"CUBE_S3_* is configured (endpoint=%s bucket=%s) but the artifact bucket is unreachable: %v; "+
				"artifact uploads will fall back to NODE-LOCAL storage — artifact downloads will 404 on any pod that did not build the artifact; "+
				"check CUBE_S3_USE_PATH_STYLE (external MinIO/Ceph needs path-style), credentials and network",
			endpoint, bucket, err)
	}
}

var sharedS3 s3ClientFactory

type s3ClientFactory struct {
	once    sync.Once
	client  *s3store.Client
	enabled bool
}

func (f *s3ClientFactory) instance() (*s3store.Client, bool) {
	f.once.Do(func() {
		enabled, endpoint, bucket, accessKey, secretKey, region, usePathStyle, useSSL, artifactPrefix := tcconfig.S3Config()
		if !enabled {
			return
		}
		client, err := s3store.NewClient(s3store.Config{
			Endpoint:       endpoint,
			Bucket:         bucket,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			Region:         region,
			UsePathStyle:   usePathStyle,
			UseSSL:         useSSL,
			ArtifactPrefix: artifactPrefix,
		})
		if err != nil {
			return
		}
		f.client = client
		f.enabled = true
	})
	return f.client, f.enabled
}
