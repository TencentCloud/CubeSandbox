// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
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
