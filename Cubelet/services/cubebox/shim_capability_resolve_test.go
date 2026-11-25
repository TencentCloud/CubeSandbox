// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/capability"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
)

// resolvedShim is a helper for the source/capability combinations below.
func resolvedShim(source capability.Source, capabilities capability.Set) capability.Resolved {
	return capability.Resolved{Source: source, Capabilities: capabilities}
}

func coordinatedFull() capability.Resolved {
	return resolvedShim(capability.SourceRuntime, capability.Set{
		capability.CapUpdateSnapshotCapture:       capability.LevelInitial,
		capability.CapUpdateSnapshotCaptureFSOnly: capability.LevelInitial,
		capability.CapErrorsCoded:                 capability.LevelInitial,
	})
}

func TestPinnedShimVersionStillResolvesTheRecordedPin(t *testing.T) {
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
		version, source := pinnedShimVersion(cb)
		require.Equal(t, "v0.7.2-rc2", version)
		require.Equal(t, shimVersionSourceComponentPin, source)
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
		version, source := pinnedShimVersion(cb)
		require.Equal(t, "v0.7.2", version)
		require.Equal(t, shimVersionSourceRunTemplate, source)
	})

	// A sandbox with no pin reports nothing here.
	t.Run("no_pin_reports_unknown", func(t *testing.T) {
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
				version, source := pinnedShimVersion(tc.cb)
				require.Empty(t, version)
				require.Equal(t, shimVersionSourceUnknown, source)
			})
		}
	})
}

