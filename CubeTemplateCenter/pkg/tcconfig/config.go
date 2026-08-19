// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package tcconfig is the single authority for CubeTemplateCenter's environment
// variables.
//
// NAMING
// ------
// Everything this process reads is spelled CUBE_TEMPLATE_CENTER_*, matching the
// component directory name. Before this package existed the surface was three
// inconsistent families — CUBE_TC_*, CUBE_MASTER_*, CUBEMASTER_* — which made it
// impossible to tell from a deployment manifest which variables belonged to
// which process.
//
// BACKWARD COMPATIBILITY
// ----------------------
// Every legacy name is still honoured as a fallback, and using one records a
// deprecation notice retrievable via Warnings(). Design §5.3 requires this
// window explicitly: a deployment script that was not updated in lockstep would
// otherwise silently write artifacts to the wrong directory, which surfaces only
// as an artifact download 404 much later.
//
// TWO KINDS OF VARIABLE
// ---------------------
// Some are read by this module and can simply be renamed. Others are read by
// SHARED code under CubeMaster/pkg (the config loader, and the artifact path
// helpers used by both local and remote build modes), where renaming in place
// would change CubeMaster's own deployment contract. For those,
// ApplySharedEnvAliases translates the new name into the legacy one that the
// shared reader expects, and must run before anything reads them.
package tcconfig

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Variables read by shared CubeMaster code. These CANNOT be renamed at the read
// site — CubeMaster itself reads them — so ApplySharedEnvAliases bridges them.
const (
	// EnvConfigPath is the config file location. Note there is no -conf flag:
	// the process reuses CubeMaster's config loader, which only reads an
	// environment variable.
	EnvConfigPath       = "CUBE_TEMPLATE_CENTER_CONFIG_PATH"
	legacyEnvConfigPath = "CUBE_MASTER_CONFIG_PATH"

	// EnvArtifactStoreDir is where finished ext4 artifacts land. It MUST resolve
	// to the same directory CubeMaster serves downloads from (design §9.7).
	EnvArtifactStoreDir       = "CUBE_TEMPLATE_CENTER_ARTIFACT_STORE_DIR"
	legacyEnvArtifactStoreDir = "CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR"

	// EnvArtifactWorkDir is the scratch directory for layer unpacking. Unlike
	// the store dir this one is genuinely private to the building process.
	EnvArtifactWorkDir       = "CUBE_TEMPLATE_CENTER_ARTIFACT_WORK_DIR"
	legacyEnvArtifactWorkDir = "CUBEMASTER_ROOTFS_ARTIFACT_DIR"

	// EnvLoopMountExt4 selects the loop-mount ext4 path over native export.
	EnvLoopMountExt4       = "CUBE_TEMPLATE_CENTER_LOOP_MOUNT_EXT4_ENABLED"
	legacyEnvLoopMountExt4 = "CUBEMASTER_LOOP_MOUNT_EXT4_ENABLED"
)

