// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeCAForBakeTest produces a freshly self-signed CA in PEM. Standalone
// helper here so this file doesn't reach into the cube_egress_ca package
// internals.
func makeCAForBakeTest(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// withTempCubeEgressCAPath redirects cubeEgressCAPath to a temp file
// for the duration of the test. The const itself is package-private,
// so we override via a package-level pointer that loadCubeEgressCA
// reads. Keep this scope to the test file.
//
// We don't have a pointer today; the const is hardcoded. To make the
// path test-overridable without polluting production code, we accept
// a small indirection: tests that want to control the path use
// loadCubeEgressCAFromPath, a sibling that takes the path explicitly.
// production loadCubeEgressCA wraps it.
//
// (see cube_egress_ca_bake.go after this commit lands the tweak)

func TestResolveWithCubeCA(t *testing.T) {
	if !resolveWithCubeCA(nil) {
		t.Fatal("nil should resolve to true (server-side default)")
	}
	tr := true
	if !resolveWithCubeCA(&tr) {
		t.Fatal("explicit true should stay true")
	}
	fa := false
	if resolveWithCubeCA(&fa) {
		t.Fatal("explicit false should stay false")
	}
}

func TestLoadCubeEgressCAWithCubeCAFalseIsNoop(t *testing.T) {
	// withCubeCA=false MUST be a no-op even if the canonical CA file
	// happens to exist on the host. The contract is "user said no, we
	// don't read the file". Fingerprint comes back empty so the spec
	// fingerprint stays in legacy form.
	pem, fp, err := loadCubeEgressCA(context.Background(), false)
	if err != nil {
		t.Fatalf("err=%v, want nil for false", err)
	}
	if pem != nil || fp != "" {
		t.Fatalf("got pem=%v fp=%q, want both empty", pem, fp)
	}
}

func TestLoadCubeEgressCAFromPathHardErrorOnMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.crt")
	_, _, err := loadCubeEgressCAFromPath(context.Background(), true, missing)
	if err == nil {
		t.Fatal("expected hard error on missing CA when withCubeCA=true")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err=%v should mention 'missing'", err)
	}
}

func TestLoadCubeEgressCAFromPathHardErrorOnInvalidPEM(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.crt")
	if err := os.WriteFile(bad, []byte("not pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadCubeEgressCAFromPath(context.Background(), true, bad)
	if err == nil {
		t.Fatal("expected hard error on invalid PEM")
	}
}

func TestLoadCubeEgressCAFromPathHappyPath(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, makeCAForBakeTest(t), 0o644); err != nil {
		t.Fatal(err)
	}
	pem, fp, err := loadCubeEgressCAFromPath(context.Background(), true, caFile)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(pem) == 0 {
		t.Fatal("expected PEM bytes returned")
	}
	if len(fp) != 64 { // sha256 hex
		t.Fatalf("fingerprint=%q is not sha256 hex", fp)
	}
}

func TestApplyCubeEgressCAToRootfsNoopWhenPemEmpty(t *testing.T) {
	res, err := applyCubeEgressCAToRootfs(context.Background(), t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.Baked || res.TargetsWritten != 0 {
		t.Fatalf("res=%+v, want zero", res)
	}
}

func TestApplyCubeEgressCAToRootfsSeedsDistroless(t *testing.T) {
	// Empty rootfs (no bundle, no anchor dir) → distroless equivalent.
	// withCubeCA=true semantics: caller asked for trust to be installed;
	// the bake seeds a canonical bundle so the root still lands instead
	// of failing the build.
	root := t.TempDir()
	caPEM := makeCAForBakeTest(t)
	res, err := applyCubeEgressCAToRootfs(context.Background(), root, caPEM, "")
	if err != nil {
		t.Fatalf("expected distroless rootfs to be seeded, got err=%v", err)
	}
	if !res.Baked || !res.Seeded {
		t.Fatalf("res=%+v, want Baked=true Seeded=true", res)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")); err != nil {
		t.Fatalf("seeded bundle missing: %v", err)
	}
}

func TestApplyCubeEgressCAToRootfsHappyPath(t *testing.T) {
	root := t.TempDir()
	// Plant a Debian-style bundle so the bake has somewhere to write.
	if err := os.MkdirAll(filepath.Join(root, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	caPEM := makeCAForBakeTest(t)
	res, err := applyCubeEgressCAToRootfs(context.Background(), root, caPEM, "")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !res.Baked {
		t.Fatal("Baked=false; expected the bundle to be written")
	}
}
