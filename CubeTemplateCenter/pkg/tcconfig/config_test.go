// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package tcconfig

import (
	"net"
	"strings"
	"testing"
)

// Environment variable names are a deployment contract: a rename that silently
// stops honouring the old spelling would make CubeTemplateCenter write artifacts
// somewhere CubeMaster cannot serve them, which shows up only as a download 404
// long after the build succeeded. So both the new names and the compatibility
// window are pinned here.

func clearEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
	}
	ResetWarningsForTest()
	t.Cleanup(ResetWarningsForTest)
}

func hasWarningAbout(needle string) bool {
	for _, w := range Warnings() {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}

func TestLegacyNamesStillWork(t *testing.T) {
	// The compatibility window required by design 5.3. Each legacy name must
	// still be honoured AND must announce itself, so an un-migrated deployment
	// keeps working but stops being invisible.
	t.Run("master-endpoint", func(t *testing.T) {
		clearEnv(t, EnvMasterEndpoint, legacyEnvMasterEndpoint)
		t.Setenv(legacyEnvMasterEndpoint, "http://legacy:8089")
		if got := MasterEndpoint(); got != "http://legacy:8089" {
			t.Fatalf("MasterEndpoint() = %q, want the legacy value", got)
		}
		if !hasWarningAbout(legacyEnvMasterEndpoint) {
			t.Fatalf("using %s must record a deprecation notice", legacyEnvMasterEndpoint)
		}
	})

	t.Run("reconcile-disabled", func(t *testing.T) {
		clearEnv(t, EnvReconcileDisabled, legacyEnvReconcileDisabled)
		t.Setenv(legacyEnvReconcileDisabled, "1")
		if !ReconcileDisabled() {
			t.Fatalf("the legacy %s must still be honoured", legacyEnvReconcileDisabled)
		}
	})
}

func TestNewNameWinsOverLegacy(t *testing.T) {
	clearEnv(t, EnvMasterEndpoint, legacyEnvMasterEndpoint)
	t.Setenv(legacyEnvMasterEndpoint, "http://legacy:8089")
	t.Setenv(EnvMasterEndpoint, "http://new:8089")
	if got := MasterEndpoint(); got != "http://new:8089" {
		t.Fatalf("MasterEndpoint() = %q, want the new name to win", got)
	}
	// No deprecation notice: the legacy value was never consulted, so warning
	// about it would be noise on a correctly migrated deployment that still has
	// a stale variable lying around.
	if hasWarningAbout(legacyEnvMasterEndpoint) {
		t.Fatalf("an unused legacy variable must not be reported: %v", Warnings())
	}
}

func TestEmptyValueDoesNotShadowFallback(t *testing.T) {
	// `FOO=` in a systemd EnvironmentFile or an empty Helm value must read as
	// absent, otherwise it would mask the legacy name and silently change
	// behaviour mid-migration.
	clearEnv(t, EnvMasterEndpoint, legacyEnvMasterEndpoint)
	t.Setenv(EnvMasterEndpoint, "   ")
	t.Setenv(legacyEnvMasterEndpoint, "http://legacy:8089")
	if got := MasterEndpoint(); got != "http://legacy:8089" {
		t.Fatalf("MasterEndpoint() = %q, want the legacy value to still apply", got)
	}
}

func TestMasterEndpointDefaultAndTrailingSlash(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"default", "", defaultMasterEndpoint},
		{"plain", "http://cubemaster:8089", "http://cubemaster:8089"},
		// A trailing slash would produce a double slash in the callback path.
		{"trailing-slash", "http://cubemaster:8089/", "http://cubemaster:8089"},
		{"multiple-slashes", "http://cubemaster:8089///", "http://cubemaster:8089"},
		{"padded", "  http://cubemaster:8089  ", "http://cubemaster:8089"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t, EnvMasterEndpoint, legacyEnvMasterEndpoint, legacyEnvMasterEndpoint2)
			if tc.raw != "" {
				t.Setenv(EnvMasterEndpoint, tc.raw)
			}
			if got := MasterEndpoint(); got != tc.want {
				t.Fatalf("MasterEndpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}

// CUBE_MASTER_ADDR is the one name every component uses for "CubeMaster's
// address". The two retired spellings are honoured only when it is unset, and
// never override it.
func TestMasterEndpointNamePrecedence(t *testing.T) {
	names := []string{EnvMasterEndpoint, legacyEnvMasterEndpoint, legacyEnvMasterEndpoint2}

	// Newest name wins over both legacy ones.
	clearEnv(t, names...)
	t.Setenv(legacyEnvMasterEndpoint, "http://legacy-one:8089")
	t.Setenv(legacyEnvMasterEndpoint2, "http://legacy-two:8089")
	t.Setenv(EnvMasterEndpoint, "http://current:8089")
	if got := MasterEndpoint(); got != "http://current:8089" {
		t.Fatalf("current name must win, got %q", got)
	}

	// First legacy name wins over the older one when the current name is unset.
	clearEnv(t, names...)
	t.Setenv(legacyEnvMasterEndpoint2, "http://legacy-two:8089")
	t.Setenv(legacyEnvMasterEndpoint, "http://legacy-one:8089")
	if got := MasterEndpoint(); got != "http://legacy-one:8089" {
		t.Fatalf("newer legacy name must win, got %q", got)
	}

	// Oldest legacy name still works on its own.
	clearEnv(t, names...)
	t.Setenv(legacyEnvMasterEndpoint2, "http://legacy-two:8089")
	if got := MasterEndpoint(); got != "http://legacy-two:8089" {
		t.Fatalf("oldest legacy name must still work, got %q", got)
	}

	// A legacy name must warn so operators migrate.
	if !hasWarningAbout(legacyEnvMasterEndpoint2) {
		t.Fatalf("using %s must produce a deprecation warning", legacyEnvMasterEndpoint2)
	}

	// A blank current name must NOT mask a legacy value -- it is "unset".
	clearEnv(t, names...)
	t.Setenv(EnvMasterEndpoint, "   ")
	t.Setenv(legacyEnvMasterEndpoint, "http://legacy-one:8089")
	if got := MasterEndpoint(); got != "http://legacy-one:8089" {
		t.Fatalf("blank current name must fall through to legacy, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Shared aliases
// ---------------------------------------------------------------------------

func TestApplySharedEnvAliasesPublishesLegacyName(t *testing.T) {
	// The point of the shim: shared CubeMaster code reads the legacy name, so
	// setting only the new one must still reach it.
	clearEnv(t, EnvArtifactStoreDir, legacyEnvArtifactStoreDir)
	t.Setenv(EnvArtifactStoreDir, "/data/CubeMaster/storage")

	ApplySharedEnvAliases()

	if got := envValue(legacyEnvArtifactStoreDir); got != "/data/CubeMaster/storage" {
		t.Fatalf("%s = %q, want the value published from %s",
			legacyEnvArtifactStoreDir, got, EnvArtifactStoreDir)
	}
}

func TestApplySharedEnvAliasesLeavesLegacyOnlyAlone(t *testing.T) {
	clearEnv(t, EnvConfigPath, legacyEnvConfigPath)
	t.Setenv(legacyEnvConfigPath, "/etc/cube/conf.yaml")

	ApplySharedEnvAliases()

	if got := envValue(legacyEnvConfigPath); got != "/etc/cube/conf.yaml" {
		t.Fatalf("an un-migrated deployment must keep working, got %q", got)
	}
	if !hasWarningAbout(legacyEnvConfigPath) {
		t.Fatalf("using %s must record a deprecation notice", legacyEnvConfigPath)
	}
}

func TestApplySharedEnvAliasesConflictIsLoud(t *testing.T) {
	// Two different artifact directories is the worst case this whole rename can
	// produce: TC builds into one, CubeMaster serves from the other, and every
	// download 404s with nothing in either log pointing at the cause. The new
	// name wins (it is what the operator just wrote) but the disagreement must
	// not be silent.
	clearEnv(t, EnvArtifactStoreDir, legacyEnvArtifactStoreDir)
	t.Setenv(EnvArtifactStoreDir, "/data/new/storage")
	t.Setenv(legacyEnvArtifactStoreDir, "/data/old/storage")

	ApplySharedEnvAliases()

	if got := envValue(legacyEnvArtifactStoreDir); got != "/data/new/storage" {
		t.Fatalf("%s = %q, want the new name to win", legacyEnvArtifactStoreDir, got)
	}
	if !hasWarningAbout("disagree") {
		t.Fatalf("a conflicting artifact directory must be reported, got %v", Warnings())
	}
}

func TestApplySharedEnvAliasesIdenticalValuesAreQuiet(t *testing.T) {
	clearEnv(t, EnvArtifactStoreDir, legacyEnvArtifactStoreDir)
	t.Setenv(EnvArtifactStoreDir, "/data/CubeMaster/storage")
	t.Setenv(legacyEnvArtifactStoreDir, "/data/CubeMaster/storage")

	ApplySharedEnvAliases()

	// A deployment exporting both with the same value is transitional but
	// correct; warning about it would train operators to ignore warnings.
	if len(Warnings()) != 0 {
		t.Fatalf("identical values must not warn, got %v", Warnings())
	}
}

func TestApplySharedEnvAliasesCoversEveryPair(t *testing.T) {
	// Guards against adding a shared variable to the constants without wiring it
	// into the shim, which would leave the new spelling silently inert.
	want := map[string]string{
		EnvConfigPath:       legacyEnvConfigPath,
		EnvArtifactStoreDir: legacyEnvArtifactStoreDir,
		EnvArtifactWorkDir:  legacyEnvArtifactWorkDir,
		EnvLoopMountExt4:    legacyEnvLoopMountExt4,
	}
	if len(sharedEnvAliases) != len(want) {
		t.Fatalf("sharedEnvAliases has %d entries, want %d", len(sharedEnvAliases), len(want))
	}
	for _, alias := range sharedEnvAliases {
		legacy, ok := want[alias.newName]
		if !ok {
			t.Fatalf("unexpected alias for %s", alias.newName)
		}
		if legacy != alias.legacyName {
			t.Fatalf("%s maps to %s, want %s", alias.newName, alias.legacyName, legacy)
		}
		if !strings.HasPrefix(alias.newName, "CUBE_TEMPLATE_CENTER_") {
			t.Fatalf("%s does not use the CUBE_TEMPLATE_CENTER_ prefix", alias.newName)
		}
	}
}

// ---------------------------------------------------------------------------
// Node IP
// ---------------------------------------------------------------------------

func TestNodeIPFallsBackToInstallerVariable(t *testing.T) {
	// CUBE_SANDBOX_NODE_IP is already auto-detected into .one-click.env and
	// loaded by every systemd unit, so a systemd deployment needs no new
	// variable at all.
	clearEnv(t, EnvNodeIP, sharedEnvNodeIP)
	t.Setenv(sharedEnvNodeIP, "10.0.0.10")
	if got := NodeIP(); got != "10.0.0.10" {
		t.Fatalf("NodeIP() = %q, want the installer-provided value", got)
	}

	t.Setenv(EnvNodeIP, "10.0.0.20")
	if got := NodeIP(); got != "10.0.0.20" {
		t.Fatalf("NodeIP() = %q, want the explicit value to win", got)
	}
}

func TestNodeIPUnsetIsEmpty(t *testing.T) {
	clearEnv(t, EnvNodeIP, sharedEnvNodeIP)
	if got := NodeIP(); got != "" {
		t.Fatalf("NodeIP() = %q, want empty when nothing is configured", got)
	}
}

func TestResolveHTTPBind(t *testing.T) {
	// Binding is the risky half of this feature: a wrong address is a crash
	// loop, not a misconfiguration. So the node IP narrows the listener only
	// when it demonstrably can.
	tests := []struct {
		name       string
		nodeIP     string
		configured string
		isLocal    bool
		wantBind   string
		wantNote   string
	}{
		{
			name:   "no-node-ip-changes-nothing",
			nodeIP: "", configured: "0.0.0.0", isLocal: true,
			wantBind: "0.0.0.0", wantNote: "",
		},
		{
			name:   "local-node-ip-narrows-the-bind",
			nodeIP: "10.0.0.10", configured: "0.0.0.0", isLocal: true,
			wantBind: "10.0.0.10", wantNote: "listening on",
		},
		{
			name:   "empty-config-is-also-unset",
			nodeIP: "10.0.0.10", configured: "", isLocal: true,
			wantBind: "10.0.0.10", wantNote: "listening on",
		},
		{
			// The Kubernetes case. The node IP is not in the Pod netns, so
			// binding to it would fail outright — and the readiness probe targets
			// the Pod IP anyway. Degrading keeps one manifest working everywhere.
			name:   "non-local-node-ip-keeps-wildcard",
			nodeIP: "10.0.0.10", configured: "0.0.0.0", isLocal: false,
			wantBind: "0.0.0.0", wantNote: "not assigned to this host",
		},
		{
			// An operator who pinned http_bind meant it.
			name:   "explicit-http-bind-wins",
			nodeIP: "10.0.0.10", configured: "127.0.0.1", isLocal: true,
			wantBind: "127.0.0.1", wantNote: "set explicitly",
		},
		{
			name:   "ipv6-wildcard-counts-as-unset",
			nodeIP: "10.0.0.10", configured: "::", isLocal: true,
			wantBind: "10.0.0.10", wantNote: "listening on",
		},
		{
			name:   "garbage-node-ip-keeps-configured",
			nodeIP: "not-an-ip", configured: "0.0.0.0", isLocal: true,
			wantBind: "0.0.0.0", wantNote: "not a valid IP address",
		},
		{
			// A hostname is a plausible mistake, and must not become a bind.
			name:   "hostname-keeps-configured",
			nodeIP: "cubemaster.internal", configured: "0.0.0.0", isLocal: true,
			wantBind: "0.0.0.0", wantNote: "not a valid IP address",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t, EnvNodeIP, sharedEnvNodeIP)
			if tc.nodeIP != "" {
				t.Setenv(EnvNodeIP, tc.nodeIP)
			}
			original := localAddrChecker
			localAddrChecker = func(net.IP) bool { return tc.isLocal }
			t.Cleanup(func() { localAddrChecker = original })

			bind, note := ResolveHTTPBind(tc.configured)
			if bind != tc.wantBind {
				t.Fatalf("ResolveHTTPBind(%q) bind = %q, want %q", tc.configured, bind, tc.wantBind)
			}
			if tc.wantNote == "" {
				if note != "" {
					t.Fatalf("expected no note, got %q", note)
				}
				return
			}
			if !strings.Contains(note, tc.wantNote) {
				t.Fatalf("note = %q, want it to mention %q", note, tc.wantNote)
			}
		})
	}
}

func TestIsLocalAddrRecognisesLoopback(t *testing.T) {
	// Sanity check on the real implementation: loopback is assigned on every
	// host this can run on, and a routable-but-unassigned address is not.
	if !isLocalAddr(net.ParseIP("127.0.0.1")) {
		t.Fatalf("127.0.0.1 must be recognised as local")
	}
	if isLocalAddr(net.ParseIP("203.0.113.1")) {
		t.Fatalf("a TEST-NET-3 address must not be recognised as local")
	}
}

// envValue reads a variable the same way lookup does, so a value that is only
// whitespace reads as absent.
func envValue(name string) string {
	v, _ := lookup(name, "")
	return v
}
