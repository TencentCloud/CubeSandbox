// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
)

// ErrArtifactInUse indicates the ext4 OS image artifact is still referenced by
// a running sandbox on this node, so its physical files are intentionally
// preserved. The caller (CubeMaster last-owner-cleanup / GC) must retry after
// the referencing sandbox exits. This is a protection, not a leak.
var ErrArtifactInUse = errors.New("ext4 artifact in use by running sandbox")

// ArtifactInUseFunc reports whether the given artifact (image) id is still
// referenced by any sandbox tracked on this node.
type ArtifactInUseFunc func(artifactID string) (bool, error)

// DestroyPmemArtifact synchronously and idempotently removes the on-disk ext4
// OS image artifact directory (rootfs .ext4 + kernel .vm + companion files) for
// the given instanceType/artifactID. It is the ext4 counterpart of the
// containerd image removal path used by DestroyImage.
//
// Unlike the containerd path it does NOT go through cdp.PreDelete (which would
// also run the runtemplate catalog hook and could be blocked by a stale bolt
// index). Template-level reference decisions are authoritative in CubeMaster;
// the only node-local safety check here is whether a running sandbox still uses
// the artifact.
//
// Safety invariants:
//   - instanceType/artifactID must pass ValidateSafeID (no separators/traversal).
//   - the managed pmem base is resolved before constructing the artifact leaf,
//     and the final directory must live under that resolved base.
//   - if inUse reports the artifact is referenced by a running sandbox, deletion
//     is refused with ErrArtifactInUse (no physical removal).
//   - a missing directory is treated as success (idempotent).
func DestroyPmemArtifact(ctx context.Context, instanceType, artifactID string, inUse ArtifactInUseFunc) error {
	if err := pathutil.ValidateSafeID(instanceType); err != nil {
		return fmt.Errorf("invalid instanceType %q: %w", instanceType, err)
	}
	if err := pathutil.ValidateSafeID(artifactID); err != nil {
		return fmt.Errorf("invalid artifactID %q: %w", artifactID, err)
	}

	if inUse != nil {
		used, err := inUse(artifactID)
		if err != nil {
			return fmt.Errorf("check artifact %q in-use: %w", artifactID, err)
		}
		if used {
			return ErrArtifactInUse
		}
	}

	base := pmem.GetPmemBasePath(instanceType)
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("resolve pmem base %q: %w", base, err)
	}
	dir := filepath.Join(resolvedBase, artifactID)
	resolved, err := pathutil.ValidatePathUnderBase(resolvedBase, dir)
	if err != nil {
		return fmt.Errorf("artifact path validation failed: %w", err)
	}

	if err := os.RemoveAll(resolved); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove ext4 artifact dir %q: %w", resolved, err)
	}
	log.G(ctx).Infof("destroyed ext4 artifact dir %q (instanceType=%s artifactID=%s)", resolved, instanceType, artifactID)
	return nil
}
