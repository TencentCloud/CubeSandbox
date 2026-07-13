package nodemeta

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestLoadDeclaredVersionsFromPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "release-manifest.json")
	data := []byte(`{
  "components": {
    "cubelet": {"version": "v1.2.3"},
    "cube-agent": {"version": "component-agent-version"}
  },
  "guest_image": {
    "version": "guest-v1",
    "agent_version": "guest-agent-v1"
  },
  "kernel": {
    "version": "kernel-v1",
    "pvm_version": "kernel-pvm-v1",
    "vmlinux_digest_sha256": "sha256:ordinary",
    "vmlinux_pvm_digest_sha256": "sha256:pvm"
  }
}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	got := loadDeclaredVersionsFromPath(path)
	want := map[string]string{
		"cubelet":     "v1.2.3",
		"cube-agent":  "guest-agent-v1",
		"guest-image": "guest-v1",
		"kernel":      "kernel-v1@sha256:ordinary",
	}
	for component, version := range want {
		if got[component] != version {
			t.Fatalf("declared[%s] = %q, want %q", component, got[component], version)
		}
	}

	info := loadDeclaredVersionInfoFromPath(path)
	if _, ok := info.Sets["kernel"]["kernel-v1@sha256:ordinary"]; !ok {
		t.Fatalf("kernel ordinary version missing from declared set: %#v", info.Sets["kernel"])
	}
	if _, ok := info.Sets["kernel"]["kernel-pvm-v1@sha256:pvm"]; !ok {
		t.Fatalf("kernel PVM version missing from declared set: %#v", info.Sets["kernel"])
	}
	if len(info.Sets["cube-agent"]) != 1 {
		t.Fatalf("cube-agent declared set should be replaced by guest_image.agent_version, got %#v", info.Sets["cube-agent"])
	}
	if _, ok := info.Sets["cube-agent"]["guest-agent-v1"]; !ok {
		t.Fatalf("cube-agent declared set should contain guest agent version, got %#v", info.Sets["cube-agent"])
	}
}

func TestLoadDeclaredVersionsFromPathMissingManifest(t *testing.T) {
	got := loadDeclaredVersionsFromPath(filepath.Join(t.TempDir(), "missing.json"))
	if len(got) != 0 {
		t.Fatalf("expected empty map for missing manifest, got %#v", got)
	}
}

func TestVersionIsDeclaredForKernelVariants(t *testing.T) {
	primary := map[string]string{"kernel": "kernel-v1@sha256:ordinary"}
	sets := map[string]map[string]struct{}{
		"kernel": {
			"kernel-v1@sha256:ordinary": {},
			"kernel-pvm-v1@sha256:pvm":  {},
		},
	}

	for _, actual := range []string{"kernel-v1@sha256:ordinary", "kernel-pvm-v1@sha256:pvm"} {
		if !versionIsDeclared("kernel", actual, primary, sets) {
			t.Fatalf("expected kernel version %q to be declared", actual)
		}
	}
	for _, actual := range []string{"kernel-v1@sha256:other", "unknown", ""} {
		if versionIsDeclared("kernel", actual, primary, sets) {
			t.Fatalf("kernel version %q must not be declared", actual)
		}
	}
}

func TestVersionIsDeclaredIgnoresPlatformSuffix(t *testing.T) {
	primary := map[string]string{"cube-egress": "v0.5.0"}
	sets := map[string]map[string]struct{}{
		"cube-egress": {
			"v0.5.0": {},
		},
	}

	for _, actual := range []string{"v0.5.0", "v0.5.0-arm64", "v0.5.0-amd64", "v0.5.0-aarch64", "v0.5.0-x86_64", "v0.5.0-arm64-amd64"} {
		assert.True(t, versionIsDeclared("cube-egress", actual, primary, sets), "platform-specific version %q should match declaration", actual)
	}
	for _, actual := range []string{"v0.5.0-dev", "v0.5.0-rc1", "v0.5.1-arm64", "v0.5.0-arm64-fips", "unknown", ""} {
		assert.False(t, versionIsDeclared("cube-egress", actual, primary, sets), "version %q must not match declaration v0.5.0", actual)
	}
}

func TestVersionIsDeclaredPlatformSuffixCaseSensitive(t *testing.T) {
	primary := map[string]string{"cube-egress": "v0.5.0"}
	sets := map[string]map[string]struct{}{
		"cube-egress": {"v0.5.0": {}},
	}

	assert.False(
		t,
		versionIsDeclared("cube-egress", "v0.5.0-ARM64", primary, sets),
		"platform suffix matching is case-sensitive by design",
	)
}

func TestVersionIsDeclaredUsesPrimaryWhenSetMissing(t *testing.T) {
	primary := map[string]string{"cube-egress": "v0.5.0"}

	assert.True(t, versionIsDeclared("cube-egress", "v0.5.0-arm64", primary, nil))
	assert.False(t, versionIsDeclared("cube-egress", "v0.5.0-dev", primary, nil))
}

func TestStripPlatformVersionSuffix(t *testing.T) {
	assert.Equal(t, "v0.5.0", stripPlatformVersionSuffix("v0.5.0-arm64-amd64"))
	assert.Equal(t, "v0.5.0-arm64-fips", stripPlatformVersionSuffix("v0.5.0-arm64-fips"))
	assert.Equal(t, "v0.5.0-ARM64", stripPlatformVersionSuffix("v0.5.0-ARM64"))
}

func TestBuildVersionMatrixUsesDeclaredDistribution(t *testing.T) {
	declared := map[string]string{
		"cubelet":     "v1.0.0",
		"cube-egress": "v0.5.0",
		"kernel":      "kernel-v1@sha256:ordinary",
	}
	declaredSets := map[string]map[string]struct{}{
		"cubelet": {
			"v1.0.0": {},
		},
		"cube-egress": {
			"v0.5.0": {},
		},
		"kernel": {
			"kernel-v1@sha256:ordinary": {},
			"kernel-pvm-v1@sha256:pvm":  {},
		},
	}
	rows := []*models.NodeComponentVersion{
		{NodeID: "node-bm", Component: "cubelet", Version: "v1.0.0"},
		{NodeID: "node-bm", Component: "cube-egress", Version: "v0.5.0-arm64"},
		{NodeID: "node-bm", Component: "kernel", Version: "kernel-v1@sha256:ordinary"},
		{NodeID: "node-pvm", Component: "cubelet", Version: "v1.1.0-test"},
		{NodeID: "node-pvm", Component: "kernel", Version: "kernel-pvm-v1@sha256:pvm"},
	}

	matrix := buildVersionMatrix(rows, map[string]bool{"node-bm": true, "node-pvm": true}, declared, declaredSets)
	cubelet := findComponentRow(t, matrix, "cubelet")
	if cubelet.Consistent {
		t.Fatalf("cubelet should record multi-version distribution")
	}
	if cubelet.DeclaredVersion != "v1.0.0" {
		t.Fatalf("cubelet declared version = %q, want v1.0.0", cubelet.DeclaredVersion)
	}

	kernel := findComponentRow(t, matrix, "kernel")
	if len(kernel.DeclaredVersions) != 2 {
		t.Fatalf("kernel should expose both BM/PVM declarations, got %#v", kernel.DeclaredVersions)
	}
	if !findNodeComponent(t, matrix, "node-bm", "kernel").Declared {
		t.Fatalf("ordinary kernel identity should be declared")
	}
	if !findNodeComponent(t, matrix, "node-pvm", "kernel").Declared {
		t.Fatalf("PVM kernel identity should be declared")
	}
	egress := findNodeComponent(t, matrix, "node-bm", "cube-egress")
	if !egress.Declared {
		t.Fatalf("platform-specific cube-egress version should match the release declaration")
	}
	if egress.Version != "v0.5.0-arm64" {
		t.Fatalf("cube-egress matrix should preserve the reported version, got %q", egress.Version)
	}
	if findNodeComponent(t, matrix, "node-pvm", "cubelet").Declared {
		t.Fatalf("cubelet test build should be marked undeclared")
	}
}

func findComponentRow(t *testing.T, matrix *VersionMatrix, component string) ComponentMatrixRow {
	t.Helper()
	for _, row := range matrix.Components {
		if row.Component == component {
			return row
		}
	}
	t.Fatalf("component %q not found in matrix: %#v", component, matrix.Components)
	return ComponentMatrixRow{}
}

func findNodeComponent(t *testing.T, matrix *VersionMatrix, nodeID, component string) NodeComponentEntry {
	t.Helper()
	for _, row := range matrix.Nodes {
		if row.NodeID != nodeID {
			continue
		}
		for _, entry := range row.Components {
			if entry.Component == component {
				return entry
			}
		}
	}
	t.Fatalf("component %q for node %q not found in matrix: %#v", component, nodeID, matrix.Nodes)
	return NodeComponentEntry{}
}

func TestKernelArtifactIdentityUsesDigestWhenTagUnknown(t *testing.T) {
	tests := []struct {
		name   string
		tag    string
		digest string
		want   string
	}{
		{name: "tag and digest", tag: "kernel-v1", digest: "sha256:kernel", want: "kernel-v1@sha256:kernel"},
		{name: "unknown tag uses digest", tag: "unknown", digest: "sha256:kernel", want: "sha256:kernel"},
		{name: "empty tag uses digest", tag: "", digest: "sha256:kernel", want: "sha256:kernel"},
		{name: "missing digest uses tag", tag: "kernel-v1", digest: "", want: "kernel-v1"},
		{name: "all missing", tag: "unknown", digest: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kernelArtifactIdentity(tt.tag, tt.digest); got != tt.want {
				t.Fatalf("kernelArtifactIdentity(%q, %q)=%q, want %q", tt.tag, tt.digest, got, tt.want)
			}
		})
	}
}
