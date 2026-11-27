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
	"path"
	"path/filepath"
	"strings"
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
	"usr/local/share/ca-certificates",           // Debian/Ubuntu
	"etc/pki/ca-trust/source/anchors",           // RHEL/Fedora/CentOS
	"etc/ca-certificates/trust-source",          // Arch
	"system/etc/security/cacerts",               // Android system trust store (pre-APEX location)
	"system/apex/com.android.conscrypt/cacerts", // Android 14+ conscrypt APEX system store
}

// androidCacertsDirs are the Android system trust store entries from
// anchorDirs. Unlike the Linux anchor dirs (where update-ca-* parses any
// PEM file), Android's TLS stack loads system CAs by their OpenSSL
// X509_NAME_hash_old file name (<hash>.<n>, as produced by
// `openssl x509 -subject_hash_old`; see opensslSubjectHashOld), so the
// anchor copies in these directories must be written under that name to
// be honoured at runtime.
//
// Android 14 moved the system store into the conscrypt APEX: the
// runtime reads the copy under system/apex/com.android.conscrypt, while
// system/etc/security/cacerts remains for older releases and legacy
// tooling. Stock A14 images ship both directories with identical
// content, so the bake writes both and whichever the runtime reads has
// the root.
var androidCacertsDirs = map[string]bool{
	"system/etc/security/cacerts":               true,
	"system/apex/com.android.conscrypt/cacerts": true,
}

