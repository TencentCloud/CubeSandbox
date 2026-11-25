// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"os"
	"path/filepath"
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
	// A resolution is counted every time Cubelet resolves a sandbox's shim capabilities.
	shimCapabilityResolutions metrics.LabeledCounter
	// Decision counts, by path and by the source that chose it.
	shimCapabilityDecisions metrics.LabeledCounter
)

func init() {
	ns := metrics.NewNamespace("cubebox", "shim", nil)
	shimCapabilityResolutions = ns.NewLabeledCounter(
		"capability_resolutions",
		"Resolutions of a sandbox shim's capabilities, by the source that answered.",
		"source",
	)
	shimCapabilityDecisions = ns.NewLabeledCounter(
		"capability_decisions",
		"Snapshot and pause path selections, by decision and the capability source that made it.",
		"decision", "source",
	)
	metrics.Register(ns)
}

// Decision labels for the capability metrics.
const (
	decisionCoordinatedCapture = "coordinated_capture"
	decisionLegacyCapture      = "legacy_capture"
	decisionFsOnlyRefused      = "fs_only_refused"
	decisionPausePreflight     = "pause_preflight"
)

// recordShimCapabilityResolution counts which source answered for a sandbox.
func recordShimCapabilityResolution(resolved capability.Resolved) {
	source := resolved.Source
	if source == "" {
		source = capability.SourceNone
	}
	shimCapabilityResolutions.WithValues(string(source)).Inc()
}

func recordShimCapabilityDecision(decision string, resolved capability.Resolved) {
	source := resolved.Source
	if source == "" {
		source = capability.SourceNone
	}
	shimCapabilityDecisions.WithValues(decision, string(source)).Inc()
}

// sandboxShimBundlePath returns the shim's bundle directory, or empty when unknown.
// The shim manager requires a namespaced context.
func (l *local) sandboxShimBundlePath(ctx context.Context, cb *cubeboxstore.CubeBox) string {
	for _, bundle := range resolveShimBundles(l, shimLookupContext(ctx, cb), cb) {
		if bundle != "" {
			return bundle
		}
	}
	return ""
}

// sandboxShimArtifactDir returns the released component root the sandbox's shim
// was started from, or empty without an absolute runtime path. The selection
// order must mirror withRuntimePathOpt in cube_container_create.go.
func sandboxShimArtifactDir(cb *cubeboxstore.CubeBox) string {
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
	// A relative path cannot be resolved to a component tree.
	if !strings.HasPrefix(selected, "/") {
		return ""
	}
	return capability.ArtifactDirFor(selected)
}

// resolveSandboxShimCapabilities resolves a sandbox's shim capabilities from the most trustworthy source.
func (l *local) resolveSandboxShimCapabilities(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	return resolveShimCapabilitiesFor(ctx, l, cb)
}

// Snapshot and pause use the same resolution as the create path.
func (s *service) resolveSandboxShimCapabilities(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	if s == nil || s.cubeboxMgr == nil {
		return resolveShimCapabilitiesFor(ctx, nil, cb)
	}
	return resolveShimCapabilitiesFor(ctx, s.cubeboxMgr, cb)
}

// resolveShimCapabilitiesFor is the single implementation behind both wrappers.
// bundles may be nil, in which case resolution starts from the artifact declaration.
func resolveShimCapabilitiesFor(ctx context.Context, bundles *local, cb *cubeboxstore.CubeBox) capability.Resolved {
	if cb == nil {
		return capability.Resolved{Source: capability.SourceNone}
	}
	bundle := ""
	if bundles != nil {
		bundle = bundles.sandboxShimBundlePath(ctx, cb)
	}
	artifact := sandboxShimArtifactDir(cb)
	version, versionSource := pinnedShimVersion(cb)

	resolved := capability.Resolve(bundle, artifact, version, func(source capability.Source, err error) {
		log.G(ctx).Warnf("sandbox %s: shim capability source %s is unusable, falling through: %v",
			sandboxIdentity(cb), source, err)
	})

	// The recorded pin only fills in the version for logging when the document omitted it.
	if resolved.Version == "" {
		resolved.Version = version
	}
	recordShimCapabilityResolution(resolved)
	log.G(ctx).Debugf("sandbox %s: shim capabilities source=%s version=%q pin_source=%s declared=%v runtime=%v",
		sandboxIdentity(cb), resolved.Source, resolved.Version, versionSource,
		resolved.Capabilities.Names(), resolved.Runtime.Names())

	// Persist the answer for restarts, later decisions and operator queries.
	cb.RecordResolvedShimCapabilities(resolved)
	return resolved
}

