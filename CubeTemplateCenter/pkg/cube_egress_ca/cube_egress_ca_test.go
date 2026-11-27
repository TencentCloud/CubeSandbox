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

// Test vectors generated with OpenSSL 3.x `openssl x509 -subject_hash_old`
// (X509_NAME_hash_old, the MD5-based convention Android's trust stores
// use; see opensslSubjectHashOld). Subject shapes: single CN; O+CN
// (multi RDN); C/ST/L/O/OU/CN (six RDNs). The SHA1-based
// `openssl x509 -subject_hash` values for the same certs are
// 0x43203952 / 0x3330afd4 / 0x101024f9 — Android does not read those.
var subjectHashOldVectors = []struct {
	name string
	pem  string
	hash uint32
}{
	{
		name: "single CN",
		pem: `-----BEGIN CERTIFICATE-----
MIIDDjCCAfagAwIBAgITK01MJagyN/WtCMt79QuhqXg6vTANBgkqhkiG9w0BAQsF
ADAXMRUwEwYDVQQDDAxUZXN0IFJvb3QgQ0EwHhcNMjYxMDE1MDg1OTU1WhcNMjYx
MTE0MDg1OTU1WjAXMRUwEwYDVQQDDAxUZXN0IFJvb3QgQ0EwggEiMA0GCSqGSIb3
DQEBAQUAA4IBDwAwggEKAoIBAQCZ0SrWe4jSxmBozUMYb9AgRb8E2/abxO/6gODS
3wrIY8GrpNZ+cni5dSgJ57P+BVP3OLLc1Upyjp4JWoKrcQrNmauJboL/PKO7RFCE
BgTWTWpjian9lUpz40GM45X2IBWzfCd0cTtR+/R6zKHyNjR0Vk6fJVmsHB6bzSEb
AWjAHKBV2IaMxIO33YqtBJlJy85lL30lvLZGJtbQkkiywZQSw23XlCdVfc6yivHO
aXGJ3kCVmqpBg8GgkbpeNm+oeWnWEXVeUZEXp8WFwe+vy+RwfSBq2rpkrmDEglWz
8AMeaBsBVd1UhCVpPpGLdx1YG55DousBvm6MqLYcyrOhCBg3AgMBAAGjUzBRMB0G
A1UdDgQWBBQ2sebnET8NCP+X13PKTEm7e+ewNjAfBgNVHSMEGDAWgBQ2sebnET8N
CP+X13PKTEm7e+ewNjAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUAA4IB
AQB2winrAxjXIbtKQshnGk+vbP9/CWzh1rkHafhgciNd1XnP6ejvGxr4hNiC51L7
rKt1JcmtX70KWIUX9EF5jis33535zMtPM9bWbtl/qXJwuqNsD//Yrb5tsFLNaQAG
03W4JNHxiZCSMuzQNiCsvnlL8pG2Rj4ewPcao7hejhTgx/JXHBFWAQekGrXFLwJI
N8Asf92BmH5N4AbZ+thShPdFO4w7sXnHmcQY/egNC5ZQw3VTZLPKGJSzo114R8tu
RRyD7FUq3+SAU4PM1EpDN/Ah1HYS500Tc1QtOtl70y1+JURSJsbzU76iRDFEswDc
yoKrYRyh/Zb6ZhLbsJtgT5v4
-----END CERTIFICATE-----`,
		hash: 0x13fbaf3f,
	},
	{
		name: "O + CN",
		pem: `-----BEGIN CERTIFICATE-----
MIIDQTCCAimgAwIBAgIULizYQ2ti0rLac/14MXf1Pu5ZMmYwDQYJKoZIhvcNAQEL
BQAwMDEVMBMGA1UECgwMVGVzdCBPcmcgTHRkMRcwFQYDVQQDDA5TZWNvbmQgUm9v
dCBDQTAeFw0yNjEwMTUwOTI2MzlaFw0yNjExMTQwOTI2MzlaMDAxFTATBgNVBAoM
DFRlc3QgT3JnIEx0ZDEXMBUGA1UEAwwOU2Vjb25kIFJvb3QgQ0EwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQCrhE0Eo0hV31KBDZB1hEZpJa+VcuzlCev7
P3r6YQM4eAdOuhuSB3Xxe7IuG8qcRw4YzdSvLd1dTmhAMaDHH/6/Nw2IiPc2fyow
kWVBRJ8hYRI7FSg3nlQP6zArO/D/LiPzlYGKU79YJiQ+mwftSWBe+vrCOkdy0Inx
YRc0pbXWDvVEgY7IVbrQUosCf0Mf7EYU6Mo4c+BLgTW0Ypr04bHTFeZ3QaegKcDR
bsyKOUm4lpdPBBGhdOR1qreDvrjvl1Mtrf8xdSnGWHE7UdfDDIr7qXz4UiCPqrGX
R7XmYuqH5RvZX3XgHDzzD8SXJVGJGA1to2wWvh1/lhddsOe5hxNrAgMBAAGjUzBR
MB0GA1UdDgQWBBTw4V3YL0eTMAhjdcB2Usccjuv7rjAfBgNVHSMEGDAWgBTw4V3Y
L0eTMAhjdcB2Usccjuv7rjAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUA
A4IBAQAdZO9NYMldZKtHgV3jRxc7gLIg9kPJ5mXPAHqbao20b5rXfL8mDTiRE/oA
9NXHotr5mNSc8/5cXCz0abh+CRnufI2mEc/KwwtlLX04p9ZGX4jZb1tly3OOg5mQ
hnTOcYvRqrLgsLkdLtEANYt8AYjuESL+W/SrfROAjehW7pUSYfoF+UBR0z2MYJkQ
760wjO7VzbG4b3aoHKBgbvwQZZqbxGKlSBmDcCeYF3DqsanVuh5xaXkOufbS2RUV
Wlz418iFkb9ZFqw9uzSZRR6VUPa+DBu5mMLhLrByCzLbQc5Z/w/cPwY7LcoGdh4h
wGgoybf3zSGfzZaH0OHjb2SPZAPm
-----END CERTIFICATE-----`,
		hash: 0xf19be30c,
	},
	{
		name: "C/ST/L/O/OU/CN",
		pem: `-----BEGIN CERTIFICATE-----
MIID0TCCArmgAwIBAgIUJADgX3+Xn6RhZIebftF2N1JzEOQwDQYJKoZIhvcNAQEL
BQAweDELMAkGA1UEBhMCQ04xEjAQBgNVBAgMCUd1YW5nZG9uZzERMA8GA1UEBwwI
U2hlbnpoZW4xEDAOBgNVBAoMB1RlbmNlbnQxFDASBgNVBAsMC0N1YmVTYW5kYm94
MRowGAYDVQQDDBFNdWx0aSBSRE4gVGVzdCBDQTAeFw0yNjEwMTUwODU5NTVaFw0y
NjExMTQwODU5NTVaMHgxCzAJBgNVBAYTAkNOMRIwEAYDVQQIDAlHdWFuZ2Rvbmcx
ETAPBgNVBAcMCFNoZW56aGVuMRAwDgYDVQQKDAdUZW5jZW50MRQwEgYDVQQLDAtD
dWJlU2FuZGJveDEaMBgGA1UEAwwRTXVsdGkgUkROIFRlc3QgQ0EwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQCR9a9wna5LAbfffhxXzhBzjxs8zYgOtNn3
yM3pVWjnmJjdEmIvNp+dryrUeWrhMp1IOnnBS+GgbOJFLzyhU9V/Vrdp+nrOUd1b
TJOBSmd7rWpEuABTL/uHgJe4w+r0TvNYsT4MAh9r3cE7M3e2xMiOPGxXcbKoPXoW
9qPTh3Alm0wyUp7XGAee6zUrijt9k5G7bjzHX7AGFSh8dBJuD0FamuSt1eAJzylt
GgGmeDnqrytFnJDKwSCYWkmfvVizSEl6B7p93SeNWKmZ0XRDgeVRJYiWWxGhKD5C
g7lCKoYCD406QV00tyNuFxpIUUnCRmFl7BJVFyNAmfK/h3SUOjpbAgMBAAGjUzBR
MB0GA1UdDgQWBBTmmDBkFqSL4Rfkc8ULEjod2rpI4zAfBgNVHSMEGDAWgBTmmDBk
FqSL4Rfkc8ULEjod2rpI4zAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUA
A4IBAQCKUO83Qpv9In+4FrvnTZMFTp6TztjmPvQamIkzjFzo5X7raMNQz+3a5lgu
LD+fMuCXjs6BZG/ZkGH7NbypZrvTKWOq2o+bc5l1obOSGPDGKh8BiXVInB1NBKr5
yTSrK/P7yCtOP7GVtCBg7mPxwA3N2H0d2CX11OK6/v6pZbsDZ3aO8frMNqw8Xt8Y
YCeo+bSB+skgJx0bkzDy/9Zt+es6LPX5oLDmx+YIXWeYXeSxVSrM7jZlRMm++BSl
7PlcW6rgBYNUZLhNgO7+P+FmgpklUxxjGCFvrpP/u5P2rFCBcrbCqEBAC4tJ2SCc
atzG+Xu+JPNiwGGsLfkNFxpunyXJ
-----END CERTIFICATE-----`,
		hash: 0xadd262b0,
	},
}

