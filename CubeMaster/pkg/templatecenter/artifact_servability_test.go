// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

// stubRemoteTier pins the topology discriminator and the network seam for
// one test, restoring both on cleanup.
func stubRemoteTier(t *testing.T, remote bool, probe func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error)) {
	t.Helper()
	origTier := artifactServedByRemoteTier
	artifactServedByRemoteTier = func() bool { return remote }
	t.Setenv(constants.TemplateCallbackTokenEnv, "existing-component-token")
	t.Setenv(constants.TemplateCenterDownloadAddrEnv, "http://tc.example:8090")
	servabilityProbeMu.Lock()
	servabilityProbeCache = map[string]artifactServabilityProbe{}
	servabilityProbeMu.Unlock()
	t.Cleanup(func() {
		artifactServedByRemoteTier = origTier
	})
	if probe != nil {
		origGet := getArtifactDownloadRange
		getArtifactDownloadRange = probe
		t.Cleanup(func() { getArtifactDownloadRange = origGet })
	}
}

func servableProbe(status int, artifactID string) func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
	return func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
		h := http.Header{}
		h.Set("X-Cube-Artifact-Id", artifactID)
		h.Set("ETag", "deadbeef")
		return status, h, []byte("x"), nil
	}
}

func envelopeProbe(code int, msg string) func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
	return func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
		body := fmt.Sprintf(`{"ret":{"ret_code":%d,"ret_msg":%q}}`, code, msg)
		return http.StatusOK, http.Header{}, []byte(body), nil
	}
}

func TestArtifactServabilityUsesComponentAuthWithoutNodeTicket(t *testing.T) {
	stubRemoteTier(t, true, nil)
	status := http.StatusOK
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodHead || r.URL.Path != "/tc/api/v1/artifacts/probe-artifact/ext4" {
			t.Error("incorrect probe route")
		}
		if r.Header.Get(constants.TemplateCallbackTokenHeader) != "existing-component-token" {
			t.Error("component auth missing")
		}
		if r.Header.Get("X-Cube-Artifact-Token") != "" || r.Header.Get("X-Cube-Node-ID") != "" {
			t.Error("probe used a node ticket")
		}
		w.Header().Set("X-Cube-Artifact-Id", "probe-artifact")
		w.WriteHeader(status)
	}))
	defer server.Close()
	artifact := &models.RootfsArtifact{ArtifactID: "probe-artifact", Generation: 1, StorageBackend: "fs", MasterNodeIP: "http://master.invalid", OwnerTCDownloadBase: server.URL}
	for _, tc := range []struct {
		status int
		want   artifactServability
	}{{200, artifactServabilityServable}, {404, artifactServabilityMissing}, {401, artifactServabilityUnknown}, {503, artifactServabilityUnknown}} {
		status = tc.status
		got, reason := probeArtifactServability(t.Context(), artifact)
		if got != tc.want {
			t.Fatalf("status=%d got=%v reason=%s", status, got, reason)
		}
	}
	t.Setenv(constants.TemplateCallbackTokenEnv, "")
	if got, _ := probeArtifactServability(t.Context(), artifact); got != artifactServabilityUnknown || calls != 4 {
		t.Fatal("missing auth should not contact TC")
	}
}

func TestClassifyArtifactServability(t *testing.T) {
	artifactHeader := http.Header{}
	artifactHeader.Set("X-Cube-Artifact-Id", "rfs-1")
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   artifactServability
	}{
		{"206 is servable", http.StatusPartialContent, http.Header{}, "", artifactServabilityServable},
		{"200 with artifact header is servable", http.StatusOK, artifactHeader, "", artifactServabilityServable},
		{"plain 404 is missing", http.StatusNotFound, http.Header{}, "", artifactServabilityMissing},
		{"plain 410 is missing", http.StatusGone, http.Header{}, "", artifactServabilityMissing},
		{"not-found envelope is missing", http.StatusOK, http.Header{},
			fmt.Sprintf(`{"ret":{"ret_code":%d,"ret_msg":"artifact source missing"}}`, errorcode.ErrorCode_NotFound),
			artifactServabilityMissing},
		{"other envelope is unknown", http.StatusOK, http.Header{},
			`{"ret":{"ret_code":130500,"ret_msg":"internal"}}`, artifactServabilityUnknown},
		{"unparseable 200 is unknown", http.StatusOK, http.Header{}, "garbage", artifactServabilityUnknown},
		{"5xx is unknown", http.StatusBadGateway, http.Header{}, "", artifactServabilityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyArtifactServability(tc.status, tc.header, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("classify = %v, want %v (reason %q)", got, tc.want, reason)
			}
		})
	}
}

