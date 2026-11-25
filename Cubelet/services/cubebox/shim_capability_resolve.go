// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"strings"

	"github.com/docker/go-metrics"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/capability"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

var (
	// A resolution is counted every time Cubelet works out what a sandbox's shim
	// or guest agent can do. The source label is the useful part: a cluster still
	// reporting `legacy-table` for live sandboxes has not finished rolling the
	// artifact out, and `none` means those sandboxes keep the pre-mechanism
	// behaviour.
	shimCapabilityResolutions metrics.LabeledCounter
	// Decision counts, by path and by the source that chose it. Only decisions
	// the capability mechanism actually makes are counted, so the sum is a direct
	// answer to "how many captures took each path".
	shimCapabilityDecisions metrics.LabeledCounter
)

func init() {
	ns := metrics.NewNamespace("cubebox", "shim", nil)
	shimCapabilityResolutions = ns.NewLabeledCounter(
		"capability_resolutions",
		"Resolutions of a sandbox component's capabilities, by the source that answered.",
		"component", "source",
	)
	shimCapabilityDecisions = ns.NewLabeledCounter(
		"capability_decisions",
		"Snapshot and pause path selections, by decision and the capability source that made it.",
		"decision", "source",
	)
	metrics.Register(ns)
}

// Decision labels. Kept as constants so the metric vocabulary is reviewable in
// one place instead of being scattered as string literals at the call sites.
const (
	decisionCoordinatedCapture = "coordinated_capture"
	decisionLegacyCapture      = "legacy_capture"
	decisionFsOnlyRefused      = "fs_only_refused"
	decisionPausePreflight     = "pause_preflight"
)

// Component labels for the resolution counter.
const (
	componentShim  = "cube-shim"
	componentAgent = "cube-agent"
)

func recordCapabilityResolution(component string, resolved capability.Resolved) {
	source := resolved.Source
	if source == "" {
		source = capability.SourceNone
	}
	shimCapabilityResolutions.WithValues(component, string(source)).Inc()
}

func recordShimCapabilityDecision(decision string, resolved capability.Resolved) {
	source := resolved.Source
	if source == "" {
		source = capability.SourceNone
	}
	shimCapabilityDecisions.WithValues(decision, string(source)).Inc()
}

// componentCapabilityCache is the node-wide answer table. Capabilities are a pure
// function of the artifact tree and the pinned version, both fixed when a sandbox
// is created, so one entry serves every sandbox started from that build.
var componentCapabilityCache = capability.NewCache()

// sandboxShimSelectedPath returns the absolute path the sandbox's shim was started
// from, or "" when the sandbox carries none.
//
// The precedence must mirror withRuntimePathOpt in cube_container_create.go, which
// is what selects the shim binary. A mismatch would read another build's
// declaration. The live toolbox path is not a source: an upgraded node points it
// at a newer shim.
//
// This is the cache key's path component, so it stays the recorded string and is
// never canonicalised against the filesystem.
func sandboxShimSelectedPath(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	// Same order, last writer wins, as withRuntimePathOpt.
	var selected string
	if cb.OciRuntime != nil && cb.OciRuntime.Path != "" {
		selected = cb.OciRuntime.Path
	}
	if cb.LocalRunTemplate != nil {
		if shim, ok := cb.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeShim]; ok {
			if path := strings.TrimSpace(shim.Component.Path); path != "" {
				selected = path
			}
		}
	}
	if path := strings.TrimSpace(cb.Annotations[constants.AnnotationCubeletInternalRuntimePath]); path != "" {
		selected = path
	}
	if !strings.HasPrefix(selected, "/") {
		return ""
	}
	return selected
}

// sandboxAgentSelectedPath returns the absolute path to the cube-agent payload the
// sandbox boots with. The agent path is fixed at create time and only recorded in
// the run template; the live toolbox would describe whatever is installed now.
func sandboxAgentSelectedPath(cb *cubeboxstore.CubeBox) string {
	if cb == nil || cb.LocalRunTemplate == nil {
		return ""
	}
	agent, ok := cb.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeAgent]
	if !ok {
		return ""
	}
	path := strings.TrimSpace(agent.Component.Path)
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

