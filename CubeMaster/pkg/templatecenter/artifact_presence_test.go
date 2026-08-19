// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
)

// captureArtifactDemotions swaps the demotion seam for a recorder and restores
// it when the test ends. Returned slice collects demoted artifact ids in order.
func captureArtifactDemotions(t *testing.T) *[]string {
	t.Helper()
	var demoted []string
	orig := demoteMissingRootfsArtifact
	demoteMissingRootfsArtifact = func(_ context.Context, artifactID, _ string) {
		demoted = append(demoted, artifactID)
	}
	t.Cleanup(func() { demoteMissingRootfsArtifact = orig })
	return &demoted
}

func stubStatArtifactFile(t *testing.T, fn func(string) (int64, error)) {
	t.Helper()
	orig := statArtifactFile
	statArtifactFile = fn
	t.Cleanup(func() { statArtifactFile = orig })
}

func TestClassifyArtifactPresence(t *testing.T) {
	cases := []struct {
		name            string
		ext4Path        string
		buildInProgress bool
		size            int64
		statErr         error
		want            artifactPresence
	}{
		{
			name:     "non empty file is present",
			ext4Path: "/store/rfs-1/rfs-1.ext4",
			size:     4096,
			want:     artifactPresencePresent,
		},
		{
			name:     "missing file is missing",
			ext4Path: "/store/rfs-1/rfs-1.ext4",
			statErr:  os.ErrNotExist,
			want:     artifactPresenceMissing,
		},
		{
			name:     "wrapped not exist is still missing",
			ext4Path: "/store/rfs-1/rfs-1.ext4",
			statErr:  &fs.PathError{Op: "stat", Path: "/store/rfs-1/rfs-1.ext4", Err: os.ErrNotExist},
			want:     artifactPresenceMissing,
		},
		{
			name:     "zero length file is missing",
			ext4Path: "/store/rfs-1/rfs-1.ext4",
			size:     0,
			want:     artifactPresenceMissing,
		},
		{
			name:     "negative size is missing",
			ext4Path: "/store/rfs-1/rfs-1.ext4",
			size:     -1,
			want:     artifactPresenceMissing,
		},
		{
			name:     "empty path on a ready row is missing",
			ext4Path: "",
			want:     artifactPresenceMissing,
		},
		{
			// A live build owns the directory: the ext4 may legitimately be absent
			// or half-written right now, so the row must not be demoted.
			name:            "build in progress outranks a missing file",
			ext4Path:        "/store/rfs-1/rfs-1.ext4",
			buildInProgress: true,
			statErr:         os.ErrNotExist,
			want:            artifactPresenceUnknown,
		},
		{
			name:            "build in progress outranks an empty path",
			ext4Path:        "",
			buildInProgress: true,
			want:            artifactPresenceUnknown,
		},
		{
			name:            "build in progress outranks a present file",
			ext4Path:        "/store/rfs-1/rfs-1.ext4",
			buildInProgress: true,
			size:            4096,
			want:            artifactPresenceUnknown,
		},
		{
			// Permission / transient I/O errors say nothing about whether the
			// artifact is still there; rebuilding on them would be wrong.
			name:     "permission error is unknown",
			ext4Path: "/store/rfs-1/rfs-1.ext4",
			statErr:  os.ErrPermission,
			want:     artifactPresenceUnknown,
		},
		{
			name:     "opaque io error is unknown",
			ext4Path: "/store/rfs-1/rfs-1.ext4",
			statErr:  errors.New("input/output error"),
			want:     artifactPresenceUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyArtifactPresence(tc.ext4Path, tc.buildInProgress, tc.size, tc.statErr)
			if got != tc.want {
				t.Fatalf("classifyArtifactPresence(%q, %t, %d, %v) = %s, want %s",
					tc.ext4Path, tc.buildInProgress, tc.size, tc.statErr, got, tc.want)
			}
		})
	}
}