// Variables read only by this module.
const (
	// EnvMasterEndpoint is where build results are reported. It deliberately
	// shares the name every other component (CubeOps, cubelet, ...) already
	// uses for "CubeMaster's address": CUBE_MASTER_ADDR means the same thing
	// everywhere, so a deployment sets one variable and all of them find
	// CubeMaster. The earlier per-component spellings
	// (CUBE_TEMPLATE_CENTER_MASTER_ENDPOINT, then CUBE_MASTER_ENDPOINT) are
	// kept only as fallbacks so an existing unit keeps working.
	EnvMasterEndpoint        = "CUBE_MASTER_ADDR"
	legacyEnvMasterEndpoint  = "CUBE_TEMPLATE_CENTER_MASTER_ENDPOINT"
	legacyEnvMasterEndpoint2 = "CUBE_MASTER_ENDPOINT"

	// EnvServeTemplateAPI mounts the public /cube/template* routes here instead
	// of leaving them to CubeMaster. Default false.
	EnvServeTemplateAPI       = "CUBE_TEMPLATE_CENTER_SERVE_TEMPLATE_API"
	legacyEnvServeTemplateAPI = "CUBE_TC_SERVE_TEMPLATE_API"

	EnvReconcileDisabled       = "CUBE_TEMPLATE_CENTER_RECONCILE_DISABLED"
	legacyEnvReconcileDisabled = "CUBE_TC_RECONCILE_DISABLED"

	EnvReconcileInterval       = "CUBE_TEMPLATE_CENTER_RECONCILE_INTERVAL"
	legacyEnvReconcileInterval = "CUBE_TC_RECONCILE_INTERVAL"

	EnvReconcileStaleAfter       = "CUBE_TEMPLATE_CENTER_RECONCILE_STALE_AFTER"
	legacyEnvReconcileStaleAfter = "CUBE_TC_RECONCILE_STALE_AFTER"

	// EnvNodeIP is this node's routable address, used for the log identity and
	// (conditionally, see ResolveHTTPBind) to narrow the HTTP listen address.
	//
	// Falls back to CUBE_SANDBOX_NODE_IP, which the one-click installer already
	// auto-detects into .one-click.env and which systemd loads for every unit.
	// Reusing it means a systemd deployment needs no new variable at all, and
	// the value is the same node IP the rest of the stack registers with.
	EnvNodeIP       = "CUBE_TEMPLATE_CENTER_NODE_IP"
	sharedEnvNodeIP = "CUBE_SANDBOX_NODE_IP"

	// EnvMaxConcurrentBuilds caps simultaneous builds. Unset or a non-positive
	// value means DefaultMaxConcurrentBuilds.
	EnvMaxConcurrentBuilds       = "CUBE_TEMPLATE_CENTER_MAX_CONCURRENT_BUILDS"
	legacyEnvMaxConcurrentBuilds = "CUBE_TC_MAX_CONCURRENT_BUILDS"
)

// defaultMasterEndpoint keeps a single-host deployment working with no
// configuration, which is the one-click layout.
const defaultMasterEndpoint = "http://localhost:8089"

// DefaultMaxConcurrentBuilds is the build concurrency cap when the env override
// is absent. Two concurrent mkfs runs already saturate a typical build host's
// disk and CPU; unbounded concurrency is the DoS surface the build endpoint
// must not expose.
const DefaultMaxConcurrentBuilds = 2

// MaxConcurrentBuilds returns the configured cap, falling back to
// DefaultMaxConcurrentBuilds for unset, unparsable, or non-positive input.
func MaxConcurrentBuilds() int {
	raw, found := lookup(EnvMaxConcurrentBuilds, legacyEnvMaxConcurrentBuilds)
	if !found {
		return DefaultMaxConcurrentBuilds
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return DefaultMaxConcurrentBuilds
	}
	return n
}

var (
	warnMu   sync.Mutex
	warnings []string
	warnSeen = map[string]struct{}{}
)

// warn records a notice for the caller to log once the logger exists.
//
// Deferred rather than logged inline because the earliest of these fire before
// log.Init: ApplySharedEnvAliases has to run before the config file is even
// located. Deduplicated by key so a variable read on every reconcile tick does
// not grow the slice without bound.
func warn(key, format string, args ...any) {
	warnMu.Lock()
	defer warnMu.Unlock()
	if _, seen := warnSeen[key]; seen {
		return
	}
	warnSeen[key] = struct{}{}
	warnings = append(warnings, fmt.Sprintf(format, args...))
}

// Warnings returns the accumulated deprecation and misconfiguration notices.
func Warnings() []string {
	warnMu.Lock()
	defer warnMu.Unlock()
	out := make([]string, len(warnings))
	copy(out, warnings)
	return out
}

// ResetWarningsForTest clears accumulated notices.
func ResetWarningsForTest() {
	warnMu.Lock()
	defer warnMu.Unlock()
	warnings = nil
	warnSeen = map[string]struct{}{}
}

