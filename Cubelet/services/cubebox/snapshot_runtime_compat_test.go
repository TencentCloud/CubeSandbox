// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"

	"github.com/stretchr/testify/require"
)

func TestUseCoordinatedSnapshotPathOnlyForV072(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		want    bool
	}{
		{"plain v0.7.2", "0.7.2", true},
		{"prefixed v0.7.2", "v0.7.2", true},
		{"plain v0.7.2-rc2", "0.7.2-rc2", true},
		{"prefixed v0.7.2-rc2", "v0.7.2-rc2", true},
		{"other release candidate", "v0.7.2-rc1", false},
		{"legacy", "v0.7.1", false},
		{"future", "v0.8.0", false},
		{"digest", "sha256-0123456789ab", false},
		{"missing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cb := &cubeboxstore.CubeBox{ComponentVersions: map[string]string{
				templatetypes.CubeComponentCubeShim: tc.version,
			}}
			require.Equal(t, tc.want, useCoordinatedSnapshotPath(cb))
		})
	}

	require.False(t, useCoordinatedSnapshotPath(nil))
	require.True(t, useCoordinatedSnapshotPath(&cubeboxstore.CubeBox{
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {
					Component: templatetypes.MachineComponent{Version: "v0.7.2"},
				},
			},
		},
	}))
}

func TestRunSnapshotWithRootfs(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                              string
		snapshotErr, rootfsErr, resumeErr error
		wantCalls                         []string
	}{
		{"success", nil, nil, nil, []string{"memory", "rootfs", "resume"}},
		{"memory failure", failure, nil, nil, []string{"memory", "resume"}},
		{"rootfs failure", nil, failure, nil, []string{"memory", "rootfs", "resume"}},
		{"resume failure", nil, nil, failure, []string{"memory", "rootfs", "resume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			snapshotErr, rootfsErr, resumeErr := runSnapshotWithRootfs(
				func() error { calls = append(calls, "memory"); return tc.snapshotErr },
				func() error { calls = append(calls, "rootfs"); return tc.rootfsErr },
				func() error { calls = append(calls, "resume"); return tc.resumeErr },
			)
			require.Equal(t, tc.wantCalls, calls)
			require.Equal(t, tc.snapshotErr, snapshotErr)
			require.Equal(t, tc.rootfsErr, rootfsErr)
			require.Equal(t, tc.resumeErr, resumeErr)
		})
	}
}

func TestRunLegacySnapshot(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                string
		firstErr, secondErr error
		wantCalls           []string
	}{
		{"success", nil, nil, []string{"first", "second"}},
		{"first failure", failure, nil, []string{"first"}},
		{"second failure", nil, failure, []string{"first", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			firstErr, secondErr := runLegacySnapshot(
				func() error { calls = append(calls, "first"); return tc.firstErr },
				func() error { calls = append(calls, "second"); return tc.secondErr },
			)
			require.Equal(t, tc.wantCalls, calls)
			require.Equal(t, tc.firstErr, firstErr)
			require.Equal(t, tc.secondErr, secondErr)
		})
	}
}

func TestCaptureLegacyCommitMemoryAdvancesBaselineOnlyOnSuccess(t *testing.T) {
	invalidateFailure := errors.New("invalidate failed")
	captureFailure := errors.New("capture failed")
	for _, tc := range []struct {
		name          string
		invalidateErr error
		captureErr    error
		wantCalls     []string
		wantBase      string
	}{
		{"success", nil, nil, []string{"invalidate", "capture"}, "next"},
		{"invalidation failure", invalidateFailure, nil, []string{"invalidate"}, "previous"},
		{"capture or metadata failure", nil, captureFailure, []string{"invalidate", "capture"}, runtimeSnapshotBindingInvalidID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox"}}
			setRuntimeSnapshotBindingLabels(cb, "previous", time.Now().UTC())
			var calls []string
			err := captureLegacyCommitMemory(cb, "next", func() error {
				calls = append(calls, "invalidate")
				if tc.invalidateErr == nil {
					setRuntimeSnapshotBindingLabels(cb, runtimeSnapshotBindingInvalidID, time.Now().UTC())
				}
				return tc.invalidateErr
			}, func() error {
				calls = append(calls, "capture")
				return tc.captureErr
			})
			wantErr := tc.invalidateErr
			if wantErr == nil {
				wantErr = tc.captureErr
			}
			require.ErrorIs(t, err, wantErr)
			require.Equal(t, tc.wantCalls, calls)
			require.Equal(t, tc.wantBase, resolveBaseSnapshotID(cb))
		})
	}
}