func TestProbeArtifactPresenceRealFiles(t *testing.T) {
	dir := t.TempDir()

	present := filepath.Join(dir, "present.ext4")
	if err := os.WriteFile(present, []byte("ext4-bytes"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if got := probeArtifactPresence(present); got != artifactPresencePresent {
		t.Fatalf("present file: got %s, want present", got)
	}

	truncated := filepath.Join(dir, "truncated.ext4")
	if err := os.WriteFile(truncated, nil, 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if got := probeArtifactPresence(truncated); got != artifactPresenceMissing {
		t.Fatalf("truncated file: got %s, want missing", got)
	}

	if got := probeArtifactPresence(filepath.Join(dir, "absent.ext4")); got != artifactPresenceMissing {
		t.Fatalf("absent file: got %s, want missing", got)
	}

	// A directory standing in for the ext4 is corrupt layout, not a usable file.
	asDir := filepath.Join(dir, "dir.ext4")
	if err := os.MkdirAll(asDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if got := probeArtifactPresence(asDir); got != artifactPresenceMissing {
		t.Fatalf("directory: got %s, want missing", got)
	}

	if got := probeArtifactPresence(""); got != artifactPresenceMissing {
		t.Fatalf("empty path: got %s, want missing", got)
	}
}

// A live build marker in the artifact directory must suppress the missing
// verdict even though the ext4 itself is absent.
func TestProbeArtifactPresenceRespectsBuildMarker(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "rfs-marker")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	ext4Path := filepath.Join(storeDir, "rfs-marker.ext4")

	if got := probeArtifactPresence(ext4Path); got != artifactPresenceMissing {
		t.Fatalf("before marker: got %s, want missing", got)
	}

	release, err := image.MarkArtifactBuildInProgress(storeDir)
	if err != nil {
		t.Fatalf("MarkArtifactBuildInProgress failed: %v", err)
	}
	if got := probeArtifactPresence(ext4Path); got != artifactPresenceUnknown {
		t.Fatalf("with marker: got %s, want unknown", got)
	}
	release()

	if got := probeArtifactPresence(ext4Path); got != artifactPresenceMissing {
		t.Fatalf("after marker released: got %s, want missing", got)
	}
}

func TestReadyArtifactUsableForReuseNilRecord(t *testing.T) {
	if readyArtifactUsableForReuse(context.Background(), nil) {
		t.Fatal("nil record must not be reusable")
	}
}

func TestReadyArtifactUsableForReusePresentArtifact(t *testing.T) {
	ext4Path := filepath.Join(t.TempDir(), "rfs-ok.ext4")
	if err := os.WriteFile(ext4Path, []byte("ext4"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	record := &models.RootfsArtifact{
		ArtifactID: "rfs-ok",
		Status:     ArtifactStatusReady,
		Ext4Path:   ext4Path,
	}
	demoted := captureArtifactDemotions(t)
	if !readyArtifactUsableForReuse(context.Background(), record) {
		t.Fatal("present artifact must be reusable")
	}
	if len(*demoted) != 0 {
		t.Fatalf("present artifact must not be demoted, got %v", *demoted)
	}
	if record.Status != ArtifactStatusReady {
		t.Fatalf("status=%q, want %q", record.Status, ArtifactStatusReady)
	}
}

func TestReadyArtifactUsableForReuseMissingArtifact(t *testing.T) {
	// Empty MasterNodeIP means "served from this node", so this node is
	// authoritative and the demotion is correct.
	stubLocalArtifactHosts(t, "master-a")
	record := &models.RootfsArtifact{
		ArtifactID: "rfs-gone",
		Status:     ArtifactStatusReady,
		Ext4Path:   filepath.Join(t.TempDir(), "rfs-gone.ext4"),
	}
	demoted := captureArtifactDemotions(t)
	if readyArtifactUsableForReuse(context.Background(), record) {
		t.Fatal("missing artifact must not be reusable")
	}
	if len(*demoted) != 1 || (*demoted)[0] != "rfs-gone" {
		t.Fatalf("expected rfs-gone to be demoted, got %v", *demoted)
	}
	// The in-memory record must move off READY too, otherwise the caller's later
	// status checks would still treat the artifact as reusable.
	if record.Status != ArtifactStatusFailed {
		t.Fatalf("status=%q, want %q", record.Status, ArtifactStatusFailed)
	}
}

func TestReadyArtifactUsableForReuseUnknownPresence(t *testing.T) {
	// Unknown presence (a stat error, or a live build marker owned by another
	// process) must NOT be reused: the per-artifact mutex is in-process only and
	// cannot serialise against a sibling CubeTemplateCenter rewriting the ext4.
	// The caller must fall through to the claim path, whose DB row lock can.
	stubStatArtifactFile(t, func(string) (int64, error) { return 0, os.ErrPermission })
	record := &models.RootfsArtifact{
		ArtifactID: "rfs-unknown",
		Status:     ArtifactStatusReady,
		Ext4Path:   filepath.Join(t.TempDir(), "rfs-unknown.ext4"),
	}
	demoted := captureArtifactDemotions(t)
	if readyArtifactUsableForReuse(context.Background(), record) {
		t.Fatal("unknown presence must not be reused outside the claim lock")
	}
	// Unknown must NOT demote either: a transient stat error says nothing about
	// whether the artifact is still there, and rebuilding over an EIO is worse.
	if len(*demoted) != 0 {
		t.Fatalf("unknown presence must not demote, got %v", *demoted)
	}
	if record.Status != ArtifactStatusReady {
		t.Fatalf("status=%q, want %q", record.Status, ArtifactStatusReady)
	}
}

func TestArtifactMissingFileErrorNamesBothHalves(t *testing.T) {
	err := artifactMissingFileError("rfs-1", "/store/rfs-1/rfs-1.ext4")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"rfs-1", "/store/rfs-1/rfs-1.ext4", "missing on disk", "rebuilds it"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

func TestArtifactPresenceString(t *testing.T) {
	cases := map[artifactPresence]string{
		artifactPresencePresent: "present",
		artifactPresenceMissing: "missing",
		artifactPresenceUnknown: "unknown",
		artifactPresence(42):    "unknown",
	}
	for presence, want := range cases {
		if got := presence.String(); got != want {
			t.Fatalf("presence(%d).String()=%q, want %q", int(presence), got, want)
		}
	}
}

// stubLocalArtifactHosts pins the "this node" identity set so ownership tests do
// not depend on the machine running them.
func stubLocalArtifactHosts(t *testing.T, hosts ...string) {
	t.Helper()
	set := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		set[strings.ToLower(h)] = true
	}
	orig := localArtifactHostsFn
	localArtifactHostsFn = func() map[string]bool { return set }
	t.Cleanup(func() { localArtifactHostsFn = orig })
}

func TestArtifactServedLocally(t *testing.T) {
	local := map[string]bool{
		"localhost":     true,
		"127.0.0.1":     true,
		"10.228.161.10": true,
		"master-a":      true,
	}
	cases := []struct {
		name    string
		baseURL string
		want    bool
	}{
		// An empty column means buildDownloadURL falls back to this host, so the
		// artifact would be served from here anyway.
		{name: "empty base url is local", baseURL: "", want: true},
		{name: "whitespace base url is local", baseURL: "   ", want: true},
		{name: "local ip with scheme and port", baseURL: "http://10.228.161.10:8080", want: true},
		{name: "local ip bare", baseURL: "10.228.161.10", want: true},
		{name: "local ip bare with port", baseURL: "10.228.161.10:8080", want: true},
		{name: "local hostname", baseURL: "http://master-a:8080", want: true},
		{name: "case insensitive hostname", baseURL: "http://MASTER-A", want: true},
		{name: "loopback", baseURL: "http://127.0.0.1:8080", want: true},
		// The other master in the HA pair: absolutely must not be treated as local.
		{name: "peer master ip is foreign", baseURL: "http://10.228.161.14:8080", want: false},
		// A load balancer VIP or service name cannot be proven local.
		{name: "load balancer vip is foreign", baseURL: "http://cube-master-lb:8080", want: false},
		{name: "service dns name is foreign", baseURL: "https://cubemaster.svc.cluster.local", want: false},
		{name: "unparseable is foreign", baseURL: "http://[::1", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := artifactServedLocally(tc.baseURL, local); got != tc.want {
				t.Fatalf("artifactServedLocally(%q) = %t, want %t", tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestLocalArtifactHostsIncludesLoopbackAndHostname(t *testing.T) {
	hosts := localArtifactHosts()
	for _, want := range []string{"localhost", "127.0.0.1"} {
		if !hosts[want] {
			t.Fatalf("localArtifactHosts() missing %q, got %v", want, hosts)
		}
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		if !hosts[strings.ToLower(name)] {
			t.Fatalf("localArtifactHosts() missing hostname %q", name)
		}
	}
}

// The #852 shape: this node owns the artifact and the file really is gone, so
// the row must be demoted to break the retry deadlock.
func TestResolveMissingArtifactDemotesOwnedArtifact(t *testing.T) {
	stubLocalArtifactHosts(t, "master-a")
	record := &models.RootfsArtifact{
		ArtifactID:   "rfs-owned",
		Status:       ArtifactStatusReady,
		Ext4Path:     filepath.Join(t.TempDir(), "rfs-owned.ext4"),
		MasterNodeIP: "http://master-a:8080",
	}
	demoted := captureArtifactDemotions(t)

	if got := resolveMissingArtifact(context.Background(), record); got != artifactMissingVerdictDemoted {
		t.Fatalf("verdict=%d, want demoted", got)
	}
	if len(*demoted) != 1 || (*demoted)[0] != "rfs-owned" {
		t.Fatalf("expected rfs-owned demoted, got %v", *demoted)
	}
	if record.Status != ArtifactStatusFailed {
		t.Fatalf("status=%q, want %q", record.Status, ArtifactStatusFailed)
	}
	err := missingArtifactError(record, artifactMissingVerdictDemoted)
	if err == nil || !strings.Contains(err.Error(), "demoted") {
		t.Fatalf("expected a demotion diagnostic, got %v", err)
	}
}

// The #1005 shape: two CubeMasters, node-local artifact store, request landed on
// the node that never built the artifact. The shared row must survive untouched.
func TestResolveMissingArtifactLeavesForeignArtifactAlone(t *testing.T) {
	stubLocalArtifactHosts(t, "10.228.161.10")
	record := &models.RootfsArtifact{
		ArtifactID:   "rfs-foreign",
		Status:       ArtifactStatusReady,
		Ext4Path:     filepath.Join(t.TempDir(), "rfs-foreign.ext4"),
		MasterNodeIP: "http://10.228.161.14:8080",
	}
	demoted := captureArtifactDemotions(t)

	if got := resolveMissingArtifact(context.Background(), record); got != artifactMissingVerdictForeign {
		t.Fatalf("verdict=%d, want foreign", got)
	}
	if len(*demoted) != 0 {
		t.Fatalf("a foreign artifact must never be demoted, got %v", *demoted)
	}
	// Leaving the row READY is the whole point: the peer still serves it.
	if record.Status != ArtifactStatusReady {
		t.Fatalf("status=%q, want %q", record.Status, ArtifactStatusReady)
	}
	err := missingArtifactError(record, artifactMissingVerdictForeign)
	if err == nil {
		t.Fatal("expected a diagnostic")
	}
	for _, want := range []string{"rfs-foreign", "10.228.161.14", "node-local", "untouched"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestResolveMissingArtifactPresentFileIsNone(t *testing.T) {
	stubLocalArtifactHosts(t, "master-a")
	ext4Path := filepath.Join(t.TempDir(), "rfs-present.ext4")
	if err := os.WriteFile(ext4Path, []byte("ext4"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	record := &models.RootfsArtifact{
		ArtifactID:   "rfs-present",
		Status:       ArtifactStatusReady,
		Ext4Path:     ext4Path,
		MasterNodeIP: "http://master-a:8080",
	}
	demoted := captureArtifactDemotions(t)

	if got := resolveMissingArtifact(context.Background(), record); got != artifactMissingVerdictNone {
		t.Fatalf("verdict=%d, want none", got)
	}
	if len(*demoted) != 0 {
		t.Fatalf("present artifact must not be demoted, got %v", *demoted)
	}
	if err := missingArtifactError(record, artifactMissingVerdictNone); err != nil {
		t.Fatalf("verdict none must yield no error, got %v", err)
	}
}

func TestResolveMissingArtifactNilRecord(t *testing.T) {
	if got := resolveMissingArtifact(context.Background(), nil); got != artifactMissingVerdictNone {
		t.Fatalf("verdict=%d, want none", got)
	}
}

// A foreign artifact must not be reused either: this node would hand cubelets a
// download URL it cannot serve.
func TestReadyArtifactUsableForReuseRejectsForeignArtifact(t *testing.T) {
	stubLocalArtifactHosts(t, "10.228.161.10")
	record := &models.RootfsArtifact{
		ArtifactID:   "rfs-foreign-reuse",
		Status:       ArtifactStatusReady,
		Ext4Path:     filepath.Join(t.TempDir(), "rfs-foreign-reuse.ext4"),
		MasterNodeIP: "http://10.228.161.14:8080",
	}
	demoted := captureArtifactDemotions(t)

	if readyArtifactUsableForReuse(context.Background(), record) {
		t.Fatal("foreign artifact must not be reusable on this node")
	}
	if len(*demoted) != 0 {
		t.Fatalf("foreign artifact must not be demoted, got %v", *demoted)
	}
	if record.Status != ArtifactStatusReady {
		t.Fatalf("status=%q, want %q", record.Status, ArtifactStatusReady)
	}
}

// The size check guards against the case neither stat nor sha256 can catch: a
// crash mid-mkfs leaves a full-size sparse file. If the on-disk size disagrees
// with the row, the file is not the one the row describes and must not be
// reused.
func TestRootfsArtifactReuseVerdictSizeMismatch(t *testing.T) {
	ext4Path := filepath.Join(t.TempDir(), "rfs-size.ext4")
	if err := os.WriteFile(ext4Path, []byte("partial"), 0o644); err != nil { // 7 bytes
		t.Fatalf("WriteFile failed: %v", err)
	}
	record := &models.RootfsArtifact{
		ArtifactID:    "rfs-size",
		Status:        ArtifactStatusReady,
		Ext4Path:      ext4Path,
		Ext4SizeBytes: 1 << 30, // row says 1 GiB, file is 7 bytes
	}
	demoted := captureArtifactDemotions(t)

	err := rootfsArtifactReuseVerdict(context.Background(), record)
	if err == nil {
		t.Fatal("size mismatch must not be reusable")
	}
	if errors.Is(err, ErrRootfsArtifactForeign) {
		t.Fatalf("size mismatch is not a foreign-artifact case, got %v", err)
	}
	// Size mismatch must NOT demote: the row may still be correct and the file
	// simply not yet finalized by a concurrent build.
	if len(*demoted) != 0 {
		t.Fatalf("size mismatch must not demote, got %v", *demoted)
	}
	if readyArtifactUsableForReuse(context.Background(), record) {
		t.Fatal("size mismatch must not be reusable via the bool wrapper")
	}
}

// A matching size (and present file) stays reusable.
func TestRootfsArtifactReuseVerdictSizeMatch(t *testing.T) {
	ext4Path := filepath.Join(t.TempDir(), "rfs-match.ext4")
	body := []byte("exactly-this-content")
	if err := os.WriteFile(ext4Path, body, 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	record := &models.RootfsArtifact{
		ArtifactID:    "rfs-match",
		Status:        ArtifactStatusReady,
		Ext4Path:      ext4Path,
		Ext4SizeBytes: int64(len(body)),
	}
	if err := rootfsArtifactReuseVerdict(context.Background(), record); err != nil {
		t.Fatalf("matching size must be reusable, got %v", err)
	}
}

// A zero recorded size disables the size check (legacy rows predate the
// column), so a present file stays reusable.
func TestRootfsArtifactReuseVerdictZeroRecordedSize(t *testing.T) {
	ext4Path := filepath.Join(t.TempDir(), "rfs-legacy.ext4")
	if err := os.WriteFile(ext4Path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	record := &models.RootfsArtifact{
		ArtifactID:    "rfs-legacy",
		Status:        ArtifactStatusReady,
		Ext4Path:      ext4Path,
		Ext4SizeBytes: 0,
	}
	if err := rootfsArtifactReuseVerdict(context.Background(), record); err != nil {
		t.Fatalf("zero recorded size must disable the size check, got %v", err)
	}
}

// rootfsArtifactReuseVerdict must surface ErrRootfsArtifactForeign so callers
// can refuse to rebuild instead of overwriting the shared row.
func TestRootfsArtifactReuseVerdictForeignIsSentinel(t *testing.T) {
	stubLocalArtifactHosts(t, "10.228.161.10")
	record := &models.RootfsArtifact{
		ArtifactID:   "rfs-foreign-sentinel",
		Status:       ArtifactStatusReady,
		Ext4Path:     filepath.Join(t.TempDir(), "absent.ext4"),
		MasterNodeIP: "http://10.228.161.14:8080",
	}
	demoted := captureArtifactDemotions(t)

	err := rootfsArtifactReuseVerdict(context.Background(), record)
	if !errors.Is(err, ErrRootfsArtifactForeign) {
		t.Fatalf("expected ErrRootfsArtifactForeign, got %v", err)
	}
	// The holder's row must be left untouched.
	if len(*demoted) != 0 {
		t.Fatalf("foreign artifact must not be demoted, got %v", *demoted)
	}
	if record.Status != ArtifactStatusReady {
		t.Fatalf("status=%q, want %q", record.Status, ArtifactStatusReady)
	}
}