// resolveSandboxShimCapabilities resolves a sandbox's shim capabilities from the
// most trustworthy source available.
func (l *local) resolveSandboxShimCapabilities(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	return resolveShimCapabilitiesFor(ctx, cb)
}

// Snapshot and pause use the same resolution as the create path, so the path a
// capture takes and the path a pause takes can never be chosen from two different
// answers.
func (s *service) resolveSandboxShimCapabilities(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	return resolveShimCapabilitiesFor(ctx, cb)
}

// resolveShimCapabilitiesFor is the single implementation behind both wrappers.
// Only the artifact and the frozen table are consulted: nothing is published by a
// running shim.
func resolveShimCapabilitiesFor(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	if cb == nil {
		return capability.Resolved{Source: capability.SourceNone}
	}
	selected := sandboxShimSelectedPath(cb)
	version, versionSource := pinnedShimVersion(cb)

	resolved := componentCapabilityCache.Get(componentShim, selected, version,
		func() capability.Resolved {
			return capability.Resolve(capability.ArtifactDirFor(selected), version,
				func(source capability.Source, err error) {
					log.G(ctx).Warnf("sandbox %s: shim capability source %s is unusable, falling through: %v",
						sandboxIdentity(cb), source, err)
				})
		})

	if resolved.Version == "" {
		resolved.Version = version
	}
	recordCapabilityResolution(componentShim, resolved)
	log.G(ctx).Debugf("sandbox %s: shim capabilities source=%s version=%q pin_source=%s declared=%v",
		sandboxIdentity(cb), resolved.Source, resolved.Version, versionSource,
		resolved.Capabilities.Names())

	return resolved
}

// resolveSandboxGuestCapabilities resolves what the guest agent behind one sandbox
// can do, using the agent artifact of the version the sandbox was created with.
func (s *service) resolveSandboxGuestCapabilities(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	return resolveGuestCapabilitiesFor(ctx, cb)
}

// resolveSandboxGuestCapabilities is the create path's entry point for the same
// resolution, so a capture decision and a create-time record cannot disagree.
func (l *local) resolveSandboxGuestCapabilities(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	return resolveGuestCapabilitiesFor(ctx, cb)
}

func resolveGuestCapabilitiesFor(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	if cb == nil {
		return capability.Resolved{Source: capability.SourceNone}
	}
	selected := sandboxAgentSelectedPath(cb)
	version, versionSource := pinnedComponentVersion(cb, templatetypes.CubeComponentCubeAgent)

	resolved := componentCapabilityCache.Get(componentAgent, selected, version,
		func() capability.Resolved {
			return capability.ResolveGuest(capability.ArtifactDirFor(selected), version,
				func(source capability.Source, err error) {
					log.G(ctx).Warnf("sandbox %s: agent capability source %s is unusable, falling through: %v",
						sandboxIdentity(cb), source, err)
				})
		})
	if resolved.Version == "" {
		resolved.Version = version
	}
	recordCapabilityResolution(componentAgent, resolved)
	log.G(ctx).Debugf("sandbox %s: agent capabilities source=%s version=%q pin_source=%s declared=%v",
		sandboxIdentity(cb), resolved.Source, resolved.Version, versionSource,
		resolved.Capabilities.Names())
	return resolved
}

// shimCapabilitiesForPause uses the same resolution as the snapshot paths: the
// node-wide cache answers, so no per-sandbox record is read or written.
func (s *service) shimCapabilitiesForPause(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	return s.resolveSandboxShimCapabilities(ctx, cb)
}

// capturePathDecision reports which capture path a full snapshot will take, and
// records it. Keeping the decision and the log line in one function means the
// metric cannot describe a path other than the one the caller then takes.
func capturePathDecision(logger snapshotPathLogger, resolved capability.Resolved) bool {
	coordinated := resolved.Capabilities.Has(capability.CapUpdateSnapshotCapture, 1)
	decision := decisionLegacyCapture
	if coordinated {
		decision = decisionCoordinatedCapture
	}
	recordShimCapabilityDecision(decision, resolved)
	if !coordinated {
		logSnapshotPathSelection(logger, resolved)
	}
	return coordinated
}