func TestCorrectLegacySnapshotMetadataVersionsUsesPinnedSandboxVersions(t *testing.T) {
	snapshotPath := t.TempDir()
	metadataPath := filepath.Join(snapshotPath, "metadata.json")
	require.NoError(t, os.WriteFile(metadataPath, []byte(`{
		"image_version":"v0.7.2-rc2",
		"agent_version":"v0.7.2-rc2",
		"kernel_version":"kernel-from-snapshot",
		"vm_res":{"cpu":2}
	}`), 0o644))

	cb := &cubeboxstore.CubeBox{
		ComponentVersions: map[string]string{
			templatetypes.CubeComponentCubeImage: "guest-image-260820-1",
			templatetypes.CubeComponentCubeAgent: "v0.7.1",
		},
	}
	require.NoError(t, correctLegacySnapshotMetadataVersions(cb, snapshotPath))

	var metadata map[string]json.RawMessage
	body, err := os.ReadFile(metadataPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &metadata))
	require.JSONEq(t, `"guest-image-260820-1"`, string(metadata["image_version"]))
	require.JSONEq(t, `"v0.7.1"`, string(metadata["agent_version"]))
	require.JSONEq(t, `"kernel-from-snapshot"`, string(metadata["kernel_version"]))
	require.JSONEq(t, `{"cpu":2}`, string(metadata["vm_res"]))
}

func TestCorrectLegacySnapshotMetadataVersionsFallsBackToPinnedTemplate(t *testing.T) {
	snapshotPath := t.TempDir()
	metadataPath := filepath.Join(snapshotPath, "metadata.json")
	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"image_version":"current","agent_version":"current"}`), 0o644))

	cb := &cubeboxstore.CubeBox{
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeImage: {
					Component: templatetypes.MachineComponent{Version: "old-image"},
				},
				templatetypes.CubeComponentCubeAgent: {
					Component: templatetypes.MachineComponent{Version: "old-agent"},
				},
			},
		},
	}
	require.NoError(t, correctLegacySnapshotMetadataVersions(cb, snapshotPath))

	var metadata struct {
		ImageVersion string `json:"image_version"`
		AgentVersion string `json:"agent_version"`
	}
	body, err := os.ReadFile(metadataPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &metadata))
	require.Equal(t, "old-image", metadata.ImageVersion)
	require.Equal(t, "old-agent", metadata.AgentVersion)
}

func TestCorrectLegacySnapshotMetadataVersionsRejectsInvalidMetadata(t *testing.T) {
	snapshotPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(snapshotPath, "metadata.json"), []byte(`not-json`), 0o644))
	cb := &cubeboxstore.CubeBox{ComponentVersions: map[string]string{
		templatetypes.CubeComponentCubeImage: "old-image",
		templatetypes.CubeComponentCubeAgent: "old-agent",
	}}

	err := correctLegacySnapshotMetadataVersions(cb, snapshotPath)
	require.ErrorContains(t, err, "parse legacy snapshot metadata")
}

func TestCorrectLegacySnapshotMetadataVersionsRequiresCompletePins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions map[string]string
	}{
		{"missing image", map[string]string{templatetypes.CubeComponentCubeAgent: "old-agent"}},
		{"missing agent", map[string]string{templatetypes.CubeComponentCubeImage: "old-image"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := correctLegacySnapshotMetadataVersions(&cubeboxstore.CubeBox{ComponentVersions: tc.versions}, t.TempDir())
			require.ErrorContains(t, err, "requires pinned image and agent versions")
		})
	}
}

func TestCorrectLegacySnapshotMetadataVersionsRejectsNullMetadata(t *testing.T) {
	snapshotPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(snapshotPath, "metadata.json"), []byte(`null`), 0o644))
	cb := &cubeboxstore.CubeBox{ComponentVersions: map[string]string{
		templatetypes.CubeComponentCubeImage: "old-image",
		templatetypes.CubeComponentCubeAgent: "old-agent",
	}}

	err := correctLegacySnapshotMetadataVersions(cb, snapshotPath)
	require.ErrorContains(t, err, "expected JSON object")
}

func TestCommitSnapshotDestinationRejectsExistingPackage(t *testing.T) {
	for _, name := range []string{"tpl-T-rootfs", "tpl-T-memory", "tpl-T-memory-snap", storage.S3MetadataSnapshotName("T")} {
		t.Run(name, func(t *testing.T) {
			err := checkCommitSnapshotDestination(context.Background(), "s3", "T",
				func(_ context.Context, backend string, refs []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
					require.Equal(t, "s3", backend)
					for _, ref := range refs {
						if ref.Name == name {
							return []storage.CowObjectStatus{{Name: name, Kind: ref.Kind, Exists: true}}, nil
						}
					}
					t.Fatalf("existing object %s was not checked", name)
					return nil, nil
				})
			require.ErrorIs(t, err, storage.ErrCowObjectAlreadyExists)
		})
	}
	dbErr := errors.New("inspect unavailable")
	err := checkCommitSnapshotDestination(context.Background(), "s3", "T",
		func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
			return nil, dbErr
		})
	require.ErrorIs(t, err, dbErr)
	require.NoError(t, checkCommitSnapshotDestination(context.Background(), "xfs", "new",
		func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
			return nil, nil
		}))
}
