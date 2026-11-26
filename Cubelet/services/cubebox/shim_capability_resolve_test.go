// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
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

	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
)

// resolvedShim is a helper for the source/capability combinations below.
func resolvedShim(source capability.Source, capabilities capability.Set) capability.Resolved {
	return capability.Resolved{Source: source, Capabilities: capabilities}
}

func coordinatedFull() capability.Resolved {
	return resolvedShim(capability.SourceArtifact, capability.Set{
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
		require.Equal(t, versionSourceComponentPin, source)
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
		require.Equal(t, versionSourceRunTemplate, source)
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
				require.Equal(t, versionSourceUnknown, source)
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
	require.Equal(t, versionSourceComponentPin, source)
}

func TestPinnedComponentVersionReadsOnlyRecordedPins(t *testing.T) {
	agent := templatetypes.CubeComponentCubeAgent

	t.Run("component_pin_wins", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{
			ComponentVersions: map[string]string{agent: "v0.8.0"},
			LocalRunTemplate: &templatetypes.LocalRunTemplate{
				Componts: map[string]templatetypes.LocalComponent{
					agent: {Component: templatetypes.MachineComponent{Version: "v0.7.3"}},
				},
			},
		}
		version, source := pinnedComponentVersion(cb, agent)
		require.Equal(t, "v0.8.0", version)
		require.Equal(t, versionSourceComponentPin, source)
	})

	t.Run("run_template_fallback", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				agent: {Component: templatetypes.MachineComponent{Version: "v0.7.3-1027-1"}},
			},
		}}
		version, source := pinnedComponentVersion(cb, agent)
		require.Equal(t, "v0.7.3-1027-1", version)
		require.Equal(t, versionSourceRunTemplate, source)
	})

	// A pin recorded for another component must not be mistaken for this one, and
	// nothing pinned stays unknown: the answer describes the build this sandbox was
	// created with, so the live install tree is never consulted as a fallback.
	t.Run("other_pins_and_live_tree_are_not_used", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{ComponentVersions: map[string]string{
			templatetypes.CubeComponentCubeShim:  "v0.8.0",
			templatetypes.CubeComponentCubeImage: "guest-image-260820-1",
		}}
		version, source := pinnedComponentVersion(cb, agent)
		require.Empty(t, version)
		require.Equal(t, versionSourceUnknown, source)
	})
}

