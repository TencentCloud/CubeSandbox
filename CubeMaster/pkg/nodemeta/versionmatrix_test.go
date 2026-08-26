package nodemeta

import (
	"os"
	"path/filepath"
	"testing"
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
