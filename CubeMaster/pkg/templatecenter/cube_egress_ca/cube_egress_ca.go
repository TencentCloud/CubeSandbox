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
	// Baked is true iff at least one bundle or anchor write succeeded.
	// A baked=false result with no error is not a failure on its own
	// (e.g. distroless image with no bundles to update); the caller
	// decides whether that's acceptable based on the "required" flag.
	Baked bool

	// TargetsWritten counts the bundle and anchor locations that
	// received the CA, including no-op idempotent skips (counted as
	// "already there"). The exact number is informational; downstream
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

// Bake runs against rootfsDir, applying caPEM to every bundle/anchor
// location it finds. Returns the structured Result plus an error iff
// the bake encountered a *hard* failure: i.e. either some target had a
// chance to be written and the write failed (so the result is
// partially mutated and we don't want to silently leave it that way),
// or the input PEM is itself invalid. Targets that simply don't exist
// in this image are NOT errors — they're recorded in
// Result.SkippedReasons.
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
		if reason != "" {
			res.SkippedReasons = append(res.SkippedReasons, rel+": "+reason)
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