func TestPinnedShimVersionNormalizesPins(t *testing.T) {
	// Digest-suffixed pins are normalized to the inventory directory key.
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

// TestLogSnapshotPathSelectionNamesTheSource keeps the downgrade observable.
func TestLogSnapshotPathSelectionNamesTheSource(t *testing.T) {
	t.Run("declared_without_the_capture_capability", func(t *testing.T) {
		logger := &recordingLogger{}
		logSnapshotPathSelection(logger, capability.Resolved{
			Source:       capability.SourceLegacyTable,
			Version:      "v0.7.1",
			Capabilities: capability.Set{},
		})
		require.Len(t, logger.warnings, 1)
		message := logger.warnings[0]
		require.Contains(t, message, "v0.7.1")
		require.Contains(t, message, string(capability.SourceLegacyTable))
		require.Contains(t, message, capability.CapUpdateSnapshotCapture)
		require.Contains(t, message, "legacy best-effort snapshot path")
	})

	t.Run("nothing_known", func(t *testing.T) {
		logger := &recordingLogger{}
		logSnapshotPathSelection(logger, capability.Resolved{Source: capability.SourceNone})
		require.Len(t, logger.warnings, 1)
		message := logger.warnings[0]
		require.Contains(t, message, "nothing is known")
		// Nothing is known about this shim, so the log must not assert unsupported.
		require.NotContains(t, message, "does not declare")
	})
}

// TestLogShimDegradedToLegacyNamesTheSource keeps the retry actionable.
func TestLogShimDegradedToLegacyNamesTheSource(t *testing.T) {
	logger := &recordingLogger{}
	logShimDegradedToLegacy(logger, capability.Resolved{
		Source:  capability.SourceRuntime,
		Version: "v0.8.0",
	})
	require.Len(t, logger.warnings, 1)
	require.Contains(t, logger.warnings[0], "v0.8.0")
	require.Contains(t, logger.warnings[0], string(capability.SourceRuntime))
	require.Contains(t, logger.warnings[0], "rejected")
}

// TestShouldRetryWithLegacy pins the retry condition.
func TestShouldRetryWithLegacy(t *testing.T) {
	coordinated := coordinatedFull()
	// Recognise both the coded rejection and the legacy wording.
	codedRejected := snapshotCaptureError(
		fmt.Errorf("update shim: %w", errors.New(
			capability.ErrCodeUnknownAction+": unknown update ext action: SnapshotCapture")), coordinated)
	require.True(t, shouldRetryWithLegacy(codedRejected, nil, snapshotmode.Full, coordinated))

	legacyRejected := snapshotCaptureError(
		fmt.Errorf("update shim: %w", errors.New("unknown update ext action: SnapshotCapture")),
		capability.Resolved{Source: capability.SourceNone})
	require.True(t, shouldRetryWithLegacy(legacyRejected, nil, snapshotmode.Full,
		capability.Resolved{Source: capability.SourceNone}))

	// CommitSandbox wraps the sentinel again when restoring binding labels.
	wrapped := fmt.Errorf("%w; failed to restore runtime snapshot binding: %v",
		codedRejected, errors.New("sync failed"))
	require.True(t, shouldRetryWithLegacy(wrapped, nil, snapshotmode.Full, coordinated))

	// The retry must never redo a transaction that already produced an artifact.
	require.False(t, shouldRetryWithLegacy(codedRejected, errors.New("rootfs committed"),
		snapshotmode.Full, coordinated))

	require.False(t, shouldRetryWithLegacy(errors.New("memory capture failed"), nil,
		snapshotmode.Full, coordinated))
	require.False(t, shouldRetryWithLegacy(nil, nil, snapshotmode.Full, coordinated))

	// The legacy path always captures memory, so it cannot stand in for fs-only.
	require.False(t, shouldRetryWithLegacy(codedRejected, nil, snapshotmode.FsOnly, coordinated))
}

// Fs-only ignores the declaration; full snapshots follow it.
func TestUseCoordinatedSnapshotPath(t *testing.T) {
	legacy := resolvedShim(capability.SourceRuntime, capability.Set{})
	unknown := capability.Resolved{Source: capability.SourceNone}
	coordinated := resolvedShim(capability.SourceRuntime, capability.Set{
		capability.CapUpdateSnapshotCapture: capability.LevelInitial,
	})

	require.True(t, useCoordinatedSnapshotPath(legacy, snapshotmode.FsOnly),
		"fs-only is decided by the shim, not by the declaration")
	require.True(t, useCoordinatedSnapshotPath(unknown, snapshotmode.FsOnly))
	require.True(t, useCoordinatedSnapshotPath(coordinated, snapshotmode.FsOnly))

	// Full snapshots follow the declaration; the legacy path is the fallback.
	require.False(t, useCoordinatedSnapshotPath(legacy, snapshotmode.Full))
	require.False(t, useCoordinatedSnapshotPath(unknown, snapshotmode.Full),
		"nothing known keeps the pre-mechanism legacy path")
	require.True(t, useCoordinatedSnapshotPath(coordinated, snapshotmode.Full))
}

// Nothing-known sandboxes must keep attempting the capture.
func TestFsOnlyRefusalRefusesOnlyOnAPositiveAnswer(t *testing.T) {
	fsOnlyShim := resolvedShim(capability.SourceRuntime, capability.Set{
		capability.CapUpdateSnapshotCapture:       capability.LevelInitial,
		capability.CapUpdateSnapshotCaptureFSOnly: capability.LevelInitial,
	})

	t.Run("supported", func(t *testing.T) {
		require.Empty(t, fsOnlyRefusal(fsOnlyShim, snapshotmode.FsOnly))
	})

	t.Run("shim_declares_no_fs_only", func(t *testing.T) {
		shim := resolvedShim(capability.SourceRuntime, capability.Set{
			capability.CapUpdateSnapshotCapture: capability.LevelInitial,
		})
		require.Equal(t, snapshotmode.ErrFilesystemOnlyUnsupported,
			fsOnlyRefusal(shim, snapshotmode.FsOnly))
	})

	t.Run("guest_answered_no", func(t *testing.T) {
		shim := fsOnlyShim
		shim.Runtime = capability.Set{capability.CapGuestFSFreeze: 0}
		require.Equal(t, snapshotmode.ErrFilesystemOnlyFreezeUnsupported,
			fsOnlyRefusal(shim, snapshotmode.FsOnly))
	})

	t.Run("guest_has_not_answered_yet", func(t *testing.T) {
		// Absent is unknown, not a refusal: the capture must still be attempted.
		require.Empty(t, fsOnlyRefusal(fsOnlyShim, snapshotmode.FsOnly))
	})

	t.Run("nothing_known", func(t *testing.T) {
		// The pre-mechanism behaviour for a sandbox created before the upgrade.
		require.Empty(t, fsOnlyRefusal(
			capability.Resolved{Source: capability.SourceNone}, snapshotmode.FsOnly))
	})

	t.Run("full_requests_are_never_refused_here", func(t *testing.T) {
		shim := resolvedShim(capability.SourceRuntime, capability.Set{})
		require.Empty(t, fsOnlyRefusal(shim, snapshotmode.Full))
	})

	// Unorderable versions must not be refused up front for fs-only.
	t.Run("ambiguous_version_is_attempted_not_refused", func(t *testing.T) {
		for _, version := range []string{
			"v0.8.0-1027-1", "0.8.0-1027-1", "v0.8.0-vendor", "v0.8.0-s1",
		} {
			resolved := capability.FromLegacyVersion(version)
			require.Equal(t, capability.SourceNone, resolved.Source, version)
			require.Empty(t, fsOnlyRefusal(resolved, snapshotmode.FsOnly),
				"%s must be attempted, not refused up front", version)
		}
	})

	// A version the table can order keeps its exact answer.
	t.Run("orderable_versions_keep_their_answer", func(t *testing.T) {
		require.Empty(t, fsOnlyRefusal(capability.FromLegacyVersion("v0.8.0"), snapshotmode.FsOnly))
		require.Equal(t, snapshotmode.ErrFilesystemOnlyUnsupported,
			fsOnlyRefusal(capability.FromLegacyVersion("v0.7.3"), snapshotmode.FsOnly))
		require.Equal(t, snapshotmode.ErrFilesystemOnlyUnsupported,
			fsOnlyRefusal(capability.FromLegacyVersion("0.7.3-1027-1"), snapshotmode.FsOnly),
			"an internal build number older than the boundary is still orderable")
	})
}

func TestShimSupportsCheckPauseAllowedPrefersTheDeclaration(t *testing.T) {
	require.True(t, shimSupportsCheckPauseAllowed(capability.Resolved{Source: capability.SourceNone}),
		"nothing known: attempt the preflight, an old shim falls through harmlessly")
	require.True(t, shimSupportsCheckPauseAllowed(
		resolvedShim(capability.SourceRuntime, capability.Set{capability.CapUpdateCheckPauseAllowed: 1})))
	require.False(t, shimSupportsCheckPauseAllowed(
		resolvedShim(capability.SourceLegacyTable, capability.Set{capability.CapUpdateSnapshotCapture: 1})),
		"a positive statement that the action is absent must not be called")
}

// A shim declaring errors.coded is judged by its code, not prose.
func TestClassifyShimErrorPrefersTheCode(t *testing.T) {
	coded := resolvedShim(capability.SourceRuntime, capability.Set{capability.CapErrorsCoded: 1})
	uncoded := capability.Resolved{Source: capability.SourceNone}

	t.Run("coded_unknown_action", func(t *testing.T) {
		err := errors.New(capability.ErrCodeUnknownAction + ": unknown update ext action: SnapshotCapture")
		require.Equal(t, shimErrorUnknownAction, classifyShimError(err, coded))
	})

	t.Run("coded_action_with_reworded_prose", func(t *testing.T) {
		// The historical prose is gone, but the code still identifies the case.
		err := errors.New(capability.ErrCodeUnknownAction + ": this shim has no such action")
		require.Equal(t, shimErrorUnknownAction, classifyShimError(err, coded))
	})

	t.Run("coded_pause_busy", func(t *testing.T) {
		err := errors.New(capability.ErrCodePauseBusy + ": sandbox pause forbidding, terminate exec tasks first")
		require.Equal(t, shimErrorPauseBusy, classifyShimError(err, coded))
	})

	t.Run("coded_other_failure", func(t *testing.T) {
		require.Equal(t, shimErrorOther,
			classifyShimError(errors.New("snapshot capture failed after VM pause"), coded))
	})

	t.Run("legacy_prose_still_works", func(t *testing.T) {
		require.Equal(t, shimErrorUnknownAction,
			classifyShimError(errors.New("unknown update ext action: SnapshotCapture"), uncoded))
		require.Equal(t, shimErrorPauseBusy,
			classifyShimError(errors.New("sandbox pause forbidding, terminate exec tasks first"), uncoded))
	})

	t.Run("nil_is_not_an_error_kind", func(t *testing.T) {
		require.Equal(t, shimErrorNone, classifyShimError(nil, coded))
	})
}

func TestIsPauseBusyRejectUnderstandsBothForms(t *testing.T) {
	for _, message := range []string{
		"sandbox pause forbidding, terminate exec tasks first",
		"Others(\"Other: update sandbox failed:sandbox pause forbidding, terminate exec tasks first\")",
		capability.ErrCodePauseBusy + ": sandbox pause forbidding, terminate exec tasks first",
	} {
		require.True(t, isPauseBusyReject(message), message)
	}
	for _, message := range []string{
		"",
		"snapshot capture failed after VM pause",
		"shim_error_unknown_action: unknown update ext action: Foo",
	} {
		require.False(t, isPauseBusyReject(message), message)
	}
}

func TestSandboxShimBundleLookupSuppliesTheSandboxNamespace(t *testing.T) {
	// Snapshot/pause decisions carry no namespace; the lookup supplies the sandbox's.
	previous := resolveShimBundles
	t.Cleanup(func() { resolveShimBundles = previous })

	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb-ns", Namespace: "custom-ns"}}

	var seenNamespace string
	resolveShimBundles = func(_ *local, ctx context.Context, _ *cubeboxstore.CubeBox) []string {
		if ns, ok := namespaces.Namespace(ctx); ok {
			seenNamespace = ns
		}
		return []string{"/bundle"}
	}
	l := &local{}
	require.Equal(t, "/bundle", l.sandboxShimBundlePath(context.Background(), cb))
	require.Equal(t, "custom-ns", seenNamespace,
		"the lookup must carry the sandbox's namespace")
}

func TestSandboxShimArtifactDirUsesOnlyRecordedAbsolutePaths(t *testing.T) {
	t.Run("runtime annotation_wins", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.AnnotationCubeletInternalRuntimePath: "/data/cubelet/root/component_versions/cube-shim/v0.8.0/bin/containerd-shim-cube-rs",
			},
		}}
		require.Equal(t, "/data/cubelet/root/component_versions/cube-shim/v0.8.0",
			sandboxShimArtifactDir(cb))
	})

	t.Run("absolute template path is used", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{
					Path: "/usr/local/services/cubetoolbox/cube-shim/bin/containerd-shim-cube-rs",
				}},
			},
		}}
		require.Equal(t, "/usr/local/services/cubetoolbox/cube-shim", sandboxShimArtifactDir(cb))
	})

	t.Run("relative or missing path yields nothing", func(t *testing.T) {
		// A relative path names nothing on this host.
		require.Empty(t, sandboxShimArtifactDir(&cubeboxstore.CubeBox{LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{
					Path: "bin/containerd-shim-cube-rs",
				}},
			},
		}}))
		require.Empty(t, sandboxShimArtifactDir(&cubeboxstore.CubeBox{}))
		require.Empty(t, sandboxShimArtifactDir(nil))
	})

	// The selection order must mirror withRuntimePathOpt: OciRuntime.Path, run
	// template, then annotation, last writer wins.
	t.Run("precedence_matches_the_task_runtime_path_selection", func(t *testing.T) {
		shimPath := func(root string) templatetypes.LocalRunTemplate {
			return templatetypes.LocalRunTemplate{Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{
					Path: root + "/bin/containerd-shim-cube-rs",
				}},
			}}
		}
		template := shimPath("/template/v0.7.3")
		cb := &cubeboxstore.CubeBox{
			Metadata: cubeboxstore.Metadata{Annotations: map[string]string{
				constants.AnnotationCubeletInternalRuntimePath: "/annotated/v0.9.0/bin/containerd-shim-cube-rs",
			}},
			LocalRunTemplate: &template,
			OciRuntime:       &cubeconfig.Runtime{Path: "/ociruntime/v0.8.0/bin/containerd-shim-cube-rs"},
		}

		// Selection order asserted by withRuntimePathOpt: each later source wins.
		require.Equal(t, "/annotated/v0.9.0", sandboxShimArtifactDir(cb),
			"the annotation wins when all three are present")

		delete(cb.Annotations, constants.AnnotationCubeletInternalRuntimePath)
		require.Equal(t, "/template/v0.7.3", sandboxShimArtifactDir(cb),
			"the run template wins over the registry default")

		cb.LocalRunTemplate = nil
		require.Equal(t, "/ociruntime/v0.8.0", sandboxShimArtifactDir(cb),
			"OciRuntime.Path is the base the other two override, not something to skip")
	})
}

