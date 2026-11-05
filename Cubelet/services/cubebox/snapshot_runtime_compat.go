// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// runSnapshotWithRootfs captures memory and rootfs in one frozen window.
func runSnapshotWithRootfs(snapshot, rootfs, resume func() error) (snapshotErr, rootfsErr, resumeErr error) {
	if snapshotErr = snapshot(); snapshotErr != nil {
		resumeErr = resume()
		return
	}
	defer func() { resumeErr = resume() }()
	rootfsErr = rootfs()
	return
}

// runLegacySnapshot preserves the v0.7.1 sequential snapshot behavior.
func runLegacySnapshot(first, second func() error) (firstErr, secondErr error) {
	if firstErr = first(); firstErr != nil {
		return
	}
	secondErr = second()
	return
}

// captureLegacyMemory captures the memory artifact with the cube-runtime CLI and
// then repairs the image and agent versions it wrote into metadata.json from the
// sandbox's pins. The CLI freezes and resumes the VM on its own, so callers must
// not hold a freeze window across it.
func (s *service) captureLegacyMemory(
	ctx context.Context,
	cb *cubeboxstore.CubeBox,
	sandboxID string,
	spec *CubeboxSnapshotSpec,
	workDir, memoryVol, snapshotType string,
) error {
	if err := s.executeCubeRuntimeSnapshot(ctx, sandboxID, spec, workDir, memoryVol, snapshotType); err != nil {
		return err
	}
	return correctLegacySnapshotMetadataVersions(cb, workDir)
}

func captureLegacyCommitMemory(cb *cubeboxstore.CubeBox, snapshotID string, invalidate, capture func() error) error {
	if err := invalidate(); err != nil {
		return err
	}
	if err := capture(); err != nil {
		return err
	}
	// cube-runtime has cleared soft-dirty state, so every subsequent commit
	// must resolve this snapshot or safely degrade to a full capture.
	setRuntimeSnapshotBinding(cb, snapshotID)
	return nil
}

// correctLegacySnapshotMetadataVersions repairs metadata written by the
// cube-runtime snapshot command. Legacy cube-runtime resolves image and agent
// versions through the live toolbox paths, which may point at upgraded
// components even though the running sandbox is pinned to older artifacts.
func correctLegacySnapshotMetadataVersions(cb *cubeboxstore.CubeBox, snapshotPath string) error {
	if cb == nil {
		return nil
	}

	versions := guestEnvironmentVersionsFromComponentMap(
		cb.ComponentVersions,
		guestEnvironmentVersionsFromComponentMap(versionsFromLocalTemplate(cb.LocalRunTemplate), guestEnvironmentVersions{}),
	)
	if versions.GuestImage == "" || versions.Agent == "" {
		return fmt.Errorf(
			"legacy snapshot requires pinned image and agent versions: image=%q agent=%q",
			versions.GuestImage, versions.Agent,
		)
	}

	return mutateSnapshotPackageMetadata(snapshotPath, "legacy snapshot metadata",
		func(metadata map[string]json.RawMessage) error {
			if versions.GuestImage != "" {
				encoded, err := json.Marshal(versions.GuestImage)
				if err != nil {
					return fmt.Errorf("marshal legacy snapshot image version: %w", err)
				}
				metadata["image_version"] = encoded
			}
			if versions.Agent != "" {
				encoded, err := json.Marshal(versions.Agent)
				if err != nil {
					return fmt.Errorf("marshal legacy snapshot agent version: %w", err)
				}
				metadata["agent_version"] = encoded
			}
			return nil
		})
}

func checkCommitSnapshotDestination(ctx context.Context, backend, snapshotID string, inspect func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error)) error {
	// Include work and sealed objects: S3 may resolve an existing writable
	// memory volume instead of rejecting it, so none may be reused here.
	refs := storage.DefaultTemplateObjectRefs(snapshotID)
	statuses, err := inspect(ctx, backend, refs)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		if status.Exists {
			return fmt.Errorf("%w: name=%s kind=%s", storage.ErrCowObjectAlreadyExists, status.Name, status.Kind)
		}
	}
	return nil
}
