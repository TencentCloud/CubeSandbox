// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cube_egress_ca bakes the CubeEgress root CA into a sandbox
// rootfs directory at template-build time. See
// design/cube-egress-ca-bake.md for the rationale and contract.
//
// What "bake" means here: append the CA to whichever ca-bundle files
// already exist in the rootfs (Debian/Ubuntu, Alpine, RHEL/Fedora,
// etc.) and drop a copy of the CA into the canonical anchor directories
// for whichever distro families have one. We deliberately do NOT exec
// `update-ca-certificates` / `update-ca-trust` inside the rootfs: that
// would require the rootfs's binaries to be runnable on the host
// (matching arch, or qemu-static + binfmt_misc), which is fragile and
// adds runtime deps. Appending to the bundle file is what those tools
// already produce; we do the same writes ourselves and call it done.
//
// Idempotence: bake matches existing bundle entries by decoded DER
// bytes, not raw text. A re-bake of the same rootfs with the same CA
// is a no-op even after intervening whitespace / newline shifts. This
// matters because buildRootfsArtifact's redo path may re-enter the
// bake on the same directory.
//
// Append vs. replace: ALWAYS append. The image bundle already contains
// Mozilla's public CA list; replacing it would break trust for every
// public HTTPS endpoint a workload talks to. PEM bundles are designed
// to be concatenated.
//
// Distroless / scratch seeding: an image that ships NEITHER a ca-bundle
// file NOR any anchor directory (e.g. gcr.io/distroless/static, FROM
// scratch) has no trust store to append to. Rather than fail the build,
// we *seed* the canonical bundle path (seedBundlePath) from scratch with
// just the CubeEgress root. This is safe and sufficient precisely
// because CubeEgress is the egress MITM: it re-signs every outbound TLS
// connection with this root, so the workload only needs to trust this
// one CA — it does not need the public Mozilla set (egress to the wider
// internet is brokered by CubeEgress, not made directly). Seeding only
// kicks in when nothing else exists and the CA is not already present,
// so it never clobbers an image's own roots.
package cube_egress_ca

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Result describes the outcome of a bake. Callers persist these onto
// the RootfsArtifact row for audit / debug.
type Result struct {
	// Baked is true iff at least one bundle or anchor write (or seed)
	// succeeded this pass. A baked=false result with no error means
	// every target was already up-to-date (idempotent no-op); the caller
	// decides whether that's acceptable based on context.
	Baked bool

	// TargetsWritten counts the bundle and anchor locations that were
	// actually written to this pass (new append, new anchor, or fresh
	// seed). Idempotent skips ("already there") are NOT counted because
	// no write occurred. The exact number is informational; downstream
	// alarms should care about Baked + the err path, not this number.
	TargetsWritten int

	// Fingerprint is hex(sha256(caPEMBlock.Bytes)) — derived from the
	// DER, not the textual PEM, so cosmetic differences (line endings,
	// trailing whitespace) don't change the fingerprint.
	//
	// Used by CubeMaster's reuse-cache logic so that rotating the host
	// CA invalidates artifacts baked with the old one (see
	// buildTemplateSpecFingerprint).
	Fingerprint string

	// SkippedReasons records human-readable reasons each candidate
	// target was skipped (file missing, dir missing, idempotent
	// no-op). Surfaced via the cubemastercli template info command for
	// triage; not load-bearing for any decision.
	SkippedReasons []string

	// Seeded is true iff the bake created a fresh ca-bundle from scratch
	// at seedBundlePath because the image carried no trust store of its
	// own (distroless / scratch). When Seeded is true, Baked is also
	// true. Surfaced in the bake log line for diagnostics; not persisted
	// separately to the DB because Baked + TargetsWritten + image
	// metadata are sufficient to infer how the trust root landed.
	Seeded bool
}

// AnchorFileName is the basename used for the dropped anchor copy,
// regardless of distro. Matches the file name used elsewhere in the
// project for the CubeEgress root.
const AnchorFileName = "cube-egress-root.crt"

// bundleFiles is the closed list of ca-bundle files we append to. Order
// is informational; each is tried independently. Paths are relative to
// rootfsDir.
var bundleFiles = []string{
	// Debian/Ubuntu, also Alpine when ca-certificates is installed.
	"etc/ssl/certs/ca-certificates.crt",

	// RHEL/Fedora/CentOS modern ca-trust extracted bundle.
	"etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",

	// RHEL legacy / Amazon Linux 2.
	"etc/pki/tls/certs/ca-bundle.crt",
}

// anchorDirs is the closed list of "drop-in" directories where the
// distro's update-ca-* tools look for new roots to add. Dropping a
// copy here lets a future runtime invocation of update-ca-* (e.g. by
// a workload that runs apt install some-package and triggers a
// post-install hook) pick the CA up too.
var anchorDirs = []string{
	"usr/local/share/ca-certificates",  // Debian/Ubuntu
	"etc/pki/ca-trust/source/anchors",  // RHEL/Fedora/CentOS
	"etc/ca-certificates/trust-source", // Arch
}

