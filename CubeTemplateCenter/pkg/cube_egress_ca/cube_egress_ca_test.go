// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube_egress_ca

import (
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

// makeCA returns a freshly-minted self-signed CA in PEM, plus its DER
// for tests that want to plant a "preexisting" entry into a bundle.
func makeCA(t *testing.T, cn string) (pemBytes, der []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	pemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return pemBytes, der
}

// mustWrite mkdir-p the parent then writes content. Test helper for
// building synthetic rootfs trees.
func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir parent of %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func mustMkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
}

func mustReadFile(t *testing.T, full string) string {
	t.Helper()
	b, err := os.ReadFile(full) // #nosec G304 — test helper
	if err != nil {
		t.Fatalf("read %s: %v", full, err)
	}
	return string(b)
}

func TestBakeRejectsEmptyRootfs(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	if _, err := Bake("", caPEM); err == nil {
		t.Fatal("Bake(\"\", caPEM) err=nil, want error")
	}
}

func TestBakeRejectsNonDirRootfs(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	tmp := filepath.Join(t.TempDir(), "imafile")
	if err := os.WriteFile(tmp, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Bake(tmp, caPEM); err == nil {
		t.Fatal("Bake on a file: err=nil, want error")
	}
}

func TestBakeRejectsInvalidPEM(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range [][]byte{
		nil,
		[]byte("   "),
		[]byte("not pem at all"),
		// PEM block with wrong type:
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
	} {
		if _, err := Bake(dir, bad); err == nil {
			t.Fatalf("Bake with invalid PEM %q: err=nil, want error", bad)
		}
	}
}

func TestBakeRejectsMultiCertPEM(t *testing.T) {
	caA, _ := makeCA(t, "ca-a")
	caB, _ := makeCA(t, "ca-b")
	combined := append(append([]byte{}, caA...), caB...)
	if _, err := Bake(t.TempDir(), combined); err == nil {
		t.Fatal("multi-cert PEM should be rejected (intermediate chain not in scope)")
	}
}

// Debian-style image: ca-certificates installed, both bundle and
// anchor dir exist. Expect both to be touched.
func TestBakeDebianStyle(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	root := t.TempDir()

	// Pre-existing bundle containing one Mozilla-ish certificate.
	pre, _ := makeCA(t, "preexisting-mozilla")
	mustWrite(t, root, "etc/ssl/certs/ca-certificates.crt", string(pre))
	mustMkdir(t, root, "usr/local/share/ca-certificates")

	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("Bake err=%v", err)
	}
	if !res.Baked {
		t.Fatal("Baked=false, want true (bundle + anchor both available)")
	}
	if res.TargetsWritten != 2 {
		t.Fatalf("TargetsWritten=%d, want 2 (bundle + anchor)", res.TargetsWritten)
	}

	// Bundle file still contains the pre-existing entry AND our new CA.
	bundle := mustReadFile(t, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"))
	if !strings.Contains(bundle, string(pre)) {
		t.Fatal("preexisting cert was overwritten/lost")
	}
	if !strings.Contains(bundle, string(caPEM)) {
		t.Fatal("our CA was not appended to the bundle")
	}

	// Anchor file is exactly our CA.
	anchor := mustReadFile(t, filepath.Join(root, "usr/local/share/ca-certificates", AnchorFileName))
	if anchor != string(caPEM) {
		t.Fatalf("anchor file content mismatch:\ngot=%q\nwant=%q", anchor, caPEM)
	}

	if res.Fingerprint == "" {
		t.Fatal("Fingerprint is empty")
	}
	if len(res.Fingerprint) != 64 { // sha256 hex
		t.Fatalf("Fingerprint=%q is not sha256 hex", res.Fingerprint)
	}
}

// RHEL-style image: only ca-trust-extracted bundle and anchor dir.
func TestBakeRHELStyle(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	root := t.TempDir()

	pre, _ := makeCA(t, "preexisting-mozilla")
	mustWrite(t, root, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", string(pre))
	mustMkdir(t, root, "etc/pki/ca-trust/source/anchors")

	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("Bake err=%v", err)
	}
	if !res.Baked || res.TargetsWritten != 2 {
		t.Fatalf("res=%+v, want Baked=true TargetsWritten=2", res)
	}
	bundle := mustReadFile(t, filepath.Join(root, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"))
	if !strings.Contains(bundle, string(pre)) || !strings.Contains(bundle, string(caPEM)) {
		t.Fatal("RHEL bundle missing pre-existing or new CA")
	}
}

// Image with both Debian-style and RHEL-style files (e.g. a custom
// base image that touches both ecosystems). Every available target
// should be hit.
func TestBakeMultiFamily(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	root := t.TempDir()

	pre, _ := makeCA(t, "preexisting")
	mustWrite(t, root, "etc/ssl/certs/ca-certificates.crt", string(pre))
	mustWrite(t, root, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", string(pre))
	mustWrite(t, root, "etc/pki/tls/certs/ca-bundle.crt", string(pre))
	mustMkdir(t, root, "usr/local/share/ca-certificates")
	mustMkdir(t, root, "etc/pki/ca-trust/source/anchors")

	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("Bake err=%v", err)
	}
	if res.TargetsWritten != 5 {
		t.Fatalf("TargetsWritten=%d, want 5 (3 bundles + 2 anchors)", res.TargetsWritten)
	}
}

// Distroless / scratch-style image: nothing for the bake to append to.
// Instead of failing, the bake SEEDS the canonical bundle from scratch
// with the CubeEgress root (safe under the egress-MITM model), so the
// trust root still lands.
func TestBakeDistroless(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	root := t.TempDir() // empty rootfs

	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("Bake err=%v", err)
	}
	if !res.Baked {
		t.Fatal("Baked=false on empty rootfs; want true (seeded canonical bundle)")
	}
	if !res.Seeded {
		t.Fatal("Seeded=false; want true (image had no trust store)")
	}
	if res.TargetsWritten != 1 {
		t.Fatalf("TargetsWritten=%d, want 1 (seeded bundle)", res.TargetsWritten)
	}
	// The seeded bundle exists at the canonical path and is exactly our CA.
	seeded := mustReadFile(t, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"))
	if seeded != string(caPEM) {
		t.Fatalf("seeded bundle content mismatch:\ngot=%q\nwant=%q", seeded, caPEM)
	}
	if res.Fingerprint == "" {
		t.Fatal("Fingerprint empty on seeded bake; reuse cache would lose CA-rotation invalidation")
	}

	// Re-bake must be idempotent: the seeded bundle now contains the CA,
	// so the second pass writes nothing and does not seed again.
	res2, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("second bake err=%v", err)
	}
	if res2.Baked || res2.Seeded || res2.TargetsWritten != 0 {
		t.Fatalf("re-bake of seeded rootfs not idempotent: res=%+v", res2)
	}
	seededAgain := mustReadFile(t, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"))
	if seededAgain != string(caPEM) {
		t.Fatal("seeded bundle mutated on idempotent re-bake")
	}
}

// Idempotence: re-baking with the same CA does not append again.
// Critical because buildRootfsArtifact's redo path can re-enter the
// same rootfs.
func TestBakeIdempotent(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	root := t.TempDir()
	pre, _ := makeCA(t, "preexisting")
	mustWrite(t, root, "etc/ssl/certs/ca-certificates.crt", string(pre))
	mustMkdir(t, root, "usr/local/share/ca-certificates")

	if _, err := Bake(root, caPEM); err != nil {
		t.Fatalf("first bake: %v", err)
	}
	bundleAfterFirst := mustReadFile(t, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"))
	anchorAfterFirst := mustReadFile(t, filepath.Join(root, "usr/local/share/ca-certificates", AnchorFileName))

	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("second bake: %v", err)
	}
	bundleAfterSecond := mustReadFile(t, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"))
	anchorAfterSecond := mustReadFile(t, filepath.Join(root, "usr/local/share/ca-certificates", AnchorFileName))

	if bundleAfterFirst != bundleAfterSecond {
		t.Fatalf("bundle changed on idempotent re-bake:\n first=%q\n second=%q",
			bundleAfterFirst, bundleAfterSecond)
	}
	if anchorAfterFirst != anchorAfterSecond {
		t.Fatalf("anchor changed on idempotent re-bake")
	}
	// Result on second pass: nothing new written, but reasons populated.
	if res.TargetsWritten != 0 {
		t.Fatalf("second bake TargetsWritten=%d, want 0 (idempotent)", res.TargetsWritten)
	}
	// Baked stays false because no work was done THIS pass. The caller
	// should consult the persisted artifact row to know whether the
	// rootfs actually carries the CA.
	if res.Baked {
		t.Fatal("Baked=true on idempotent re-bake; want false (no writes this pass)")
	}
}

// Whitespace-tolerant idempotence: even if the bundle was rewritten
// with subtly different newlines, we still recognise our cert.
func TestBakeIdempotentDespiteWhitespace(t *testing.T) {
	caPEM, derWanted := makeCA(t, "cube-egress-root")
	root := t.TempDir()

	// Plant the CA into the bundle but with weird whitespace mixed in.
	bundleContent := "# header comment\r\n" + string(caPEM) + "\n\n# trailing\n"
	mustWrite(t, root, "etc/ssl/certs/ca-certificates.crt", bundleContent)

	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("Bake: %v", err)
	}
	if res.TargetsWritten != 0 {
		t.Fatalf("TargetsWritten=%d; bake should have detected our CA already present despite whitespace", res.TargetsWritten)
	}
	bundle := mustReadFile(t, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"))
	if bundle != bundleContent {
		t.Fatal("bundle mutated when it shouldn't have")
	}
	// Sanity: the planted block really did contain our DER.
	if !bundleContainsDER([]byte(bundleContent), derWanted) {
		t.Fatal("test setup bug: planted bundle doesn't contain wanted DER")
	}
}

// Bundle without trailing newline: the appender must insert one so
// the new PEM block doesn't get glued onto the previous line.
func TestBakeAppendsNewlineSeparator(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	root := t.TempDir()
	pre, _ := makeCA(t, "pre")
	// Strip trailing newline.
	preNoNL := strings.TrimRight(string(pre), "\n")
	mustWrite(t, root, "etc/ssl/certs/ca-certificates.crt", preNoNL)

	if _, err := Bake(root, caPEM); err != nil {
		t.Fatalf("Bake: %v", err)
	}
	got := mustReadFile(t, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"))
	if !strings.Contains(got, "-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----") {
		t.Fatalf("expected newline between consecutive PEM blocks, got:\n%s", got)
	}
}

// Anchor write replaces a divergent existing file (could happen if
// some prior process planted a different CA at the same name).
func TestBakeAnchorReplacesDivergent(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	root := t.TempDir()

	mustMkdir(t, root, "usr/local/share/ca-certificates")
	mustWrite(t, root, filepath.Join("usr/local/share/ca-certificates", AnchorFileName), "stale\n")

	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("Bake: %v", err)
	}
	if !res.Baked {
		t.Fatal("Baked=false; expected the divergent anchor to be replaced")
	}
	got := mustReadFile(t, filepath.Join(root, "usr/local/share/ca-certificates", AnchorFileName))
	if got != string(caPEM) {
		t.Fatalf("anchor not overwritten with our CA:\ngot=%q", got)
	}
}

// FingerprintOf can be called without a rootfs (used by the
// fingerprint composer at request time).
func TestFingerprintOfStable(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	fp1, err := FingerprintOf(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := FingerprintOf(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable: %s vs %s", fp1, fp2)
	}
	// Different CA → different fingerprint.
	otherPEM, _ := makeCA(t, "other-root")
	fpOther, err := FingerprintOf(otherPEM)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fpOther {
		t.Fatal("two distinct CAs share a fingerprint; sha256 collision or test bug")
	}
}

// FingerprintOf is whitespace-tolerant: the same DER re-encoded with
// different line endings yields the same fingerprint.
func TestFingerprintOfIgnoresPEMWhitespace(t *testing.T) {
	caPEM, _ := makeCA(t, "cube-egress-root")
	fpA, err := FingerprintOf(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	// Mangle whitespace.
	mangled := strings.ReplaceAll(string(caPEM), "\n", "\r\n")
	fpB, err := FingerprintOf([]byte(mangled))
	if err != nil {
		t.Fatal(err)
	}
	if fpA != fpB {
		t.Fatalf("FingerprintOf differs on cosmetic re-encoding: %s vs %s", fpA, fpB)
	}
}
