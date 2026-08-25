// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cow declares the Cubelet CoW object-store abstraction.
//
// [Store] is the base interface; concrete backends are subclasses:
//   - [NameXfsCow] ("xfscow"): local XFS/reflink via cubecow
//   - [NameS3] ("s3"): S3 scheme Store (cubecow kind=s3; export/fetch/status
//     for cross-node)
//
// XFS and S3 Stores coexist in one Cubelet process. Callers pick a backend via
// request `type` / [NormalizeBackend] (see storage.StoreFor). Legacy helpers
// that omit a backend keep defaulting to XFS.
package cow

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
)

// Backend names. Config still uses storage_backend=cubecow for the XFS path;
// Store.Name() reports the logical subclass name (xfscow / s3).
const (
	NameXfsCow = "xfscow"
	NameS3     = "s3"
)

// Object kinds match cubecow volume vs snapshot objects.
const (
	KindVolume   = "volume"
	KindSnapshot = "snapshot"
)

// Object is a named CoW volume or snapshot with an optional device path.
type Object struct {
	VolumeName string
	Kind       string
	Gen        uint32
	FilePath   string
}

// Store is the CoW object storage abstraction (base class).
// Concrete backends (xfscow today, s3 later) encapsulate create/clone/delete
// and device-path resolution behind these methods.
type Store interface {
	// Name returns the backend subclass id (NameXfsCow / NameS3).
	Name() string

	CreateDefaultMediumVolume(ctx context.Context, sandboxID, volumeName string, sizeBytes uint64) (*Object, error)
	CreateSandboxRootfsFromTemplate(ctx context.Context, sandboxID, templateID string, gen uint32, desiredSizeBytes uint64) (*Object, error)
	RollbackDeriveNewGen(ctx context.Context, sandboxID, snapshotRootfsVol string, gen uint32, desiredSizeBytes uint64) (*Object, error)
	CreateTemplateBuildRootfs(ctx context.Context, templateID string, sizeBytes uint64) (*Object, error)
	CommitTemplateRootfs(ctx context.Context, sourceName, templateID string) (*Object, error)
	CreateMemoryVolume(ctx context.Context, templateID string, sizeBytes uint64) (*Object, error)
	CommitTemplateMemory(ctx context.Context, sourceName, templateID string, sizeBytes uint64) (*Object, error)
	// CloneSandboxMemory copies package memory into sb-<id>-memory
	// so Resume can drop the pause package afterwards.
	CloneSandboxMemory(ctx context.Context, sandboxID, sourceName string) (*Object, error)
	DeleteByKind(ctx context.Context, name, kind string) error
	DeactivateByKind(ctx context.Context, name, kind string) error
	ResolveDevPath(ctx context.Context, name, kind string) (string, error)
	GetSizeBytes(ctx context.Context, name string) (uint64, error)
	GetVolumeInfo(ctx context.Context, name string) (*cubecow.Volume, error)
	GetMetrics(ctx context.Context) (map[string]uint64, error)
}