// sandboxShimCapabilities returns the recorded capabilities, resolving on first use.
func (s *service) sandboxShimCapabilities(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	if cb == nil {
		return capability.Resolved{Source: capability.SourceNone}
	}
	if recorded := cb.ResolvedShimCapabilities(); recorded.Source != "" {
		return recorded
	}
	return s.resolveSandboxShimCapabilities(ctx, cb)
}

// shimCapabilitiesForPause uses the same resolution as the snapshot paths.
func (s *service) shimCapabilitiesForPause(ctx context.Context, cb *cubeboxstore.CubeBox) capability.Resolved {
	return s.sandboxShimCapabilities(ctx, cb)
}

// refreshShimCapabilitiesAfterRestart re-reads the declaration after a Cubelet restart.
// The bundle file is authoritative after a restart.
func (l *local) refreshShimCapabilitiesAfterRestart(ctx context.Context, cb *cubeboxstore.CubeBox) {
	if cb == nil {
		return
	}
	before := cb.ShimCapabilitiesCopy()
	resolved := l.resolveSandboxShimCapabilities(ctx, cb)
	if resolved.Source == capability.SourceNone {
		return
	}
	if capabilitiesRecordEqual(before, cb.ShimCapabilitiesCopy()) {
		// Nothing changed: skip the write so recovery does not touch every row.
		return
	}
	if err := l.cubeboxManger.SyncByID(ctx, cb.ID); err != nil {
		log.G(ctx).Warnf("sandbox %s: persist shim capabilities after restart: %v", sandboxIdentity(cb), err)
	}
}

// capabilitiesRecordEqual compares two records by content, treating nil as no record.
func capabilitiesRecordEqual(a, b *cubeboxstore.ShimCapabilitiesRecord) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Source != b.Source || a.Version != b.Version {
		return false
	}
	return capability.Set(a.Capabilities).Equal(capability.Set(b.Capabilities)) &&
		capability.Set(a.Runtime).Equal(capability.Set(b.Runtime))
}

// capturePathDecision reports and records which capture path a full snapshot will take.
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

// fsOnlyRefusal reports why a filesystem-only capture cannot be attempted, or empty.
// Only a positive answer refuses up front: a known-missing shim capability or a
// guest refusal. SourceNone deliberately proceeds.
func fsOnlyRefusal(resolved capability.Resolved, mode snapshotmode.Mode) string {
	if !mode.IsFsOnly() {
		return ""
	}
	if resolved.Source != capability.SourceNone &&
		!resolved.Capabilities.Has(capability.CapUpdateSnapshotCaptureFSOnly, 1) {
		recordShimCapabilityDecision(decisionFsOnlyRefused, resolved)
		return snapshotmode.ErrFilesystemOnlyUnsupported
	}
	if resolved.Runtime.KnownUnsupported(capability.CapGuestFSFreeze, 1) {
		recordShimCapabilityDecision(decisionFsOnlyRefused, resolved)
		return snapshotmode.ErrFilesystemOnlyFreezeUnsupported
	}
	return ""
}

// shimSupportsCheckPauseAllowed reports whether the pause preflight may be called.
// Nothing known attempts the preflight; a declared lack of the action skips it.
func shimSupportsCheckPauseAllowed(resolved capability.Resolved) bool {
	if resolved.Source == capability.SourceNone {
		return true
	}
	return resolved.Capabilities.Has(capability.CapUpdateCheckPauseAllowed, 1)
}

// classifyShimError decides whether err rejects the coordinated capture action.
// With errors.coded the code decides; otherwise the legacy wording is matched here.
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

// shimRejectsCapture reports whether err rejects the coordinated capture action.
func shimRejectsCapture(err error, resolved capability.Resolved) bool {
	return classifyShimError(err, resolved) == shimErrorUnknownAction
}

// isPauseBusyReject reports a shim pause refusal in coded or legacy form.
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

// shimCapabilityFileExists reports whether a bundle carries a runtime declaration.
func shimCapabilityFileExists(bundleDir string) bool {
	if bundleDir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(bundleDir, capability.RuntimeFileName))
	return err == nil && info.Mode().IsRegular()
}
