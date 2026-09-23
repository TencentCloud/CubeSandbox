// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

const (
	coordinatedSnapshotShimVersion    = "0.7.2"
	coordinatedSnapshotShimRC2Version = "0.7.2-rc2"
)

func useCoordinatedSnapshotPath(cb *cubeboxstore.CubeBox) bool {
	if cb == nil {
		return false
	}
	version := strings.TrimSpace(cb.ComponentVersions[templatetypes.CubeComponentCubeShim])
	if version == "" && cb.LocalRunTemplate != nil {
		if shim, ok := cb.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeShim]; ok {
			version = strings.TrimSpace(shim.Component.Version)
		}
	}
	switch strings.TrimPrefix(version, "v") {
	case coordinatedSnapshotShimVersion, coordinatedSnapshotShimRC2Version:
		return true
	default:
		return false
	}
}

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

func captureLegacyCommitMemory(cb *cubeboxstore.CubeBox, snapshotID string, invalidate, capture func() error) error {
	if err := invalidate(); err != nil {
		return err
	}
	if err := capture(); err != nil {
		return err
	}
	// cube-runtime has cleared soft-dirty state, so every subsequent commit
	// must resolve this snapshot or safely degrade to a full capture.
	setRuntimeSnapshotBindingLabels(cb, snapshotID, time.Now().UTC())
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

	metadataPath := filepath.Join(snapshotPath, "metadata.json")
	body, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("read legacy snapshot metadata: %w", err)
	}

	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(body, &metadata); err != nil {
		return fmt.Errorf("parse legacy snapshot metadata: %w", err)
	}
	if metadata == nil {
		return fmt.Errorf("parse legacy snapshot metadata: expected JSON object")
	}
	if versions.GuestImage != "" {
		metadata["image_version"], err = json.Marshal(versions.GuestImage)
		if err != nil {
			return fmt.Errorf("marshal legacy snapshot image version: %w", err)
		}
	}
	if versions.Agent != "" {
		metadata["agent_version"], err = json.Marshal(versions.Agent)
		if err != nil {
			return fmt.Errorf("marshal legacy snapshot agent version: %w", err)
		}
	}

	body, err = json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal legacy snapshot metadata: %w", err)
	}
	tmpPath := metadataPath + ".tmp"
	if err := os.WriteFile(tmpPath, body, 0o644); err != nil {
		return fmt.Errorf("write legacy snapshot metadata: %w", err)
	}
	if err := os.Rename(tmpPath, metadataPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish legacy snapshot metadata: %w", err)
	}
	return nil
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