// lookup reads newName, falling back to legacyName.
//
// Only a non-empty value counts as set: an empty variable is treated as absent
// so that `FOO=` in a systemd EnvironmentFile does not shadow the fallback.
func lookup(newName, legacyName string) (value string, found bool) {
	if v := strings.TrimSpace(os.Getenv(newName)); v != "" {
		return v, true
	}
	if legacyName == "" {
		return "", false
	}
	if v := strings.TrimSpace(os.Getenv(legacyName)); v != "" {
		warn("deprecated:"+legacyName,
			"environment variable %s is deprecated, use %s instead (the old name still works for now)",
			legacyName, newName)
		return v, true
	}
	return "", false
}

// boolValue parses the truthy spellings used across this repo's deploy scripts.
func boolValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ServePublicTemplateAPI reports whether the public /cube/template* control
// plane is served here.
//
// Default false: in the current iteration CubeMaster owns the template API and
// TC only builds what it is told. Serving them while CubeMaster still owns the
// flow would create a shadow entry point, with two processes writing the same
// tables concurrently.
func ServePublicTemplateAPI() bool {
	v, _ := lookup(EnvServeTemplateAPI, legacyEnvServeTemplateAPI)
	return boolValue(v)
}

// MasterEndpoint returns the CubeMaster base URL for status callbacks, with no
// trailing slash.
//
// CUBE_MASTER_ADDR wins; the two retired spellings are tried in turn so a
// deployment pinned to either keeps working. A value in a newer name never
// falls through to an older one.
func MasterEndpoint() string {
	for _, name := range []string{EnvMasterEndpoint, legacyEnvMasterEndpoint, legacyEnvMasterEndpoint2} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			if name != EnvMasterEndpoint {
				warn("deprecated:"+name,
					"environment variable %s is deprecated, use %s instead (the old name still works for now)",
					name, EnvMasterEndpoint)
			}
			return strings.TrimRight(v, "/")
		}
	}
	return defaultMasterEndpoint
}

// ReconcileDisabled reports whether the background sweep is switched off.
func ReconcileDisabled() bool {
	v, _ := lookup(EnvReconcileDisabled, legacyEnvReconcileDisabled)
	return boolValue(v)
}

// ReconcileInterval returns the raw sweep cadence override, if any. Parsing is
// left to the reconciler, which owns the default and the sanity bounds.
func ReconcileInterval() (string, bool) {
	return lookup(EnvReconcileInterval, legacyEnvReconcileInterval)
}

// ReconcileStaleAfter returns the raw abandoned-job threshold override, if any.
func ReconcileStaleAfter() (string, bool) {
	return lookup(EnvReconcileStaleAfter, legacyEnvReconcileStaleAfter)
}

// NodeIP returns this node's configured routable address, or "" when unset.
func NodeIP() string {
	if v := strings.TrimSpace(os.Getenv(EnvNodeIP)); v != "" {
		return v
	}
	// Not a deprecation: CUBE_SANDBOX_NODE_IP is a deployment-wide value owned
	// by the installer, so consuming it is intentional reuse rather than a name
	// this package is trying to retire.
	return strings.TrimSpace(os.Getenv(sharedEnvNodeIP))
}

// sharedEnvAliases maps a new name to the legacy name a shared reader expects.
var sharedEnvAliases = []struct{ newName, legacyName string }{
	{EnvConfigPath, legacyEnvConfigPath},
	{EnvArtifactStoreDir, legacyEnvArtifactStoreDir},
	{EnvArtifactWorkDir, legacyEnvArtifactWorkDir},
	{EnvLoopMountExt4, legacyEnvLoopMountExt4},
}

