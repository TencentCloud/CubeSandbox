// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"

	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
)

// TestMinCoordinatedSnapshotShimVersionIsCanonical guards a fail-open trap:
// semver.Compare reports two invalid versions as equal, so a malformed boundary
// constant would make coordinatedSnapshotSupported return true for everything.
// Pinning the literal too means any change to the boundary has to be deliberate.
func TestMinCoordinatedSnapshotShimVersionIsCanonical(t *testing.T) {
	require.Equal(t, "v0.7.2-rc2", minCoordinatedSnapshotShimVersion)
	require.True(t, semver.IsValid(minCoordinatedSnapshotShimVersion),
		"boundary must be canonical v-prefixed semver, got %q", minCoordinatedSnapshotShimVersion)
	require.True(t, strings.HasPrefix(minCoordinatedSnapshotShimVersion, "v"),
		"golang.org/x/mod/semver only accepts the canonical v-prefixed form")
}

// TestCoordinatedSnapshotSupportedBoundary pins the version boundary. The
// boundary is the release candidate 0.7.2-rc2, not the final 0.7.2, because
// 0.7.2-rc2 sorts *below* 0.7.2 in semantic versioning and v0.7.2-rc2 is the
// default image tag. Anything at or above the boundary must use the coordinated
// path, including every version newer than it -- an exact-match allowlist here
// would silently downgrade every future release.
func TestCoordinatedSnapshotSupportedBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		want    bool
	}{
		{"below_boundary_legacy_shim", "v0.7.1", false},
		{"rc_before_first_coordinated_rc", "v0.7.2-rc1", false},
		{"older_base_with_high_rc", "v0.6.9-rc99", false},
		{"boundary_rc2_is_first_coordinated", "v0.7.2-rc2", true},
		{"boundary_without_v_prefix", "0.7.2-rc2", true},
		// The rc number is compared numerically. semver orders the identifiers
		// as text, which puts "rc10" *below* "rc2" -- a two-digit release
		// candidate must not silently drop back to the legacy path.
		{"two_digit_rc_above_boundary", "v0.7.2-rc10", true},
		{"three_digit_rc_above_boundary", "v0.7.2-rc100", true},
		{"rc9_above_boundary", "v0.7.2-rc9", true},
		{"later_rc_of_boundary_release", "v0.7.2-rc3", true},
		{"final_release_above_boundary", "v0.7.2", true},
		{"final_release_without_v_prefix", "0.7.2", true},
		{"build_metadata_ignored", "v0.7.2+build.1", true},
		{"build_metadata_named_vendor", "v0.7.2+vendor", true},
		{"prerelease_after_boundary_rc", "v0.7.2-rc2.1", true},
		// At the boundary's own base version, a prerelease that is not the rc
		// scheme has no defined order against the boundary, so it fails closed.
		{"boundary_base_vendor_fails_closed", "v0.7.2-vendor", false},
		{"boundary_base_s_series_fails_closed", "v0.7.2-s1", false},
		{"boundary_base_alpha_fails_closed", "v0.7.2-alpha", false},
		// On a strictly newer base version the base comparison alone decides and
		// the prerelease is deliberately ignored, so an unrecognised scheme is
		// trusted as a later release train. The asymmetry with the three rows
		// above is intended, not an oversight: the legacy retry is what keeps a
		// wrong guess cheap.
		{"later_base_vendor_is_trusted", "v0.7.3-vendor", true},
		{"later_base_s_series_is_trusted", "v0.8.0-s1", true},
		{"later_base_alpha_is_trusted", "v0.7.3-alpha", true},
		{"next_patch_enables_coordinated", "v0.7.3", true},
		{"next_patch_rc1_enables_coordinated", "v0.7.3-rc1", true},
		{"next_minor_enables_coordinated", "v0.8.0", true},
		{"future_major_enables_coordinated", "v1.0.0", true},
		{"digest_identity_is_not_a_version", "sha256-0123456789ab", false},
		{"source_build_defaults_to_legacy", "0.0.0-dev", false},
		{"two_part_version_is_not_semver", "0.7", false},
		{"guest_image_tag_is_not_a_version", "guest-image-260820-1", false},
		{"unknown_version_fails_safe", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, coordinatedSnapshotSupported(tc.version))
		})
	}
}