func TestOpenSSLSubjectHashOldVectors(t *testing.T) {
	for _, tc := range subjectHashOldVectors {
		t.Run(tc.name, func(t *testing.T) {
			block, _ := pem.Decode([]byte(tc.pem))
			if block == nil {
				t.Fatalf("vector PEM does not decode")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatalf("vector PEM does not parse: %v", err)
			}
			got := opensslSubjectHashOld(cert)
			if got != tc.hash {
				t.Fatalf("subject hash = %08x, want %08x (openssl x509 -subject_hash_old)", got, tc.hash)
			}
		})
	}
}

func TestBakeAndroidAnchorUsesSubjectHashOldName(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, root, "system/etc/security/cacerts")
	mustMkdir(t, root, "system/apex/com.android.conscrypt/cacerts")

	vector := subjectHashOldVectors[0] // CN=Test Root CA, hash_old 13fbaf3f
	res, err := Bake(root, []byte(vector.pem))
	if err != nil {
		t.Fatalf("Bake: %v", err)
	}
	if !res.Baked {
		t.Fatalf("expected Baked=true, got %+v", res)
	}
	// Both Android store locations must carry the anchor under the
	// X509_NAME_hash_old name: the conscrypt APEX copy is what
	// Android 14+ reads, the classic path serves older releases and
	// legacy tooling.
	for _, dir := range []string{
		"system/etc/security/cacerts",
		"system/apex/com.android.conscrypt/cacerts",
	} {
		wantName := filepath.Join(root, dir, "13fbaf3f.0")
		if _, err := os.Stat(wantName); err != nil {
			t.Fatalf("Android anchor must be written as <subject_hash_old>.0 in %s: %v", dir, err)
		}
		if _, err := os.Stat(filepath.Join(root, dir, AnchorFileName)); err == nil {
			t.Fatalf("Android trust store %s must not carry the generic %s name", dir, AnchorFileName)
		}
	}
}

