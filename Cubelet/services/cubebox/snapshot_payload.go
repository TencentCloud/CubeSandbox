// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

// snapshotPayload is the set of physical artifacts one CommitSandbox produces;
// a nil memory means the snapshot is filesystem-only. This is the single
// decision point for whether the artifact carries guest memory.
type snapshotPayload struct {
	rootfs *storage.CowSnapshotObject
	memory *storage.CowSnapshotObject
}

func (p snapshotPayload) HasMemory() bool { return p.memory != nil }

// MemoryVol is the catalog memory volume, or "" for a filesystem-only package.
func (p snapshotPayload) MemoryVol() string {
	if p.memory == nil {
		return ""
	}
	return p.memory.Name
}

func (p snapshotPayload) MemoryKind() string {
	if p.memory == nil {
		return ""
	}
	return p.memory.Kind
}

func (p snapshotPayload) MemoryDevPath() string {
	if p.memory == nil {
		return ""
	}
	return p.memory.DevPath
}

// prepareCommitPayload decides whether a commit prepares a memory artifact and
// runs the incremental-memory ladder. A filesystem-only snapshot returns an
// empty payload and never touches memory objects or the memory lineage.
func prepareCommitPayload(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	cb *cubeboxstore.CubeBox,
	templateID string,
	memorySizeBytes uint64,
	backend string,
	mode snapshotmode.Mode,
) (snapshotPayload, string, error) {
	if mode.IsFsOnly() {
		return snapshotPayload{}, "", nil
	}
	memory, snapshotType, err := prepareCommitMemoryArtifact(ctx, stepLog, cb, templateID, memorySizeBytes, backend)
	if err != nil {
		return snapshotPayload{}, snapshotType, err
	}
	if err := validateSnapshotMemoryObject(memory, memorySizeBytes); err != nil {
		return snapshotPayload{memory: memory}, snapshotType, err
	}
	return snapshotPayload{memory: memory}, snapshotType, nil
}

// validateCommitPayload enforces the invariant that a full snapshot has a
// memory artifact and a filesystem-only snapshot has none.
func validateCommitPayload(payload snapshotPayload, mode snapshotmode.Mode) error {
	if mode.IsFsOnly() && payload.HasMemory() {
		return fmt.Errorf("filesystem-only snapshot must not have a memory volume")
	}
	if !mode.IsFsOnly() && !payload.HasMemory() {
		return fmt.Errorf("full snapshot requires a memory volume")
	}
	return nil
}

// parseCommitSnapshotMode turns the wire field into a canonical mode. An empty
// value is full for backward compatibility; any other unknown value is a
// client error, never normalized to full.
func parseCommitSnapshotMode(raw string) (snapshotmode.Mode, error) {
	if strings.TrimSpace(raw) == "" {
		return snapshotmode.Full, nil
	}
	mode, err := snapshotmode.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", snapshotmode.ErrSnapshotModeInvalid, err)
	}
	return mode, nil
}