// seedBundlePath is the bundle file we *create* from scratch when an
// image has no trust store of its own (distroless / scratch). It is the
// Debian/Ubuntu canonical path, which is also Go's first-choice probe
// location (crypto/x509) and the value distroless images point
// SSL_CERT_FILE at — so seeding here is picked up by the widest set of
// runtimes. It is intentionally identical to bundleFiles[0]: the append
// pass runs first and, on a distroless image, records it "missing"; the
// seed pass then creates it.
const seedBundlePath = "etc/ssl/certs/ca-certificates.crt"

// Bake runs against rootfsDir, applying caPEM to every bundle/anchor
// location it finds. Returns the structured Result plus an error iff
// the bake encountered a *hard* failure: i.e. either some target had a
// chance to be written and the write failed (so the result is
// partially mutated and we don't want to silently leave it that way),
// or the input PEM is itself invalid. Targets that simply don't exist
// in this image are NOT errors — they're recorded in
// Result.SkippedReasons.
//
// Distroless / scratch images that ship no bundle and no anchor dir are
// not skipped: Bake seeds seedBundlePath with the CA (Result.Seeded set)
// so the trust root still lands. See the package doc for why this is
// safe under the CubeEgress MITM model.
//
// Concurrency: not safe for parallel callers writing to the same
// rootfsDir. The artifact-build pipeline serializes per-rootfs builds
// upstream, so this is fine.
func Bake(rootfsDir string, caPEM []byte) (Result, error) {
	if rootfsDir == "" {
		return Result{}, errors.New("cube_egress_ca: rootfsDir is empty")
	}
	if info, err := os.Stat(rootfsDir); err != nil {
		return Result{}, fmt.Errorf("cube_egress_ca: stat rootfsDir %q: %w", rootfsDir, err)
	} else if !info.IsDir() {
		return Result{}, fmt.Errorf("cube_egress_ca: rootfsDir %q is not a directory", rootfsDir)
	}

	caBlock, fingerprint, err := parseCA(caPEM)
	if err != nil {
		return Result{}, err
	}
	canonical := pem.EncodeToMemory(caBlock)

	res := Result{Fingerprint: fingerprint}
	// caPresent tracks whether the CA already lives in some bundle/anchor
	// (idempotent no-op). It gates the distroless seed so a re-bake of an
	// already-seeded image doesn't fabricate a second copy.
	caPresent := false

	for _, rel := range bundleFiles {
		full := filepath.Join(rootfsDir, rel)
		written, reason, err := appendBundle(full, canonical, caBlock.Bytes)
		if err != nil {
			return res, fmt.Errorf("cube_egress_ca: append %s: %w", rel, err)
		}
		if written {
			res.TargetsWritten++
			res.Baked = true
		}
		if reason == "present" {
			caPresent = true
		}
		if reason != "" {
			res.SkippedReasons = append(res.SkippedReasons, rel+": "+reason)
		}
	}

	for _, rel := range anchorDirs {
		full := filepath.Join(rootfsDir, rel)
		written, reason, err := dropAnchor(full, canonical)
		if err != nil {
			return res, fmt.Errorf("cube_egress_ca: drop anchor in %s: %w", rel, err)
		}
		if written {
			res.TargetsWritten++
			res.Baked = true
		}
		if reason == "present" {
			caPresent = true
		}
		if reason != "" {
			res.SkippedReasons = append(res.SkippedReasons, rel+": "+reason)
		}
	}

	// Distroless / scratch fallback: the image had no trust store to
	// append to and the CA isn't already present anywhere, so seed the
	// canonical bundle from scratch. Safe under the CubeEgress MITM model
	// (see package doc) and never clobbers an image's own roots because
	// it only runs when nothing else matched.
	if !res.Baked && !caPresent {
		full := filepath.Join(rootfsDir, seedBundlePath)
		written, reason, err := seedBundle(full, canonical)
		if err != nil {
			return res, fmt.Errorf("cube_egress_ca: seed %s: %w", seedBundlePath, err)
		}
		if written {
			res.TargetsWritten++
			res.Baked = true
			res.Seeded = true
			res.SkippedReasons = append(res.SkippedReasons,
				seedBundlePath+": seeded (image shipped no trust store)")
		} else if reason != "" {
			res.SkippedReasons = append(res.SkippedReasons, seedBundlePath+": "+reason)
		}
	}

	return res, nil
}