// isAndroidCacertsDir reports whether rel is one of the Android system
// trust store directories that load anchors by subject-hash name.
func isAndroidCacertsDir(rel string) bool {
	return androidCacertsDirs[rel]
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
	// parseCA already validated the block parses as x509; keep the parsed
	// form for the Android anchor's subject-hash file name.
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return Result{}, fmt.Errorf("cube_egress_ca: re-parse caPEM: %w", err)
	}

	res := Result{Fingerprint: fingerprint}
	// caPresent tracks whether the CA already lives in some bundle/anchor
	// (idempotent no-op). It gates the distroless seed so a re-bake of an
	// already-seeded image doesn't fabricate a second copy.
	caPresent := false

	for _, rel := range bundleFiles {
		full, err := resolveUnderRootfs(rootfsDir, rel)
		if err != nil {
			return res, fmt.Errorf("cube_egress_ca: append %s: %w", rel, err)
		}
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
		anchorName := AnchorFileName
		if isAndroidCacertsDir(rel) {
			// Android loads system CAs by their X509_NAME_hash_old
			// subject-hash file name; any other file name in these
			// directories is silently ignored.
			anchorName = fmt.Sprintf("%08x.0", opensslSubjectHashOld(caCert))
		}
		full, err := resolveUnderRootfs(rootfsDir, rel)
		if err != nil {
			return res, fmt.Errorf("cube_egress_ca: drop anchor in %s: %w", rel, err)
		}
		written, reason, err := dropAnchor(full, anchorName, canonical)
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
		full, err := resolveUnderRootfs(rootfsDir, seedBundlePath)
		if err != nil {
			return res, fmt.Errorf("cube_egress_ca: seed %s: %w", seedBundlePath, err)
		}
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

// symlinkHopLimit matches Linux's SYMLOOP_MAX: a symlink chain longer
// than 40 hops cannot be resolved and is rejected, mirroring ELOOP.
const symlinkHopLimit = 40

// errDanglingSymlink reports a symlink whose target does not exist
// inside the rootfs. The target may well exist on the build host — that
// must not matter: the host is never consulted during resolution.
var errDanglingSymlink = errors.New("cube_egress_ca: symlink dangling inside the rootfs")

// containedInRoot reports whether fs is cleanRoot itself or a path
// under it. cleanRoot == "/" (a rootfs at the filesystem root, which
// never happens for template artifacts but costs nothing to allow)
// trivially contains every absolute path.
func containedInRoot(cleanRoot, fs string) bool {
	if cleanRoot == string(os.PathSeparator) {
		return true
	}
	return fs == cleanRoot || strings.HasPrefix(fs, cleanRoot+string(os.PathSeparator))
}

// resolveUnderRootfs joins rel onto rootfsDir and resolves the path,
// verifying every existing component — including the final one — stays
// inside the rootfs.
//
// Resolution semantics:
//
//   - The verified root is the symlink-resolved rootfsDir (cleanRoot),
//     and every returned path is built from it, so a rootfsDir spelled
//     through symlinks compares like with like (its raw spelling never
//     reaches the containment checks).
//   - Symlinks are resolved entirely inside the rootfs, component by
//     component: absolute targets are clamped at the guest root
//     (leading ".." collapses at "/" POSIX style) and then rebased onto
//     cleanRoot; relative targets join the symlink's own directory in
//     guest terms; targets that themselves pass through further
//     symlinks are re-walked the same way. Every filesystem lookup is a
//     single-component Lstat whose earlier components are already
//     verified symlink-free, so the kernel never resolves an
//     intermediate symlink for us and the walk cannot be steered to
//     build-host files. Android's /etc -> /system/etc and RHEL's
//     /etc/pki/tls/certs/ca-bundle.crt -> /etc/pki/ca-trust/... land
//     where they exist in the guest tree; host paths an image may
//     reuse are simply absent from the rootfs.
//   - A missing component ends the walk: the remainder is joined onto
//     the guest-visible location the walk stopped at. Callers create
//     the remainder fresh (MkdirAll/WriteFile), and anchoring it on
//     the walked location keeps MkdirAll from traversing an earlier
//     symlink into a location that only partially exists elsewhere.
//   - A symlink whose target is missing inside the rootfs (dangling,
//     e.g. a link to a host-only path) keeps "missing" semantics, with
//     the remainder joined onto the rebased target: a later MkdirAll
//     creates the guest-visible location inside the rootfs, mirroring
//     what the guest itself would see through the link, and never
//     touching the build host.
//   - The final component is resolved too: callers Stat/Read/Write the
//     returned path, and an unresolved leaf symlink would let those
//     reach files outside the verified chain.
func resolveUnderRootfs(rootfsDir, rel string) (string, error) {
	cleanRoot, err := filepath.EvalSymlinks(rootfsDir)
	if err != nil {
		// The rootfs itself must exist (Bake stats it earlier); treat any
		// failure as "cannot resolve" and fall through to the raw path.
		return filepath.Join(rootfsDir, rel), nil
	}
	relParts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	for _, part := range relParts {
		if part == ".." {
			return "", fmt.Errorf("path %q escapes rootfs %q lexically", rel, cleanRoot)
		}
	}
	resolvedFS := cleanRoot
	resolvedGuest := "/"
	hops := 0
	for i, part := range relParts {
		if part == "" || part == "." {
			continue
		}
		fs, guest, err := resolveGuestPath(cleanRoot, path.Join(resolvedGuest, part), &hops)
		if errors.Is(err, errDanglingSymlink) {
			// Missing at this component: the remainder is created fresh
			// by the caller, anchored on the guest-visible location the
			// walk stopped at.
			return filepath.Join(append([]string{fs}, relParts[i+1:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		resolvedFS, resolvedGuest = fs, guest
	}
	return resolvedFS, nil
}

// resolveGuestPath resolves guest — an absolute guest-space path — under
// cleanRoot, walking one component at a time. Each component is Lstat'ed
// on a path whose earlier components are already verified symlink-free,
// so the kernel never resolves an intermediate symlink for us; when a
// component is a symlink its target is clamped at the guest root
// (path.Clean collapses leading ".." at "/"), rebased onto cleanRoot,
// and re-walked the same way. Symlink resolutions are counted through
// hops and capped at symlinkHopLimit (Linux SYMLOOP_MAX semantics).
//
// Returns (fsPath, guestPath, nil) when every component exists and is
// symlink-free — fsPath is safe for direct use by the caller. Returns
// (fsPath, guestPath, errDanglingSymlink) when the guest path is
// missing inside the rootfs; fsPath is the full guest-visible location
// (missing middle components included) where the caller can create the
// remainder. Any fsPath handed back is asserted to stay under
// cleanRoot: a future walk bug fails loudly instead of escaping.
func resolveGuestPath(cleanRoot, guest string, hops *int) (string, string, error) {
walk:
	for {
		guest = path.Clean(guest)
		if !path.IsAbs(guest) {
			return "", "", errors.New("cube_egress_ca: guest path must be absolute")
		}
		parts := strings.Split(guest, "/")
		fs := cleanRoot
		for i := 1; i < len(parts); i++ {
			fs = filepath.Join(fs, parts[i])
			info, err := os.Lstat(fs)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// Hand back the full guest-visible location; the
					// caller creates the remainder (MkdirAll builds any
					// missing middle components inside the rootfs).
					fs = filepath.Join(cleanRoot, guest)
					if !containedInRoot(cleanRoot, fs) {
						return "", "", fmt.Errorf("symlink resolution escaped the rootfs: %q", guest)
					}
					return fs, guest, errDanglingSymlink
				}
				return "", "", err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(fs)
				if err != nil {
					return "", "", err
				}
				*hops++
				if *hops > symlinkHopLimit {
					return "", "", fmt.Errorf("symlink chain too deep at %q (limit %d hops)", guest, symlinkHopLimit)
				}
				t := filepath.ToSlash(target)
				if path.IsAbs(t) {
					// Clamp at the guest root first: path.Clean collapses
					// leading ".." at "/", so the target can never climb
					// out of the guest namespace before it is rebased
					// onto cleanRoot.
					guest = path.Clean(t)
				} else {
					// Resolve against the symlink's own directory, in
					// guest terms.
					guest = path.Join("/"+strings.Join(parts[1:i], "/"), t)
				}
				// The components that follow the symlink in the current
				// path continue past the target. The substituted path may
				// traverse further symlinks, so the walk restarts on it.
				guest = path.Join(guest, strings.Join(parts[i+1:], "/"))
				continue walk
			}
		}
		if !containedInRoot(cleanRoot, fs) {
			return "", "", fmt.Errorf("symlink resolution escaped the rootfs: %q", guest)
		}
		return fs, guest, nil
	}
}

// writeFileNotThroughSymlink writes data to path, refusing when path itself
// is a symlink: an image could ship <target>.cube-egress-tmp as a symlink
// to a build-host file, and os.WriteFile would follow it out of the rootfs.
// The Lstat/Write pair is not atomic, but the extracted rootfs is a static
// artifact during the bake, so no concurrent mutation races it.
func writeFileNotThroughSymlink(path string, data []byte, perm os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cube_egress_ca: refusing to write through symlink %q", path)
	}
	return os.WriteFile(path, data, perm)
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
	if err := writeFileNotThroughSymlink(tmp, merged, 0o644); err != nil {
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
func dropAnchor(dir, fileName string, canonical []byte) (bool, string, error) {
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
	full := filepath.Join(dir, fileName)
	existing, err := os.ReadFile(full) // #nosec G304 — fixed basename
	if err == nil && bytes.Equal(existing, canonical) {
		return false, "present", nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "", err
	}
	tmp := full + ".cube-egress-tmp"
	if err := writeFileNotThroughSymlink(tmp, canonical, 0o644); err != nil {
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
		// Distros whose /etc is a symlink (e.g. Android: /etc -> /system/etc)
		// make MkdirAll fail with EEXIST — reachable through the raw-path
		// fallback when the rootfs itself cannot be resolved. Anchor dirs
		// cover those distros; skip seeding instead of failing the bake.
		if errors.Is(err, os.ErrExist) {
			return false, "seed skipped: parent path occupied (symlink?)", nil
		}
		return false, "", err
	}
	tmp := full + ".cube-egress-tmp"
	if err := writeFileNotThroughSymlink(tmp, canonical, 0o644); err != nil {
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