func TestVerifyArtifactServabilityRemoteServable(t *testing.T) {
	stubRemoteTier(t, true, servableProbe(http.StatusPartialContent, "rfs-ok"))
	artifact := &models.RootfsArtifact{
		ArtifactID:    "rfs-ok",
		Status:        ArtifactStatusReady,
		Ext4Path:      "/nonexistent/rfs-ok.ext4",
		MasterNodeIP:  "http://cube-sandbox-master.cube-sandbox.svc.cluster.local:8089",
		DownloadToken: "tok",
	}
	if err := verifyArtifactServability(context.Background(), artifact); err != nil {
		t.Fatalf("servable artifact must pass the preflight, got %v", err)
	}
}

func TestVerifyArtifactServabilityLocalOwnershipSkipsRemoteProbe(t *testing.T) {
	probeCalled := false
	stubRemoteTier(t, true, func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
		probeCalled = true
		return 0, nil, nil, errors.New("must not be called for local ownership")
	})
	artDir := t.TempDir()
	ext4Path := artDir + "/rfs-local.ext4"
	if err := os.WriteFile(ext4Path, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:    "rfs-local",
		Status:        ArtifactStatusReady,
		Ext4Path:      ext4Path,
		MasterNodeIP:  "",
		DownloadToken: "tok",
	}
	if err := verifyArtifactServability(context.Background(), artifact); err != nil {
		t.Fatalf("local-owned artifact must use local check, got %v", err)
	}
	if probeCalled {
		t.Fatal("local-owned artifact must not use remote servability probe")
	}
}

func TestVerifyArtifactServabilityRemoteMissingDemotes(t *testing.T) {
	stubRemoteTier(t, true, envelopeProbe(int(errorcode.ErrorCode_NotFound), "artifact source missing"))
	demoted := ""
	origDemote := demoteUnservableRootfsArtifact
	demoteUnservableRootfsArtifact = func(ctx context.Context, artifact *models.RootfsArtifact, reason string) (bool, error) {
		demoted = artifact.ArtifactID
		return true, nil
	}
	t.Cleanup(func() { demoteUnservableRootfsArtifact = origDemote })

	artifact := &models.RootfsArtifact{
		ArtifactID:    "rfs-gone",
		Status:        ArtifactStatusReady,
		Ext4Path:      "/nonexistent/rfs-gone.ext4",
		MasterNodeIP:  "http://master:8089",
		DownloadToken: "tok",
	}
	err := verifyArtifactServability(context.Background(), artifact)
	if err == nil {
		t.Fatal("missing artifact must fail the preflight")
	}
	if demoted != "rfs-gone" {
		t.Fatalf("missing artifact must be demoted, demoted=%q", demoted)
	}
	if !strings.Contains(err.Error(), "demoted") {
		t.Fatalf("error must say the row was demoted, got %v", err)
	}
}

func TestVerifyArtifactServabilityRemoteUnknownKeepsRow(t *testing.T) {
	stubRemoteTier(t, true, func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
		return 0, nil, nil, errors.New("connection refused")
	})
	demoteCalled := false
	origDemote := demoteUnservableRootfsArtifact
	demoteUnservableRootfsArtifact = func(ctx context.Context, artifact *models.RootfsArtifact, reason string) (bool, error) {
		demoteCalled = true
		return true, nil
	}
	t.Cleanup(func() { demoteUnservableRootfsArtifact = origDemote })

	artifact := &models.RootfsArtifact{
		ArtifactID:    "rfs-unknown",
		Status:        ArtifactStatusReady,
		Ext4Path:      "/nonexistent/rfs-unknown.ext4",
		MasterNodeIP:  "http://master:8089",
		DownloadToken: "tok",
	}
	err := verifyArtifactServability(context.Background(), artifact)
	if err == nil {
		t.Fatal("undecidable probe must fail the preflight")
	}
	if demoteCalled {
		t.Fatal("a transient probe failure must never demote the row")
	}
	if !strings.Contains(err.Error(), "leaving the row untouched") {
		t.Fatalf("error must state the row was left untouched, got %v", err)
	}
}