func TestBakeAbsoluteHostSymlinkNeverReachesHost(t *testing.T) {
	// etc -> <absolute build-host path> (like Android's /etc ->
	// /system/etc in shape, but pointing at a location that exists on
	// the host and is NOT inside the rootfs). Resolution must rebase the
	// target into the rootfs: the host side is never consulted, the
	// tempting host bundle is neither read nor modified, and the bake
	// succeeds with the seed landing at the guest-visible location
	// inside the rootfs.
	root := t.TempDir()
	outside := t.TempDir()

	// The "host" side carries a bundle file to tempt the append.
	if err := os.MkdirAll(filepath.Join(outside, "ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostBundle := filepath.Join(outside, "ssl/certs/ca-certificates.crt")
	if err := os.WriteFile(hostBundle, []byte("existing host content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}

	caPEM, _ := makeCA(t, "Escape Test CA")
	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("host-pointing symlink must be rebased, not a hard error: %v", err)
	}
	if !res.Baked || !res.Seeded {
		t.Fatalf("expected Baked=true Seeded=true (seed at rebased target), got %+v", res)
	}
	if got := mustReadFile(t, hostBundle); got != "existing host content" {
		t.Fatalf("host file was read or modified through the symlink: %q", got)
	}
	// The seed must land at the rebased target inside the rootfs
	// (rootfs/<full absolute target>/ssl/certs/ca-certificates.crt),
	// which is exactly what the guest itself would see through the
	// link — not in the build host's directory.
	seeded := filepath.Join(root, outside, "ssl/certs/ca-certificates.crt")
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("seed must land inside the rootfs at the rebased target %q: %v", seeded, err)
	}
}

func TestBakeAndroidEtcSymlinkResolvesInRootfs(t *testing.T) {
	// Android layout: /etc -> /system/etc (absolute symlink), and the
	// bundle lives under /system/etc/ssl/certs. Resolution must follow
	// the link *inside the rootfs* and append to the real file, the
	// same file the guest reaches through its own /etc.
	root := t.TempDir()
	mustWrite(t, root, "system/etc/ssl/certs/ca-certificates.crt", "-----BEGIN CERTIFICATE-----\n")
	if err := os.Symlink("/system/etc", filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}

	caPEM, der := makeCA(t, "Android Etc Test CA")
	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("Android /etc symlink must resolve inside the rootfs: %v", err)
	}
	if !res.Baked {
		t.Fatalf("expected Baked=true, got %+v", res)
	}
	got := mustReadFile(t, filepath.Join(root, "system/etc/ssl/certs/ca-certificates.crt"))
	if !bundleContainsDER([]byte(got), der) {
		t.Fatalf("appended bundle must contain the CA DER through the /etc symlink")
	}
}

func TestBakeRHLEAbsoluteSymlinkResolvesInRootfs(t *testing.T) {
	// RHEL/Fedora layout: etc/pki/tls/certs/ca-bundle.crt is an absolute
	// symlink to /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem, and
	// the build host (also RHEL-family) happens to carry the same path
	// with different content. The append must resolve the link inside
	// the rootfs and write the in-rootfs target — never the host's
	// file. Previously this hard-failed as a symlink escape.
	root := t.TempDir()
	host := t.TempDir()
	mustMkdir(t, host, "etc/pki/ca-trust/extracted/pem")
	hostBundle := filepath.Join(host, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem")
	if err := os.WriteFile(hostBundle, []byte("host bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, root, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "-----BEGIN CERTIFICATE-----\n")
	mustMkdir(t, root, "etc/pki/tls/certs")
	if err := os.Symlink("/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		filepath.Join(root, "etc/pki/tls/certs/ca-bundle.crt")); err != nil {
		t.Fatal(err)
	}

	caPEM, der := makeCA(t, "RHEL Symlink Test CA")
	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("RHEL absolute symlink must resolve inside the rootfs: %v", err)
	}
	if !res.Baked {
		t.Fatalf("expected Baked=true, got %+v", res)
	}
	got := mustReadFile(t, filepath.Join(root, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"))
	if !bundleContainsDER([]byte(got), der) {
		t.Fatalf("in-rootfs symlink target must contain the appended CA")
	}
	if got := mustReadFile(t, hostBundle); got != "host bundle" {
		t.Fatalf("build-host bundle must be untouched, got %q", got)
	}
}

func TestBakeRootfsDirItselfSymlinked(t *testing.T) {
	// The rootfs is reached through a symlinked spelling (artifact dir
	// behind a link). The walk must compare like with like: containment
	// checks are anchored on the resolved root, so every path still
	// resolves and the bake succeeds instead of rejecting everything.
	realRoot := t.TempDir()
	linkFarm := t.TempDir()
	link := filepath.Join(linkFarm, "rootfs")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, realRoot, "usr/local/share/ca-certificates")

	caPEM, _ := makeCA(t, "Linked Rootfs Test CA")
	res, err := Bake(link, caPEM)
	if err != nil {
		t.Fatalf("rootfsDir behind a symlink must not reject everything: %v", err)
	}
	if !res.Baked {
		t.Fatalf("expected Baked=true, got %+v", res)
	}
	anchor := filepath.Join(realRoot, "usr/local/share/ca-certificates", AnchorFileName)
	if _, err := os.Stat(anchor); err != nil {
		t.Fatalf("anchor must be written into the real rootfs: %v", err)
	}
}

func TestBakeSymlinkLoopRejected(t *testing.T) {
	// The bundle leaf resolves to a -> b -> a chain: the walk must
	// reject it like Linux ELOOP instead of spinning forever.
	root := t.TempDir()
	mustMkdir(t, root, "etc/ssl/certs")
	if err := os.Symlink("a", filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(root, "etc/ssl/certs/a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(root, "etc/ssl/certs/b")); err != nil {
		t.Fatal(err)
	}

	caPEM, _ := makeCA(t, "Loop Test CA")
	_, err := Bake(root, caPEM)
	if err == nil || !strings.Contains(err.Error(), "symlink chain too deep") {
		t.Fatalf("expected symlink-loop rejection, got %v", err)
	}
}

func TestBakeDanglingSymlinkTreatedAsMissing(t *testing.T) {
	// Android-style: etc -> <absolute path> that exists nowhere relevant.
	// Resolution rebases the target into the rootfs; the rebased target
	// is missing there, so the link keeps "missing" semantics for
	// readers. The bake must still succeed via the anchor dirs and
	// never touch the build host.
	root := t.TempDir()
	// Point at a path guaranteed not to exist: t.TempDir creates fresh
	// unique dirs, so a subpath of it that was never created is safe.
	dangling := filepath.Join(t.TempDir(), "nonexistent", "sub", "path")
	if err := os.Symlink(dangling, filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	// The anchor dirs exist as plain dirs inside the rootfs so something
	// gets written and the bake is not a distroless seed.
	mustMkdir(t, root, "usr/local/share/ca-certificates")

	caPEM, _ := makeCA(t, "Dangling Test CA")
	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("dangling symlink must be treated as missing, got: %v", err)
	}
	if !res.Baked {
		t.Fatalf("expected Baked=true via anchor dir, got %+v", res)
	}
}

func TestBakeSeedRedirectedIntoRootfsNotHost(t *testing.T) {
	// etc -> <absolute build-host dir that exists but has no ssl/certs
	// under it>. The walk rebases the target into the rootfs, so the
	// seed's MkdirAll builds the guest-visible directory tree *inside
	// the rootfs* — the old raw-path fallback used to create the same
	// directories on the build host through the symlink.
	root := t.TempDir()
	outside := t.TempDir() // exists on the host, empty
	if err := os.Symlink(outside, filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	// No other trust store: the seed path must be reachable.
	caPEM, _ := makeCA(t, "Seed Escape Test CA")
	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("seed must be redirected into the rootfs, not fail: %v", err)
	}
	if !res.Baked || !res.Seeded {
		t.Fatalf("expected Baked=true Seeded=true, got %+v", res)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("directories were created on the build host: %v", entries)
	}
	seeded := filepath.Join(root, outside, "ssl/certs/ca-certificates.crt")
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("seed must land inside the rootfs at %q: %v", seeded, err)
	}
}

func TestBakeSeedThroughSymlinkedLeafLandsAtGuestTarget(t *testing.T) {
	// The seed leaf itself is a symlink to an absolute build-host file.
	// Writing through the link must land at the guest-visible (rebased)
	// location inside the rootfs — the host file itself is never
	// opened, read, or modified.
	root := t.TempDir()
	outside := t.TempDir()
	hostFile := filepath.Join(outside, "host-bundle")
	if err := os.WriteFile(hostFile, []byte("existing host content"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, root, "etc/ssl/certs")
	if err := os.Symlink(hostFile, filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")); err != nil {
		t.Fatal(err)
	}

	caPEM, _ := makeCA(t, "Leaf Escape Test CA")
	res, err := Bake(root, caPEM)
	if err != nil {
		t.Fatalf("leaf symlink must resolve to the guest target, not fail: %v", err)
	}
	if !res.Baked || !res.Seeded {
		t.Fatalf("expected Baked=true Seeded=true, got %+v", res)
	}
	if got := mustReadFile(t, hostFile); got != "existing host content" {
		t.Fatalf("host file was modified through the leaf symlink: %q", got)
	}
	seeded := filepath.Join(root, hostFile)
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("seed must land at the rebased guest target %q: %v", seeded, err)
	}
}

func TestBakeRefusesWriteThroughSymlinkedTmp(t *testing.T) {
	// appendBundle writes the merged bundle to a tmp path before renaming.
	// An image could ship that exact tmp path as a symlink to a host file;
	// writeFileNotThroughSymlink must refuse.
	root := t.TempDir()
	outside := t.TempDir()
	hostFile := filepath.Join(outside, "host-tmp-target")
	if err := os.WriteFile(hostFile, []byte("host target"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, root, "etc/ssl/certs")
	bundle := filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")
	if err := os.WriteFile(bundle, []byte("-----BEGIN CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostFile, bundle+".cube-egress-tmp"); err != nil {
		t.Fatal(err)
	}

	caPEM, _ := makeCA(t, "Tmp Escape Test CA")
	_, err := Bake(root, caPEM)
	if err == nil || !strings.Contains(err.Error(), "refusing to write through symlink") {
		t.Fatalf("expected tmp-symlink refusal, got %v", err)
	}
	if got := mustReadFile(t, hostFile); got != "host target" {
		t.Fatalf("host file was modified through the tmp symlink: %q", got)
	}
}