func TestSandboxAgentSelectedPathUsesOnlyTheRunTemplatePath(t *testing.T) {
	agent := templatetypes.CubeComponentCubeAgent
	const payload = "/data/cubelet/root/component_versions/cube-agent/v0.8.0/cube-agent.ext4"
	cb := &cubeboxstore.CubeBox{LocalRunTemplate: &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{
			agent: {Component: templatetypes.MachineComponent{Path: payload}},
		},
	}}
	require.Equal(t, payload, sandboxAgentSelectedPath(cb))

	// A relative path names nothing on this host, and the shim's path is not a
	// stand-in for the agent's.
	cb.LocalRunTemplate.Componts[agent] = templatetypes.LocalComponent{
		Component: templatetypes.MachineComponent{Path: "cube-agent.ext4"},
	}
	require.Empty(t, sandboxAgentSelectedPath(cb))
	require.Empty(t, sandboxAgentSelectedPath(&cubeboxstore.CubeBox{}))
	require.Empty(t, sandboxAgentSelectedPath(nil))
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
		Source:  capability.SourceArtifact,
		Version: "v0.8.0",
	})
	require.Len(t, logger.warnings, 1)
	require.Contains(t, logger.warnings[0], "v0.8.0")
	require.Contains(t, logger.warnings[0], string(capability.SourceArtifact))
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
	legacy := resolvedShim(capability.SourceArtifact, capability.Set{})
	unknown := capability.Resolved{Source: capability.SourceNone}
	coordinated := resolvedShim(capability.SourceArtifact, capability.Set{
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
	fsOnlyShim := resolvedShim(capability.SourceArtifact, capability.Set{
		capability.CapUpdateSnapshotCapture:       capability.LevelInitial,
		capability.CapUpdateSnapshotCaptureFSOnly: capability.LevelInitial,
	})
	// An agent that declares it can freeze, and one that declares it cannot.
	freezeAgent := resolvedShim(capability.SourceArtifact, capability.Set{
		capability.CapGuestFSFreeze: capability.LevelInitial,
	})
	noFreezeAgent := resolvedShim(capability.SourceArtifact, capability.Set{})

	t.Run("supported", func(t *testing.T) {
		require.Empty(t, fsOnlyRefusal(fsOnlyShim, freezeAgent, snapshotmode.FsOnly))
	})

	t.Run("shim_declares_no_fs_only", func(t *testing.T) {
		shim := resolvedShim(capability.SourceArtifact, capability.Set{
			capability.CapUpdateSnapshotCapture: capability.LevelInitial,
		})
		require.Equal(t, snapshotmode.ErrFilesystemOnlyUnsupported,
			fsOnlyRefusal(shim, freezeAgent, snapshotmode.FsOnly))
	})

	t.Run("agent_declares_no_freeze", func(t *testing.T) {
		require.Equal(t, snapshotmode.ErrFilesystemOnlyFreezeUnsupported,
			fsOnlyRefusal(fsOnlyShim, noFreezeAgent, snapshotmode.FsOnly))
	})

	t.Run("agent_declaration_missing_is_attempted", func(t *testing.T) {
		// Absent is unknown, not a refusal: the capture must still be attempted and
		// the freeze RPC remains the authority.
		unknownAgent := capability.Resolved{Source: capability.SourceNone}
		require.Empty(t, fsOnlyRefusal(fsOnlyShim, unknownAgent, snapshotmode.FsOnly))
	})

	t.Run("nothing_known", func(t *testing.T) {
		// The pre-mechanism behaviour for a sandbox created before the upgrade.
		unknown := capability.Resolved{Source: capability.SourceNone}
		require.Empty(t, fsOnlyRefusal(unknown, unknown, snapshotmode.FsOnly))
	})

	t.Run("full_requests_are_never_refused_here", func(t *testing.T) {
		shim := resolvedShim(capability.SourceArtifact, capability.Set{})
		require.Empty(t, fsOnlyRefusal(shim, noFreezeAgent, snapshotmode.Full))
	})

	// Unorderable versions must not be refused up front for fs-only.
	t.Run("ambiguous_version_is_attempted_not_refused", func(t *testing.T) {
		for _, version := range []string{
			"v0.8.0-1027-1", "0.8.0-1027-1", "v0.8.0-vendor", "v0.8.0-s1",
		} {
			ambiguous := capability.FromLegacyVersion(version)
			require.Equal(t, capability.SourceNone, ambiguous.Source, version)
			require.Empty(t, fsOnlyRefusal(ambiguous, ambiguous, snapshotmode.FsOnly),
				"%s must be attempted, not refused up front", version)
			// Same rule on the agent side.
			require.Empty(t, fsOnlyRefusal(fsOnlyShim, capability.FromLegacyAgentVersion(version),
				snapshotmode.FsOnly), "%s agent side", version)
		}
	})

	// A version the table can order keeps its exact answer.
	t.Run("orderable_versions_keep_their_answer", func(t *testing.T) {
		require.Empty(t, fsOnlyRefusal(capability.FromLegacyVersion("v0.8.0"), freezeAgent, snapshotmode.FsOnly))
		require.Equal(t, snapshotmode.ErrFilesystemOnlyUnsupported,
			fsOnlyRefusal(capability.FromLegacyVersion("v0.7.3"), freezeAgent, snapshotmode.FsOnly))
		require.Equal(t, snapshotmode.ErrFilesystemOnlyUnsupported,
			fsOnlyRefusal(capability.FromLegacyVersion("0.7.3-1027-1"), freezeAgent, snapshotmode.FsOnly),
			"an internal build number older than the boundary is still orderable")
		// The agent table answers independently.
		require.Equal(t, snapshotmode.ErrFilesystemOnlyFreezeUnsupported,
			fsOnlyRefusal(fsOnlyShim, capability.FromLegacyAgentVersion("0.7.3-1027-1"), snapshotmode.FsOnly))
	})
}

// The refusal message names the component that actually refused.
func TestFsOnlyRefusalMessageNamesTheRefusingComponent(t *testing.T) {
	shim := capability.Resolved{Source: capability.SourceArtifact, Version: "v0.9.1"}
	guest := capability.Resolved{Source: capability.SourceArtifact, Version: "v0.9.2"}
	sourceNone := capability.Resolved{Source: capability.SourceNone}

	t.Run("shim_refusal_names_the_shim", func(t *testing.T) {
		require.Equal(t,
			`snapshot_filesystem_only_unsupported_node: shim "v0.9.1" does not support filesystem-only snapshots`,
			fsOnlyRefusalMessage(snapshotmode.ErrFilesystemOnlyUnsupported, shim, guest))
	})

	t.Run("agent_refusal_names_the_agent", func(t *testing.T) {
		require.Equal(t,
			`snapshot_filesystem_only_freeze_unsupported: guest agent "v0.9.2" cannot freeze a filesystem`,
			fsOnlyRefusalMessage(snapshotmode.ErrFilesystemOnlyFreezeUnsupported, shim, guest))
	})

	// An unknown agent reports an empty version, never the shim's.
	t.Run("an_unknown_agent_does_not_borrow_the_shim_version", func(t *testing.T) {
		message := fsOnlyRefusalMessage(snapshotmode.ErrFilesystemOnlyFreezeUnsupported, shim, sourceNone)
		require.Contains(t, message, "guest agent")
		require.NotContains(t, message, "v0.9.1")
	})
}

func TestShimSupportsCheckPauseAllowedPrefersTheDeclaration(t *testing.T) {
	require.True(t, shimSupportsCheckPauseAllowed(capability.Resolved{Source: capability.SourceNone}),
		"nothing known: attempt the preflight, an old shim falls through harmlessly")
	require.True(t, shimSupportsCheckPauseAllowed(
		resolvedShim(capability.SourceArtifact, capability.Set{capability.CapUpdateCheckPauseAllowed: 1})))
	require.False(t, shimSupportsCheckPauseAllowed(
		resolvedShim(capability.SourceLegacyTable, capability.Set{capability.CapUpdateSnapshotCapture: 1})),
		"a positive statement that the action is absent must not be called")
}

// A shim declaring errors.coded is judged by its code, not prose.
func TestClassifyShimErrorPrefersTheCode(t *testing.T) {
	coded := resolvedShim(capability.SourceArtifact, capability.Set{capability.CapErrorsCoded: 1})
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

	// An uncoded rejection from a coded build still triggers the retry.
	t.Run("unrecognised_code_falls_back_to_prose", func(t *testing.T) {
		require.Equal(t, shimErrorUnknownAction,
			classifyShimError(errors.New("unknown update ext action: SnapshotCapture"), coded),
			"an uncoded rejection from a coded build must still trigger the retry")
		require.Equal(t, shimErrorPauseBusy,
			classifyShimError(errors.New("sandbox pause forbidding, terminate exec tasks first"), coded))
	})

	// A reworded coded message is still decided by its code.
	t.Run("code_wins_over_wording", func(t *testing.T) {
		err := errors.New(capability.ErrCodeUnknownAction + ": reworded, no historical phrase")
		require.Equal(t, shimErrorUnknownAction, classifyShimError(err, coded))
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

func TestSandboxShimSelectedPathUsesOnlyRecordedAbsolutePaths(t *testing.T) {
	t.Run("runtime annotation_wins", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.AnnotationCubeletInternalRuntimePath: "/data/cubelet/root/component_versions/cube-shim/v0.8.0/bin/containerd-shim-cube-rs",
			},
		}}
		require.Equal(t, "/data/cubelet/root/component_versions/cube-shim/v0.8.0/bin/containerd-shim-cube-rs",
			sandboxShimSelectedPath(cb))
	})

	t.Run("absolute template path is used", func(t *testing.T) {
		cb := &cubeboxstore.CubeBox{LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{
					Path: "/usr/local/services/cubetoolbox/cube-shim/bin/containerd-shim-cube-rs",
				}},
			},
		}}
		require.Equal(t, "/usr/local/services/cubetoolbox/cube-shim/bin/containerd-shim-cube-rs",
			sandboxShimSelectedPath(cb))
	})

	t.Run("relative or missing path yields nothing", func(t *testing.T) {
		// A relative path names nothing on this host.
		require.Empty(t, sandboxShimSelectedPath(&cubeboxstore.CubeBox{LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{
					Path: "bin/containerd-shim-cube-rs",
				}},
			},
		}}))
		require.Empty(t, sandboxShimSelectedPath(&cubeboxstore.CubeBox{}))
		require.Empty(t, sandboxShimSelectedPath(nil))
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
		require.Equal(t, "/annotated/v0.9.0/bin/containerd-shim-cube-rs", sandboxShimSelectedPath(cb),
			"the annotation wins when all three are present")

		delete(cb.Annotations, constants.AnnotationCubeletInternalRuntimePath)
		require.Equal(t, "/template/v0.7.3/bin/containerd-shim-cube-rs", sandboxShimSelectedPath(cb),
			"the run template wins over the registry default")

		cb.LocalRunTemplate = nil
		require.Equal(t, "/ociruntime/v0.8.0/bin/containerd-shim-cube-rs", sandboxShimSelectedPath(cb),
			"OciRuntime.Path is the base the other two override, not something to skip")
	})
}

// writeCapabilityFile writes a component capability document into dir.
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

// installShimArtifactForTest creates a shim artifact tree carrying a declaration
// and returns its root, so a test can drive resolution from a real file.
func installShimArtifactForTest(t *testing.T, version string, caps map[string]uint32) string {
	t.Helper()
	root := t.TempDir()
	writeCapabilityFile(t, root, capability.ArtifactFileName, version, caps)
	return root
}

// pinShimArtifactForTest records the artifact path and version on a sandbox the
// way a create does, which is what resolution reads.
func pinShimArtifactForTest(cb *cubeboxstore.CubeBox, artifactRoot, version string) {
	cb.ComponentVersions = map[string]string{templatetypes.CubeComponentCubeShim: version}
	cb.LocalRunTemplate = &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{
			templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{
				Path:    filepath.Join(artifactRoot, "bin", "containerd-shim-cube-rs"),
				Version: version,
			}},
		},
	}
}
