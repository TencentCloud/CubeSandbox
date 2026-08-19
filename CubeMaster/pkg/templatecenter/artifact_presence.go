// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
)

// artifactPresence describes whether a rootfs artifact's ext4 file is still
// backing its READY database row.
//
// WHY THIS EXISTS
// ---------------
// A rootfs artifact is two things that can drift apart: a row in
// rootfs_artifacts (status=READY, ext4_sha256, download_token) and a file on
// the node's local disk. Nothing keeps them in sync. The file disappears
// whenever the disk backing the artifact store is not durable across restarts
// (emptyDir instead of a PVC, a wiped host dir, an operator cleaning up by
// hand), while the row happily stays READY forever.
//
// Every consumer used to trust the row alone:
//
//   - the reuse path returned the READY row and skipped the build entirely,
//   - distribution passed its readiness guard on DB fields only and pushed a
//     CreateImage to cubelets,
//   - the download endpoint failed the pull with "artifact source missing"
//     without ever touching the row.
//
// The result was a permanent dead end: every retry took the same reuse path,
// re-distributed the same phantom artifact, and failed the same way. Probing
// the file lets the reuse and download paths demote the row so the very next
// create rebuilds it.
type artifactPresence int

const (
	// artifactPresenceUnknown means the answer must not be acted on: either a
	// build currently owns the directory, or stat failed for a reason that is
	// not "the file is gone" (permissions, transient I/O). Callers must neither
	// reuse the artifact nor demote its row.
	artifactPresenceUnknown artifactPresence = iota
	// artifactPresencePresent means a non-empty ext4 file backs the row.
	artifactPresencePresent
	// artifactPresenceMissing means the row is not backed by a usable file and
	// the artifact has to be rebuilt.
	artifactPresenceMissing
)

func (p artifactPresence) String() string {
	switch p {
	case artifactPresencePresent:
		return "present"
	case artifactPresenceMissing:
		return "missing"
	default:
		return "unknown"
	}
}

// classifyArtifactPresence is the whole decision, kept free of I/O so every
// branch is directly testable.
//
// The ordering matters. buildInProgress is checked first because a live build
// marker means another process is writing this very directory: the ext4 may be
// absent or half-written at this instant, and treating that as Missing would
// demote a row whose build is about to succeed.
//
// A zero-length file is Missing rather than Present: mkfs.ext4 never produces
// one, so it is a truncated leftover that would fail the pull with
// "invalid size:0" further downstream.
//
// Any stat error other than ErrNotExist stays Unknown on purpose. A permission
// or I/O error says nothing about whether the artifact is still there, and
// rebuilding gigabytes of ext4 because of a transient EIO is worse than
// surfacing the error to the caller that already has to handle it.
func classifyArtifactPresence(ext4Path string, buildInProgress bool, size int64, statErr error) artifactPresence {
	if buildInProgress {
		return artifactPresenceUnknown
	}
	// An empty path on a READY row is corrupt metadata, not a transient
	// condition: there is no file to find, so the artifact must be rebuilt.
	if ext4Path == "" {
		return artifactPresenceMissing
	}
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return artifactPresenceMissing
		}
		return artifactPresenceUnknown
	}
	if size <= 0 {
		return artifactPresenceMissing
	}
	return artifactPresencePresent
}

// statArtifactFile is the single I/O seam, swapped out in tests.
var statArtifactFile = func(path string) (int64, error) {
	st, err := os.Stat(path) // NOCC:Path Traversal()
	if err != nil {
		return 0, err
	}
	if st.IsDir() {
		// A directory where the ext4 is expected is corrupt layout; report it as
		// absent so the artifact is rebuilt rather than served as a file.
		return 0, os.ErrNotExist
	}
	return st.Size(), nil
}

// probeArtifactPresence resolves the on-disk state of an artifact's ext4 file.
func probeArtifactPresence(ext4Path string) artifactPresence {
	if ext4Path == "" {
		return classifyArtifactPresence("", false, 0, nil)
	}
	buildInProgress, _ := image.ArtifactBuildInProgress(filepath.Dir(ext4Path))
	size, err := statArtifactFile(ext4Path)
	return classifyArtifactPresence(ext4Path, buildInProgress, size, err)
}

// artifactMissingFileError renders the diagnostic used when a phantom artifact
// is refused. It names both halves of the drift (row and path) because the
// operator has to look at the disk to understand it.
func artifactMissingFileError(artifactID, ext4Path string) error {
	return fmt.Errorf(
		"rootfs artifact %s is READY in the database but its ext4 file %q is missing on disk; "+
			"the artifact store is not durable across restarts (expected a persistent volume) — "+
			"the row has been demoted so the next create rebuilds it",
		artifactID, ext4Path)
}