func TestResolveShimSnapshotCapabilityFromPins(t *testing.T) {
	t.Run("component_pin_wins", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{
			ComponentVersions: map[string]string{
				templatetypes.CubeComponentCubeShim: "v0.7.2-rc2",
			},
			LocalRunTemplate: &templatetypes.LocalRunTemplate{
				Componts: map[string]templatetypes.LocalComponent{
					templatetypes.CubeComponentCubeShim: {
						Component: templatetypes.MachineComponent{Version: "v0.7.1"},
					},
				},
			},
		}
		capability := resolveShimSnapshotCapability(cb)
		require.Equal(t, "v0.7.2-rc2", capability.Version)
		require.Equal(t, shimVersionSourceComponentPin, capability.Source)
		require.True(t, capability.Coordinated)
	})

	t.Run("run_template_fallback", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{
			LocalRunTemplate: &templatetypes.LocalRunTemplate{
				Componts: map[string]templatetypes.LocalComponent{
					templatetypes.CubeComponentCubeShim: {
						Component: templatetypes.MachineComponent{Version: "v0.7.2"},
					},
				},
			},
		}
		capability := resolveShimSnapshotCapability(cb)
		require.Equal(t, "v0.7.2", capability.Version)
		require.Equal(t, shimVersionSourceRunTemplate, capability.Source)
		require.True(t, capability.Coordinated)
	})

	// A sandbox with no pin takes the legacy path rather than being back-filled
	// from the live toolbox, which may point at an upgraded shim.
	t.Run("no_pin_uses_legacy", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cb   *cubeboxstore.CubeBox
		}{
			{"nil_sandbox", nil},
			{"empty_sandbox", &cubeboxstore.CubeBox{}},
			{"unrelated_pins_only", &cubeboxstore.CubeBox{ComponentVersions: map[string]string{
				templatetypes.CubeComponentCubeImage: "guest-image-260820-1",
			}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				capability := resolveShimSnapshotCapability(tc.cb)
				require.Empty(t, capability.Version)
				require.Equal(t, shimVersionSourceUnknown, capability.Source)
				require.False(t, capability.Coordinated)
			})
		}
	})
}