func TestSandboxCapabilitiesRoundTripThroughTheRecord(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb"}}
	cb.RecordResolvedShimCapabilities(capability.Resolved{
		Capabilities: capability.Set{capability.CapUpdateSnapshotCapture: 1},
		Runtime:      capability.Set{capability.CapGuestFSFreeze: 0},
		Source:       capability.SourceRuntime,
		Version:      "v0.8.0",
	})

	resolved := cb.ResolvedShimCapabilities()
	require.Equal(t, capability.SourceRuntime, resolved.Source)
	require.Equal(t, "v0.8.0", resolved.Version)
	require.True(t, resolved.Capabilities.Has(capability.CapUpdateSnapshotCapture, 1))
	require.True(t, resolved.Runtime.KnownUnsupported(capability.CapGuestFSFreeze, 1))

	// The persisted form must survive Store.Sync's serialisation of a DeepCopy.
	copied := cb.DeepCopy()
	require.NotNil(t, copied.ShimCapabilities)
	require.Equal(t, cb.ShimCapabilities.Source, copied.ShimCapabilities.Source)

	// Replacing the record removes capabilities the new shim does not have.
	cb.RecordResolvedShimCapabilities(capability.Resolved{
		Capabilities: capability.Set{},
		Source:       capability.SourceNone,
	})
	replaced := cb.ResolvedShimCapabilities()
	require.Equal(t, capability.SourceNone, replaced.Source)
	require.Empty(t, replaced.Capabilities)
}