// ApplySharedEnvAliases publishes the CUBE_TEMPLATE_CENTER_* values under the
// legacy names that shared CubeMaster code reads.
//
// MUST be called before config.Init and before any artifact path is resolved:
// the config loader reads its variable during Init, and the path helpers cache
// nothing but are consulted on the first build.
//
// The new name always wins when both are set. That direction matters: a rollout
// that adds the new variable while an old unit file still exports the legacy one
// should follow the value the operator just wrote, and a disagreement about the
// artifact directory is loud rather than silent because the two processes ending
// up on different directories makes every download 404 with nothing in either
// log pointing at the cause.
func ApplySharedEnvAliases() {
	for _, alias := range sharedEnvAliases {
		newValue := strings.TrimSpace(os.Getenv(alias.newName))
		legacyValue := strings.TrimSpace(os.Getenv(alias.legacyName))

		switch {
		case newValue == "" && legacyValue == "":
			// Neither set: the shared reader falls back to its own default.
		case newValue == "":
			warn("deprecated:"+alias.legacyName,
				"environment variable %s is deprecated, use %s instead (the old name still works for now)",
				alias.legacyName, alias.newName)
		case legacyValue == "":
			_ = os.Setenv(alias.legacyName, newValue)
		case legacyValue != newValue:
			warn("conflict:"+alias.newName,
				"%s=%q overrides %s=%q; the two disagree, so verify the deployment exports only the new name",
				alias.newName, newValue, alias.legacyName, legacyValue)
			_ = os.Setenv(alias.legacyName, newValue)
		default:
			// Identical, nothing to do.
		}
	}
}

// localAddrChecker is swapped in tests; production always enumerates the real
// interfaces.
var localAddrChecker = isLocalAddr

// isLocalAddr reports whether ip is assigned to an interface on this host.
func isLocalAddr(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		// Cannot tell, so claim nothing: the caller keeps its current bind.
		return false
	}
	for _, addr := range addrs {
		switch v := addr.(type) {
		case *net.IPNet:
			if v.IP.Equal(ip) {
				return true
			}
		case *net.IPAddr:
			if v.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// ResolveHTTPBind decides the HTTP listen address, given what the config file
// asked for. It returns the address to use and an optional note explaining any
// deviation.
//
// The node IP narrows the bind ONLY when it is safe to do so, because getting
// this wrong costs a crash loop rather than a misconfiguration:
//
//   - An explicit http_bind in the config file always wins. Only the
//     post-default "0.0.0.0" is treated as "not chosen".
//   - The address must actually be assigned to this host. Inside a Kubernetes
//     Pod the node IP is not in the Pod's network namespace, so binding to it
//     would fail outright — and the readiness probe targets the Pod IP anyway.
//     Falling back to 0.0.0.0 there is both required and correct, so the check
//     makes the same manifest work in a Pod and under systemd.
//
// On a multi-NIC systemd host the node IP is local, so the listener narrows to
// the routable interface instead of every one of them.
func ResolveHTTPBind(configured string) (bind string, note string) {
	nodeIP := NodeIP()
	if nodeIP == "" {
		return configured, ""
	}

	trimmed := strings.TrimSpace(configured)
	if trimmed != "" && trimmed != "0.0.0.0" && trimmed != "::" {
		return configured, fmt.Sprintf(
			"%s=%s ignored for the listen address: http_bind=%q is set explicitly",
			EnvNodeIP, nodeIP, configured)
	}

	ip := net.ParseIP(nodeIP)
	if ip == nil {
		return configured, fmt.Sprintf(
			"%s=%q is not a valid IP address, keeping http_bind=%q",
			EnvNodeIP, nodeIP, configured)
	}
	if !localAddrChecker(ip) {
		return configured, fmt.Sprintf(
			"%s=%s is not assigned to this host (expected inside a Pod, where the "+
				"node IP is outside the Pod netns), keeping http_bind=%q",
			EnvNodeIP, nodeIP, configured)
	}
	return nodeIP, fmt.Sprintf("listening on %s from %s", nodeIP, EnvNodeIP)
}