// artifactNotServedHereError is the counterpart used when this node is simply
// not the one holding the artifact. Nothing is demoted in that case, because
// the file is presumably intact on whichever node built it.
//
// This is the multi-master shape of the problem (issue #1005): the artifact
// store is node-local, so with two CubeMasters behind one address only the
// builder has the ext4. The request landed on the wrong node.
func artifactNotServedHereError(artifactID, ext4Path, downloadBaseURL string) error {
	return fmt.Errorf(
		"rootfs artifact %s has no ext4 file at %q on this node, and its download base URL %q does not point here; "+
			"the artifact store is node-local, so a multi-CubeMaster deployment must either share it "+
			"(one volume) or keep artifact downloads pinned to the building node — "+
			"the database row was left untouched because another node likely still holds the file",
		artifactID, ext4Path, downloadBaseURL)
}

// localArtifactHostsFn resolves the set of host identities that mean "this
// node", used to decide whether this process is the authoritative holder of an
// artifact. Indirected for tests.
var localArtifactHostsFn = localArtifactHosts

func localArtifactHosts() map[string]bool {
	hosts := map[string]bool{
		"localhost": true,
		"127.0.0.1": true,
		"::1":       true,
		"0.0.0.0":   true,
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		hosts[strings.ToLower(name)] = true
	}
	// Every address configured on this host counts: the download base URL is
	// derived from whatever Host header the caller used, so it may name any of
	// them.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return hosts
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		hosts[strings.ToLower(ipNet.IP.String())] = true
	}
	return hosts
}