// fsOnlyRefusal reports why a filesystem-only capture cannot be attempted, or ""
// when it may proceed. Two independent questions: does the shim implement the mode,
// and can the guest agent freeze a filesystem.
//
// Only a positive answer refuses up front. SourceNone (a component that predates
// the mechanism, or an unordered version) proceeds: the capture is attempted and
// the artifact inspected afterwards, which is the pre-mechanism behaviour.
func fsOnlyRefusal(shim capability.Resolved, guest capability.Resolved, mode snapshotmode.Mode) string {
	if !mode.IsFsOnly() {
		return ""
	}
	if shim.Source != capability.SourceNone &&
		!shim.Capabilities.Has(capability.CapUpdateSnapshotCaptureFSOnly, 1) {
		recordShimCapabilityDecision(decisionFsOnlyRefused, shim)
		return snapshotmode.ErrFilesystemOnlyUnsupported
	}
	if guest.Source != capability.SourceNone &&
		!guest.Capabilities.Has(capability.CapGuestFSFreeze, 1) {
		recordShimCapabilityDecision(decisionFsOnlyRefused, guest)
		return snapshotmode.ErrFilesystemOnlyFreezeUnsupported
	}
	return ""
}

// shimSupportsCheckPauseAllowed reports whether the pause preflight may be called.
// Nothing known means attempt it: an older shim rejects the action and falls through
// to PauseToSnapshot at the cost of one round trip.
func shimSupportsCheckPauseAllowed(resolved capability.Resolved) bool {
	if resolved.Source == capability.SourceNone {
		return true
	}
	return resolved.Capabilities.Has(capability.CapUpdateCheckPauseAllowed, 1)
}

// classifyShimError reports how err should be classified. With `errors.coded` the
// code decides and the prose is never read; the code is matched anywhere because
// Cubelet wraps the error before this sees it. Without it, the wording match below
// is the only option and is confined to this function.
func classifyShimError(err error, resolved capability.Resolved) shimErrorKind {
	if err == nil {
		return shimErrorNone
	}
	message := err.Error()
	if resolved.Capabilities.Has(capability.CapErrorsCoded, 1) {
		switch {
		case strings.Contains(message, capability.ErrCodePauseBusy):
			return shimErrorPauseBusy
		case strings.Contains(message, capability.ErrCodeUnknownAction):
			return shimErrorUnknownAction
		default:
			return shimErrorOther
		}
	}
	// Legacy adapter: the only place that matches prose.
	if strings.Contains(message, capability.LegacyUnknownActionFragment+shimSnapshotCaptureAction) {
		return shimErrorUnknownAction
	}
	if isPauseBusyReject(message) {
		return shimErrorPauseBusy
	}
	return shimErrorOther
}

type shimErrorKind int

const (
	shimErrorNone shimErrorKind = iota
	// shimErrorUnknownAction is the shim refusing the capture action itself.
	shimErrorUnknownAction
	// shimErrorPauseBusy is open exec sessions blocking a pause.
	shimErrorPauseBusy
	shimErrorOther
)

// shimRejectsCapture reports whether err is the shim refusing the coordinated
// capture action because it does not implement it.
func shimRejectsCapture(err error, resolved capability.Resolved) bool {
	return classifyShimError(err, resolved) == shimErrorUnknownAction
}

// isPauseBusyReject reports a shim pause refusal. It understands both the coded
// form and the pre-capability wording, so it works across a rolling upgrade in
// either direction.
func isPauseBusyReject(message string) bool {
	if capability.CodedErrorCode(message) == capability.ErrCodePauseBusy {
		return true
	}
	lower := strings.ToLower(message)
	for _, fragment := range capability.LegacyPauseBusyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}