func TestPinnedShimVersionNormalizesPins(t *testing.T) {
	// Digest-suffixed pins are normalized to the inventory directory key, the
	// same way every other version read in this package normalizes them.
	cb := &cubeboxstore.CubeBox{ComponentVersions: map[string]string{
		templatetypes.CubeComponentCubeShim: "v0.7.2@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	version, source := pinnedShimVersion(cb)
	require.Equal(t, "sha256-aaaaaaaaaaaa", version)
	require.Equal(t, shimVersionSourceComponentPin, source)
}

type recordingLogger struct {
	warnings []string
}

func (l *recordingLogger) Warnf(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

// TestLogSnapshotPathSelectionNamesTheVersion keeps the downgrade observable:
// an operator must be able to tell a too-old pin from a missing one and from
// one that is not a semantic version.
func TestLogSnapshotPathSelectionNamesTheVersion(t *testing.T) {
	t.Run("pinned_too_old", func(t *testing.T) {
		logger := &recordingLogger{}
		logSnapshotPathSelection(logger, shimSnapshotCapability{
			Version: "v0.7.1",
			Source:  shimVersionSourceComponentPin,
		})
		require.Len(t, logger.warnings, 1)
		message := logger.warnings[0]
		require.Contains(t, message, "v0.7.1")
		require.Contains(t, message, shimVersionSourceComponentPin)
		require.Contains(t, message, minCoordinatedSnapshotShimVersion)
		require.Contains(t, message, "below the coordinated snapshot boundary")
		require.NotContains(t, message, "not a comparable")
	})

	// 0.0.0-dev is real semver below the boundary, so "below" is honest here.
	t.Run("source_build_is_below", func(t *testing.T) {
		logger := &recordingLogger{}
		logSnapshotPathSelection(logger, shimSnapshotCapability{
			Version: "0.0.0-dev",
			Source:  shimVersionSourceComponentPin,
		})
		require.Len(t, logger.warnings, 1)
		message := logger.warnings[0]
		require.Contains(t, message, "0.0.0-dev")
		require.Contains(t, message, "below the coordinated snapshot boundary")
		require.NotContains(t, message, "not a comparable")
	})

	// semver canonicalizes the patchless "0.7" to v0.7.0, also below.
	t.Run("two_part_version_is_below", func(t *testing.T) {
		logger := &recordingLogger{}
		logSnapshotPathSelection(logger, shimSnapshotCapability{
			Version: "0.7",
			Source:  shimVersionSourceComponentPin,
		})
		require.Len(t, logger.warnings, 1)
		message := logger.warnings[0]
		require.Contains(t, message, "0.7")
		require.Contains(t, message, "below the coordinated snapshot boundary")
		require.NotContains(t, message, "not a comparable")
	})

	t.Run("no_pin_at_all", func(t *testing.T) {
		logger := &recordingLogger{}
		logSnapshotPathSelection(logger, shimSnapshotCapability{Source: shimVersionSourceUnknown})
		require.Len(t, logger.warnings, 1)
		message := logger.warnings[0]
		require.Contains(t, message, "not recorded")
		require.Contains(t, message, shimVersionSourceUnknown)
		require.Contains(t, message, minCoordinatedSnapshotShimVersion)
		// Nothing is known about this shim, so the log must not assert what it
		// does not support.
		require.NotContains(t, message, "is below")
		require.NotContains(t, message, "not a comparable")
	})

	// A digest, or a tag semver rejects, has no order: "below" would suggest an
	// upgrade that cannot help.
	for _, version := range []string{"sha256-aaaaaaaaaaaa", "guest-image-260820-1"} {
		t.Run(version, func(t *testing.T) {
			logger := &recordingLogger{}
			logSnapshotPathSelection(logger, shimSnapshotCapability{
				Version: version,
				Source:  shimVersionSourceComponentPin,
			})
			require.Len(t, logger.warnings, 1)
			message := logger.warnings[0]
			require.Contains(t, message, version)
			require.Contains(t, message, shimVersionSourceComponentPin)
			require.Contains(t, message, minCoordinatedSnapshotShimVersion)
			require.Contains(t, message, "not a comparable semantic version")
			require.NotContains(t, message, "below")
		})
	}
}

// TestLogShimDegradedToLegacyNamesTheVersion keeps the retry actionable: the
// warning must name both the pin that selected the coordinated path and the
// boundary it was measured against, so whoever reads it during an upgrade can
// tell which of the two was wrong. There is deliberately no unresolved-version
// case: the retry only runs on the coordinated path, which requires a resolved
// pin, so an empty version is unreachable here.
func TestLogShimDegradedToLegacyNamesTheVersion(t *testing.T) {
	logger := &recordingLogger{}
	logShimDegradedToLegacy(logger, "v0.7.3")
	require.Len(t, logger.warnings, 1)
	require.Contains(t, logger.warnings[0], "v0.7.3")
	require.Contains(t, logger.warnings[0], minCoordinatedSnapshotShimVersion)
	require.Contains(t, logger.warnings[0], "rejected")
}

// TestReleaseCandidateNumber covers the parsing edges, including the shapes that
// must NOT be read as a release candidate.
func TestReleaseCandidateNumber(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"-rc1", 1, true},
		{"-rc2", 2, true},
		{"-rc10", 10, true},
		{"-rc2.1", 2, true},
		{"-rc", 0, false},
		{"-rc.", 0, false},
		{"-rcx", 0, false},
		{"-rc2bis", 0, false},
		{"-rc-5", 0, false},
		{"rc2", 0, false},
		{"-vendor", 0, false},
		{"-alpha", 0, false},
		{"", 0, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := releaseCandidateNumber(tc.in)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

// TestShouldRetryWithLegacy pins the condition the legacy retry depends on.
// Retrying on anything wider would redo a transaction after a real failure;
// retrying on anything narrower would turn a mislabelled shim into a failed RPC,
// which is exactly what the retry exists to avoid.
func TestShouldRetryWithLegacy(t *testing.T) {
	rejected := snapshotCaptureError(
		fmt.Errorf("update shim: %w", errors.New("unknown update ext action: SnapshotCapture")))
	require.True(t, shouldRetryWithLegacy(rejected, nil))

	// CommitSandbox wraps the sentinel again when restoring the binding labels
	// fails; the retry must still see it.
	wrapped := fmt.Errorf("%w; failed to restore runtime snapshot binding: %v",
		rejected, errors.New("sync failed"))
	require.True(t, shouldRetryWithLegacy(wrapped, nil))

	// The retry must never redo a transaction that already produced an artifact.
	// runSnapshotWithRootfs makes this unreachable today; the case pins the
	// invariant so a change to that helper cannot quietly break it.
	require.False(t, shouldRetryWithLegacy(rejected, errors.New("rootfs committed")))

	require.False(t, shouldRetryWithLegacy(errors.New("memory capture failed"), nil))
	require.False(t, shouldRetryWithLegacy(nil, nil))
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