// parseCA validates that caPEM contains exactly one CERTIFICATE PEM
// block parseable as a real x509 cert, and returns it normalised. We
// reject empty / multi-cert / non-cert PEM up front because the
// invariant "the bake plants one root" is what the rest of the system
// assumes.
//
// fingerprint is hex(sha256(DER)) — using DER not the raw PEM means
// whitespace differences don't perturb it. This is the value plumbed
// into buildTemplateSpecFingerprint so a real CA rotation (different
// DER) invalidates the artifact cache while a cosmetic re-encoding of
// the same cert does not.
func parseCA(caPEM []byte) (*pem.Block, string, error) {
	if len(bytes.TrimSpace(caPEM)) == 0 {
		return nil, "", errors.New("cube_egress_ca: caPEM is empty")
	}
	block, rest := pem.Decode(caPEM)
	if block == nil {
		return nil, "", errors.New("cube_egress_ca: caPEM is not PEM-encoded")
	}
	if block.Type != "CERTIFICATE" {
		return nil, "", fmt.Errorf("cube_egress_ca: caPEM block type is %q, want CERTIFICATE", block.Type)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, "", fmt.Errorf("cube_egress_ca: parse caPEM as x509: %w", err)
	}
	if extra := bytes.TrimSpace(rest); len(extra) > 0 {
		// Reject multi-cert bundles. If we ever need to bake an
		// intermediate chain, that's a separate decision and we'd
		// want it called out explicitly.
		if next, _ := pem.Decode(rest); next != nil {
			return nil, "", errors.New("cube_egress_ca: caPEM contains more than one PEM block; expected single CERTIFICATE")
		}
	}
	sum := sha256.Sum256(block.Bytes)
	return block, hex.EncodeToString(sum[:]), nil
}

// appendBundle appends `canonical` (PEM bytes) to the file at `full` if
// the file exists AND doesn't already contain a PEM block whose DER
// matches `derNeedle`. Returns:
//
//	written=true, reason=""           file was modified
//	written=false, reason="missing"   file does not exist; not our problem
//	written=false, reason="present"   CA already in this bundle (idempotent)
//	written=false, reason!=""         a recoverable skip with explanation
//	written=false, err!=nil           write attempt failed mid-flight
func appendBundle(full string, canonical, derNeedle []byte) (bool, string, error) {
	existing, err := os.ReadFile(full) // #nosec G304 — paths come from a closed list
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "missing", nil
		}
		return false, "", err
	}
	if bundleContainsDER(existing, derNeedle) {
		return false, "present", nil
	}
	// Write atomically: temp file + rename. Important because
	// buildRootfsArtifact may abort partway and leave us holding the
	// rootfs directory; a half-appended bundle would silently corrupt
	// downstream TLS in subtle ways.
	tmp := full + ".cube-egress-tmp"
	separator := []byte{}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		separator = []byte{'\n'}
	}
	merged := make([]byte, 0, len(existing)+len(separator)+len(canonical))
	merged = append(merged, existing...)
	merged = append(merged, separator...)
	merged = append(merged, canonical...)
	if err := os.WriteFile(tmp, merged, 0o644); err != nil {
		return false, "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return false, "", err
	}
	return true, "", nil
}

// dropAnchor copies the canonical PEM into <dir>/<AnchorFileName> if
// the directory exists. Skips quietly if the dir is missing — most
// images carry only one of the candidate anchor dirs.
//
// Idempotent: if the file already exists with identical content, we
// don't bump its mtime (the result is the same as a re-write but it's
// nicer for diff/debug to leave it alone).
func dropAnchor(dir string, canonical []byte) (bool, string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "dir missing", nil
		}
		return false, "", err
	}
	if !info.IsDir() {
		return false, "not a directory", nil
	}
	full := filepath.Join(dir, AnchorFileName)
	existing, err := os.ReadFile(full) // #nosec G304 — fixed basename
	if err == nil && bytes.Equal(existing, canonical) {
		return false, "present", nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "", err
	}
	tmp := full + ".cube-egress-tmp"
	if err := os.WriteFile(tmp, canonical, 0o644); err != nil {
		return false, "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return false, "", err
	}
	return true, "", nil
}

// seedBundle creates a fresh ca-bundle at `full` containing only the
// canonical CA, for distroless / scratch images that ship no trust
// store of their own. It mkdir -p's the parent directory first and
// writes atomically (temp + rename) for the same crash-safety reason as
// appendBundle.
//
// Callers only reach this when no existing bundle/anchor matched, so the
// file normally does not exist. We still guard the rare "already there"
// case (identical content) to keep redo paths a no-op.
//
//	written=true,  reason=""        bundle was created
//	written=false, reason="present" identical bundle already exists
//	written=false, err!=nil         mkdir / write attempt failed
func seedBundle(full string, canonical []byte) (bool, string, error) {
	if existing, err := os.ReadFile(full); err == nil { // #nosec G304 — fixed path from a closed list
		if bytes.Equal(existing, canonical) {
			return false, "present", nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, "", err
	}
	tmp := full + ".cube-egress-tmp"
	if err := os.WriteFile(tmp, canonical, 0o644); err != nil {
		return false, "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return false, "", err
	}
	return true, "", nil
}

// bundleContainsDER walks all PEM CERTIFICATE blocks in `bundle` and
// returns true iff any block's DER matches `needle`. We compare DER
// rather than text to be tolerant of whitespace, line-ending, and
// re-encoding differences.
func bundleContainsDER(bundle, needle []byte) bool {
	for {
		block, rest := pem.Decode(bundle)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" && bytes.Equal(block.Bytes, needle) {
			return true
		}
		bundle = rest
	}
}

// FingerprintOf is a small helper for callers that need the
// fingerprint without running a full bake (e.g. computing the
// template spec fingerprint at request validation time before any
// rootfs is on disk).
func FingerprintOf(caPEM []byte) (string, error) {
	_, fp, err := parseCA(caPEM)
	return fp, err
}
