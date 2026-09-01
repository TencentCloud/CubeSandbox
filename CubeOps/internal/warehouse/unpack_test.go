// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestUnpackOneClick_ShimFromManifestKernelHash(t *testing.T) {
	dir := t.TempDir()
	archive := buildFakeOneClick(t, dir, "v0.6.0")

	out, err := UnpackOneClick(archive, filepath.Join(dir, "work"))
	if err != nil {
		t.Fatalf("UnpackOneClick: %v", err)
	}
	got := map[string]string{}
	for _, item := range out {
		got[item.Component+"/"+item.Version] = item.Dir
		if err := ValidateInstalledTree(item.Dir, item.Component); err != nil {
			t.Errorf("validate %s: %v", item.Component, err)
		}
	}
	if _, ok := got["cube-shim/v0.6.0"]; !ok {
		t.Fatalf("missing shim from manifest, got %#v", got)
	}
	shimBin := filepath.Join(got["cube-shim/v0.6.0"], "bin", "containerd-shim-cube-rs")
	st, err := os.Stat(shimBin)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Fatalf("shim binary lost executable bit: %s", st.Mode())
	}
	rtBin := filepath.Join(got["cube-shim/v0.6.0"], "bin", "cube-runtime")
	st, err = os.Stat(rtBin)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Fatalf("cube-runtime lost executable bit: %s", st.Mode())
	}
	if _, ok := got["cube-image/v0.6.0"]; !ok {
		t.Fatalf("missing image, got %#v", got)
	}
	if _, ok := got["cube-agent/v0.6.0"]; !ok {
		t.Fatalf("missing agent, got %#v", got)
	}
	bm := filepath.Join(dir, "pkg", "cube-kernel-scf", "vmlinux-bm")
	short, err := ContentShortHash(bm)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["cube-kernel-scf/"+short]; !ok {
		t.Fatalf("missing kernel %s, got %#v", short, got)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := safeJoin(root, "../etc/passwd"); err == nil {
		t.Fatal("expected traversal reject")
	}
}

func TestFetchWhitelist(t *testing.T) {
	cfg := FetchConfig{GitHubRepos: []string{"TencentCloud/CubeSandbox"}}
	if allowedRepo(cfg.GitHubRepos, "evil/repo") {
		t.Fatal("evil repo allowed")
	}
	if !allowedRepo(cfg.GitHubRepos, "TencentCloud/CubeSandbox") {
		t.Fatal("official repo rejected")
	}
	err := cfg.DownloadRelease(SourceGitHub, "evil/repo", "v0.6.0", "amd64", filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatal("expected whitelist error")
	}
}

func buildFakeOneClick(t *testing.T, dir, tag string) string {
	t.Helper()
	pkg := filepath.Join(dir, "pkg")
	mustMkdir(t, filepath.Join(pkg, "cube-shim", "bin"))
	mustWriteMode(t, filepath.Join(pkg, "cube-shim", "bin", "containerd-shim-cube-rs"), "shim", 0o755)
	mustWriteMode(t, filepath.Join(pkg, "cube-shim", "bin", "cube-runtime"), "runtime", 0o755)
	mustMkdir(t, filepath.Join(pkg, "cube-image"))
	mustWrite(t, filepath.Join(pkg, "cube-image", "cube-guest-image-cpu.img"), "img")
	mustWrite(t, filepath.Join(pkg, "cube-image", "version"), tag+"\n")
	mustMkdir(t, filepath.Join(pkg, "cube-agent"))
	mustWrite(t, filepath.Join(pkg, "cube-agent", "cube-agent.ext4"), "agent")
	mustWrite(t, filepath.Join(pkg, "cube-agent", "version"), tag+"\n")
	mustMkdir(t, filepath.Join(pkg, "cube-kernel-scf"))
	mustWrite(t, filepath.Join(pkg, "cube-kernel-scf", "vmlinux-bm"), "kernel-bm-bytes")

	inner := filepath.Join(dir, "sandbox-package.tar.gz")
	tarGzDir(t, inner, pkg, "sandbox-package")

	outerDir := filepath.Join(dir, "outer")
	mustMkdir(t, filepath.Join(outerDir, "assets", "package"))
	if err := copyRegularFile(inner, filepath.Join(outerDir, "assets", "package", "sandbox-package.tar.gz")); err != nil {
		t.Fatal(err)
	}
	manifest := ReleaseManifest{
		Components: map[string]manifestComponent{
			"containerd-shim-cube-rs": {Version: tag},
		},
		GuestImage: manifestGuestImage{Version: tag, AgentVersion: tag},
	}
	raw, _ := json.Marshal(manifest)
	mustWrite(t, filepath.Join(outerDir, "release-manifest.json"), string(raw))

	archive := filepath.Join(dir, "one-click.tar.gz")
	tarGzDir(t, archive, outerDir, "")
	return archive
}

func tarGzDir(t *testing.T, dest, src, prefix string) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		name := rel
		if prefix != "" {
			if rel == "." {
				name = prefix
			} else {
				name = prefix + "/" + filepath.ToSlash(rel)
			}
		} else if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(name)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	mustWriteMode(t, path, body, 0o644)
}

func mustWriteMode(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedDownloadHost(t *testing.T) {
	if allowedDownloadHost("169.254.169.254") || allowedDownloadHost("127.0.0.1") {
		t.Fatal("loopback/metadata host allowed")
	}
	if !allowedDownloadHost("github.com") || !allowedDownloadHost("objects.githubusercontent.com") {
		t.Fatal("github hosts rejected")
	}
	if !allowedDownloadHost("cnb.cool") || !allowedDownloadHost("cdn.cnb.cool") {
		t.Fatal("cnb hosts rejected")
	}
}

func TestKernelShortHashStable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vmlinux-bm")
	mustWrite(t, p, "kernel-bm-bytes")
	sum := sha256.Sum256([]byte("kernel-bm-bytes"))
	want := "sha256-" + hex.EncodeToString(sum[:])[:12]
	got, err := ContentShortHash(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
