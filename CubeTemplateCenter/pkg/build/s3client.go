// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
)

// SharedS3Client returns the process-wide S3 client, initializing it lazily on
// first call. Both the build path (upload) and the artifact deleter (delete)
// share this instance so S3 config is parsed exactly once and credentials are
// not duplicated across call sites.
//
// Returns (nil, false) when S3 is not configured or the client cannot be
// constructed. Absent or incomplete S3 credentials retain local-disk behavior.
// Build rejects cached initialization errors from NewClient or AnnounceIfFS;
// those failures do not fall back to local disk.
func SharedS3Client() (*s3store.Client, bool) {
	return sharedS3.instance()
}

var sharedS3 s3ClientFactory

type s3ClientFactory struct {
	once    sync.Once
	client  *s3store.Client
	enabled bool
	initErr error
}

func (f *s3ClientFactory) instance() (*s3store.Client, bool) {
	f.once.Do(f.init)
	return f.client, f.enabled
}

func (f *s3ClientFactory) init() {
	backend := configenv.ArtifactStoreBackend()
	s3 := configenv.ParseArtifactS3()
	cfg := s3store.Config{
		Driver:         backend,
		Endpoint:       s3.Endpoint,
		Bucket:         s3.Bucket,
		AccessKey:      s3.AccessKey,
		SecretKey:      s3.SecretKey,
		Region:         s3.Region,
		UsePathStyle:   s3.UsePathStyle,
		UseSSL:         s3.UseSSL,
		ArtifactPrefix: configenv.EnvOr(configenv.EnvS3ArtifactPrefix),
		FSRoot:         configenv.ResolveArtifactFSRoot(),
	}
	if err := blobstore.AnnounceIfFS(cfg.FSRoot, backend); err != nil {
		f.initErr = fmt.Errorf("configured artifact store (backend=%s) failed to open: %w", backend, err)
		slog.Error("artifact store initialization failed; template builds will be rejected", "backend", backend, "error", err)
		return
	}
	if backend != "fs" && !s3.Enabled {
		slog.Warn("storage backend degraded",
			"requested", "s3", "effective", "local-disk",
			"reason", "incomplete CUBE_S3_* credentials")
		return
	}
	client, err := s3store.NewClient(cfg)
	if err != nil {
		f.initErr = fmt.Errorf("configured artifact store (backend=%s) failed to open: %w", backend, err)
		slog.Error("artifact store initialization failed; template builds will be rejected", "backend", backend, "error", err)
		return
	}
	f.client = client
	f.enabled = true
	if backend == "fs" {
		slog.Info("storage backend selected", "backend", "fs", "reason", "explicit", "root", cfg.FSRoot)
		return
	}
	slog.Info("storage backend selected", "backend", "s3", "reason", "explicit")
}