func TestVerifyArtifactServabilityStatsS3WithoutHTTPProbe(t *testing.T) {
	httpCalled := false
	stubRemoteTier(t, true, func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
		httpCalled = true
		return 0, nil, nil, errors.New("S3 must not be probed over HTTP")
	})
	oldStat := artifactServabilityS3StatFn
	statCalled := false
	artifactServabilityS3StatFn = func(_ context.Context, artifact *models.RootfsArtifact) (bool, error) {
		statCalled = artifact.ArtifactID == "rfs-s3"
		return true, nil
	}
	t.Cleanup(func() { artifactServabilityS3StatFn = oldStat })
	artifact := &models.RootfsArtifact{
		ArtifactID:     "rfs-s3",
		Generation:     1,
		Status:         ArtifactStatusReady,
		StorageBackend: "s3",
		ArtifactURL:    "http://minio:9000/bucket/rfs-s3.ext4?sig=...",
		MasterNodeIP:   "http://master:8089",
		DownloadToken:  "tok-s3",
	}
	if err := verifyArtifactServability(context.Background(), artifact); err != nil {
		t.Fatalf("S3-backed artifact should be probed through the unified download endpoint, got %v", err)
	}
	if !statCalled || httpCalled {
		t.Fatalf("statCalled=%v httpCalled=%v", statCalled, httpCalled)
	}
}

func TestRootfsArtifactReuseVerdictRemoteTier(t *testing.T) {
	record := &models.RootfsArtifact{
		ArtifactID:    "rfs-reuse",
		Status:        ArtifactStatusReady,
		Ext4Path:      "/nonexistent/rfs-reuse.ext4",
		MasterNodeIP:  "http://cube-sandbox-master.cube-sandbox.svc.cluster.local:8089",
		DownloadToken: "tok",
	}

	t.Run("servable means reusable", func(t *testing.T) {
		stubRemoteTier(t, true, servableProbe(http.StatusPartialContent, record.ArtifactID))
		if err := rootfsArtifactReuseVerdict(context.Background(), record); err != nil {
			t.Fatalf("servable artifact must be reusable, got %v", err)
		}
	})

	t.Run("missing demotes and reports rebuild", func(t *testing.T) {
		stubRemoteTier(t, true, envelopeProbe(int(errorcode.ErrorCode_NotFound), "gone"))
		demoted := false
		origDemote := demoteUnservableRootfsArtifact
		demoteUnservableRootfsArtifact = func(ctx context.Context, artifact *models.RootfsArtifact, reason string) (bool, error) {
			demoted = true
			return true, nil
		}
		t.Cleanup(func() { demoteUnservableRootfsArtifact = origDemote })
		err := rootfsArtifactReuseVerdict(context.Background(), record)
		if err == nil || errors.Is(err, ErrRootfsArtifactForeign) {
			t.Fatalf("missing artifact must report rebuild, not foreign: %v", err)
		}
		if !demoted {
			t.Fatal("missing artifact must be demoted")
		}
	})

	t.Run("unknown is neither reusable nor foreign", func(t *testing.T) {
		stubRemoteTier(t, true, func(ctx context.Context, url string, header http.Header) (int, http.Header, []byte, error) {
			return 0, nil, nil, errors.New("timeout")
		})
		err := rootfsArtifactReuseVerdict(context.Background(), record)
		if err == nil {
			t.Fatal("undecidable probe must not be reusable")
		}
		if errors.Is(err, ErrRootfsArtifactForeign) {
			t.Fatalf("the foreign verdict must not fire on the master tier: %v", err)
		}
	})
}