func TestShimCapabilityRecordDeepCopyIsIndependent(t *testing.T) {
	original := &cubeboxstore.ShimCapabilitiesRecord{
		Capabilities: map[string]uint32{capability.CapUpdateSnapshotCapture: 1},
		Runtime:      map[string]uint32{capability.CapGuestFSFreeze: 1},
		Source:       string(capability.SourceRuntime),
	}
	copied := original.DeepCopy()
	copied.Capabilities[capability.CapUpdateSnapshotCapture] = 2
	copied.Runtime[capability.CapGuestFSFreeze] = 0
	require.Equal(t, uint32(1), original.Capabilities[capability.CapUpdateSnapshotCapture])
	require.Equal(t, uint32(1), original.Runtime[capability.CapGuestFSFreeze])
	require.Nil(t, (*cubeboxstore.ShimCapabilitiesRecord)(nil).DeepCopy())
}

func TestShimCapabilityFileExists(t *testing.T) {
	dir := t.TempDir()
	require.False(t, shimCapabilityFileExists(dir))
	require.False(t, shimCapabilityFileExists(""))
	writeCapabilityFile(t, dir, capability.RuntimeFileName, "v0.8.0", nil)
	require.True(t, shimCapabilityFileExists(dir))
}

// writeCapabilityFile writes a shim capability document into dir.
func writeCapabilityFile(t *testing.T, dir, name, version string, caps map[string]uint32) string {
	t.Helper()
	body := fmt.Sprintf(`{"schema":1,"component":"cube-shim","version":%q`, version)
	if len(caps) > 0 {
		parts := make([]string, 0, len(caps))
		for k, v := range caps {
			parts = append(parts, fmt.Sprintf("%q:%d", k, v))
		}
		body += `,"capabilities":{` + strings.Join(parts, ",") + `}`
	}
	body += "}"
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}