// artifactServedLocally reports whether downloadBaseURL designates this node.
//
// An empty base URL means "this node" because buildDownloadURL falls back to the
// local hostname when the column is empty, so the artifact would be served from
// here anyway.
//
// A host that cannot be matched (a load balancer VIP, a service name, a DNS
// alias) yields false: it is explicitly NOT a claim that the file is gone, only
// that this node cannot prove it owns it. Callers must not demote on false.
func artifactServedLocally(downloadBaseURL string, localHosts map[string]bool) bool {
	raw := strings.TrimSpace(downloadBaseURL)
	if raw == "" {
		return true
	}
	if !strings.Contains(raw, "://") {
		// The column historically held a bare IP or host[:port].
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	return localHosts[strings.ToLower(host)]
}

// artifactAuthoritativeHere reports whether this node may rewrite the shared
// database row for an artifact whose file it cannot find.
func artifactAuthoritativeHere(record *models.RootfsArtifact) bool {
	if record == nil {
		return false
	}
	return artifactServedLocally(record.MasterNodeIP, localArtifactHostsFn())
}

// demoteMissingRootfsArtifact flips a READY row whose file vanished to FAILED so
// the reuse path stops handing it out and the next create rebuilds it.
//
// FAILED (not CLEANUP_PENDING/ORPHANED) is deliberate: those two states drive
// GC, which would try to reclaim files that are already gone and would delete
// the row that ensureRootfsArtifact wants to claim and rebuild in place. FAILED
// is exactly "this row exists but holds nothing usable", which is the state the
// build path already knows how to recover from.
//
// Indirected through a var so tests can observe the demotion without a live DB.
var demoteMissingRootfsArtifact = func(ctx context.Context, artifactID, ext4Path string) {
	log.G(ctx).Errorf("rootfs artifact %s: ext4 file %q is gone while the row is READY; demoting to %s so it is rebuilt",
		artifactID, ext4Path, ArtifactStatusFailed)
	if err := updateRootfsArtifact(ctx, artifactID, map[string]any{
		"status":     ArtifactStatusFailed,
		"last_error": fmt.Sprintf("ext4 file %s missing on disk; artifact must be rebuilt", ext4Path),
	}); err != nil {
		// Losing the demotion only costs another failed attempt that will retry
		// this same demotion, so it must not fail the caller.
		log.G(ctx).Warnf("rootfs artifact %s: demote to %s failed: %v", artifactID, ArtifactStatusFailed, err)
	}
}

// artifactMissingVerdict is the outcome of discovering that an artifact's ext4
// file is not on this node.
type artifactMissingVerdict int

const (
	// artifactMissingVerdictNone means the file is there (or its state is
	// unknown) and nothing needs to happen.
	artifactMissingVerdictNone artifactMissingVerdict = iota
	// artifactMissingVerdictDemoted means this node owns the artifact, the file
	// is really gone, and the row was demoted so the next create rebuilds it.
	artifactMissingVerdictDemoted
	// artifactMissingVerdictForeign means the file is absent here but this node
	// is not the artifact's holder, so the row was deliberately left alone.
	artifactMissingVerdictForeign
)

// resolveMissingArtifact centralises the "file is not here" decision so the
// reuse, distribution and download paths cannot drift apart on the one thing
// that must never be got wrong: whether it is safe to rewrite a shared row.
//
// Demoting is only correct when this node is the artifact's holder. In a
// multi-CubeMaster deployment the file legitimately lives on another node, and
// demoting from here would destroy a perfectly good artifact (and force a
// rebuild whose fresh sha256 then mismatches every already-distributed replica,
// which is issue #1005 made worse rather than better).
func resolveMissingArtifact(ctx context.Context, record *models.RootfsArtifact) artifactMissingVerdict {
	if record == nil {
		return artifactMissingVerdictNone
	}
	if probeArtifactPresence(record.Ext4Path) != artifactPresenceMissing {
		return artifactMissingVerdictNone
	}
	if !artifactAuthoritativeHere(record) {
		log.G(ctx).Errorf("rootfs artifact %s: ext4 file %q absent here and download base URL %q is not local; leaving the row untouched",
			record.ArtifactID, record.Ext4Path, record.MasterNodeIP)
		return artifactMissingVerdictForeign
	}
	demoteMissingRootfsArtifact(ctx, record.ArtifactID, record.Ext4Path)
	record.Status = ArtifactStatusFailed
	return artifactMissingVerdictDemoted
}

// missingArtifactError renders the verdict as the error a caller should return.
func missingArtifactError(record *models.RootfsArtifact, verdict artifactMissingVerdict) error {
	switch verdict {
	case artifactMissingVerdictDemoted:
		return artifactMissingFileError(record.ArtifactID, record.Ext4Path)
	case artifactMissingVerdictForeign:
		return artifactNotServedHereError(record.ArtifactID, record.Ext4Path, record.MasterNodeIP)
	default:
		return nil
	}
}

// readyArtifactUsableForReuse reports whether a READY row may be reused without
// rebuilding. When the backing file is gone it demotes the row as a side effect
// (only if this node owns it) and returns false, so the caller falls through to
// its rebuild path.
//
// Three cases must NOT be reused, all returning false:
//
//   - Missing: the ext4 is gone; demote (if authoritative) and rebuild.
//   - Unknown: a live build marker means another PROCESS (CubeTemplateCenter
//     shares this disk but not the in-process artifactBuildLocks) may be
//     rewriting the ext4 right now. The per-artifact mutex cannot serialise
//     across processes, so Unknown must fall through to claimRootfsArtifactForBuild,
//     whose DB row lock can. Reusing here would distribute a half-written file
//     or a stale sha/token.
//   - Size mismatch: a crash mid-mkfs leaves a full-size sparse file (stat
//     passes, sha256 of the present bytes computes fine), so size alone cannot
//     prove completion -- but a size that DISAGREES with the DB row proves the
//     file is not the one that row describes. This is the check the earlier
//     validateReusableRootfsArtifactFile did and the three-value probe dropped.
//
// ErrRootfsArtifactForeign is returned (wrapped) when a create/redo is asked to
// reuse an artifact whose file lives on a DIFFERENT CubeMaster. Falling through
// to a local rebuild would overwrite the shared row (fresh download_token and
// sha256), invalidating every in-flight pull and already-distributed replica on
// the holder -- the exact #1005 failure this package exists to avoid. Callers
// must surface it rather than rebuild.
var ErrRootfsArtifactForeign = errors.New("rootfs artifact is held by another cubemaster")

func readyArtifactUsableForReuse(ctx context.Context, record *models.RootfsArtifact) bool {
	return rootfsArtifactReuseVerdict(ctx, record) == nil
}

// rootfsArtifactReuseVerdict is readyArtifactUsableForReuse with the reason
// exposed: nil means reusable, ErrRootfsArtifactForeign means the artifact is
// intact but owned elsewhere (must NOT be rebuilt locally), and any other error
// means it must be rebuilt in place.
func rootfsArtifactReuseVerdict(ctx context.Context, record *models.RootfsArtifact) error {
	if record == nil {
		return errors.New("no artifact record")
	}
	if !rootfsArtifactSizeMatches(record) {
		log.G(ctx).Warnf("rootfs artifact %s: on-disk size does not match the row (path=%q recorded=%d); rebuilding",
			record.ArtifactID, record.Ext4Path, record.Ext4SizeBytes)
		return errors.New("artifact size mismatch")
	}
	verdict := resolveMissingArtifact(ctx, record)
	switch verdict {
	case artifactMissingVerdictForeign:
		// The file is absent here but intact on the holder. Do NOT rebuild:
		// overwriting the shared row is strictly worse than refusing.
		return ErrRootfsArtifactForeign
	case artifactMissingVerdictDemoted:
		return errors.New("artifact file missing; row demoted")
	}
	// resolveMissingArtifact returns None for Unknown presence too. Probe again
	// to distinguish "present" from "unknown": only a definite Present is safe
	// to reuse outside the claim lock.
	if probeArtifactPresence(record.Ext4Path) != artifactPresencePresent {
		return errors.New("artifact presence unknown (build in progress elsewhere)")
	}
	return nil
}

// rootfsArtifactSizeMatches reports whether the on-disk ext4 size agrees with
// the row. A zero recorded size disables the check (legacy rows predate the
// column); a missing/unreadable file does not fail here -- that is the probe's
// job, which runs next and classifies it precisely.
func rootfsArtifactSizeMatches(record *models.RootfsArtifact) bool {
	if record == nil || record.Ext4SizeBytes <= 0 {
		return true
	}
	size, err := statArtifactFile(record.Ext4Path)
	if err != nil {
		return true
	}
	return size == record.Ext4SizeBytes
}
