// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/redact"
)

// TestLLMEgressRule_PlaintextForTransportAndRedactedForLog verifies that
// LLMEgressRule returns the plaintext API key for transport while redact.Value()
// masks it in logs.
func TestLLMEgressRule_PlaintextForTransportAndRedactedForLog(t *testing.T) {
	const apiKey = "sk-DO-NOT-LOG"

	rule, err := LLMEgressRule(&LLMConfig{
		Provider: "test",
		BaseURL:  "https://llm.example.test/v1",
		APIKey:   apiKey,
	})
	if err != nil {
		t.Fatalf("LLMEgressRule: %v", err)
	}

	// 1. Transport payload must carry the plaintext API key.
	payload, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(payload), apiKey) {
		t.Fatalf("transport payload missing the API key: %s", payload)
	}

	// 2. After redact.Value(), the rule must be safe to log.
	redactedRule := redact.Value(rule).(map[string]interface{})
	action := redactedRule["action"].(map[string]interface{})
	inject := action["inject"].([]interface{})
	inj0 := inject[0].(map[string]interface{})

	if got := inj0["secret"]; got != "***REDACTED***" {
		t.Errorf("redacted secret leaf = %v, want \"***REDACTED***\"", got)
	}

	redactedPayload, err := json.Marshal(redactedRule)
	if err != nil {
		t.Fatalf("json.Marshal(redacted): %v", err)
	}
	if strings.Contains(string(redactedPayload), apiKey) {
		t.Errorf("redacted payload leaked the API key: %s", redactedPayload)
	}
	if !strings.Contains(string(redactedPayload), "Bearer ${SECRET}") {
		t.Errorf("redacted payload dropped the \"format\" field: %s", redactedPayload)
	}
}

// writeOpenclawConfig writes a minimal openclaw.json with the given gateway
// token into dir (creating it if needed), so host-file reads resolve.
func writeOpenclawConfig(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cfg := map[string]interface{}{
		"gateway": map[string]interface{}{
			"auth": map[string]interface{}{"token": token},
		},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "openclaw.json"), data, 0o644); err != nil {
		t.Fatalf("write openclaw.json: %v", err)
	}
}

// writeOpenclawLastGood writes an openclaw.json.last-good file with the given
// token into dir.
func writeOpenclawLastGood(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cfg := map[string]interface{}{
		"gateway": map[string]interface{}{
			"auth": map[string]interface{}{"token": token},
		},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "openclaw.json.last-good"), data, 0o644); err != nil {
		t.Fatalf("write openclaw.json.last-good: %v", err)
	}
}

// freshenLastGood writes openclaw.json.last-good and sets its mtime strictly
// after openclaw.json, so waitLastGoodFresherThanConfig returns immediately.
func freshenLastGood(t *testing.T, dir, token string) {
	t.Helper()
	writeOpenclawLastGood(t, dir, token)
	jsonInfo, err := os.Stat(filepath.Join(dir, "openclaw.json"))
	if err != nil {
		return // openclaw.json absent: condition trivially not met, nothing to do
	}
	future := jsonInfo.ModTime().Add(time.Second)
	_ = os.Chtimes(filepath.Join(dir, "openclaw.json.last-good"), future, future)
}

// fakeEnvd stands in for envd: it feeds each request's script to handle() and
// replies with a Connect stream, so probes need no sandbox.
func fakeEnvd(t *testing.T, handle func(script string, envs map[string]string) string) *http.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) < 5 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			Process struct {
				Args []string          `json:"args"`
				Envs map[string]string `json:"envs"`
			} `json:"process"`
		}
		if err := json.Unmarshal(body[5:], &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		script := ""
		if n := len(req.Process.Args); n > 0 {
			script = req.Process.Args[n-1]
		}

		var stream []byte
		for _, frame := range []map[string]interface{}{
			{"event": map[string]interface{}{"data": map[string]interface{}{"stdout": handle(script, req.Process.Envs)}}},
			{"event": map[string]interface{}{"end": map[string]interface{}{"exitCode": 0}}},
		} {
			payload, _ := json.Marshal(frame)
			header := make([]byte, 5)
			binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
			stream = append(append(stream, header...), payload...)
		}
		_, _ = w.Write(stream)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AGENTHUB_SANDBOX_PROXY_URL", srv.URL)

	return srv.Client()
}

// shrinkGatewayPolling removes the probe backoff so tests never sleep.
func shrinkGatewayPolling(t *testing.T) {
	shrinkGatewayPollingTo(t, 0, 0)
}

// shrinkGatewayPollingTo pins the probe backoff for tests that exercise the
// retry loop itself.
func shrinkGatewayPollingTo(t *testing.T, timeout, interval time.Duration) {
	t.Helper()
	prevTimeout, prevInterval := gatewayReadyTimeout, gatewayReadyInterval
	gatewayReadyTimeout, gatewayReadyInterval = timeout, interval
	t.Cleanup(func() { gatewayReadyTimeout, gatewayReadyInterval = prevTimeout, prevInterval })
}

// TestProbeGatewayAuth_IgnoresImageProxy verifies the probe pins a direct
// connection, so an image's http_proxy cannot answer for the gateway.
func TestProbeGatewayAuth_IgnoresImageProxy(t *testing.T) {
	var script string
	client := fakeEnvd(t, func(s string, _ map[string]string) string {
		script = s
		return "401"
	})

	if _, ok := probeGatewayAuth(client, "sb", "cube.app", "tok"); !ok {
		t.Fatal("probe did not reach a verdict")
	}
	if !strings.Contains(script, "ProxyHandler({})") {
		t.Errorf("probe script does not disable proxies:\n%s", script)
	}
	if strings.Contains(script, "urllib.request.urlopen(") {
		t.Error("probe still uses the default opener, which honours http_proxy")
	}
	// The port must be read in the guest, not baked in by CubeOps: the two
	// environments can disagree, and only the guest's OPENCLAW_PORT is right.
	if !strings.Contains(script, `os.environ.get("OPENCLAW_PORT"`) {
		t.Errorf("probe script does not read the guest OPENCLAW_PORT:\n%s", script)
	}
}

// gatewayStub answers probes the way a gateway enforcing accepted would.
func gatewayStub(accepted string) func(string, map[string]string) string {
	return func(script string, envs map[string]string) string {
		if !strings.Contains(script, gatewayProbePath()) {
			return ""
		}
		if envs["OPENCLAW_PROBE_TOKEN"] == accepted {
			return "200"
		}
		return "401"
	}
}

// TestResolveGatewayToken_PicksTheTokenTheGatewayAccepts is the core guarantee:
// the enforced token wins even when openclaw.json advertises another one.
func TestResolveGatewayToken_PicksTheTokenTheGatewayAccepts(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "stale-file-token")
	writeOpenclawLastGood(t, dir, "enforced-token")

	client := fakeEnvd(t, gatewayStub("enforced-token"))

	got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "generated-by-cubeops")
	if err != nil {
		t.Fatalf("ResolveGatewayToken returned error: %v", err)
	}
	if got != "enforced-token" {
		t.Fatalf("ResolveGatewayToken = %q, want %q", got, "enforced-token")
	}
}

// TestResolveGatewayToken_PrefersApplyToken verifies the token CubeOps just
// wrote is tried first, so the common case costs a single probe.
func TestResolveGatewayToken_PrefersApplyToken(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "stale-file-token")
	freshenLastGood(t, dir, "stale-file-token")

	client := fakeEnvd(t, gatewayStub("generated-by-cubeops"))

	got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "generated-by-cubeops")
	if err != nil {
		t.Fatalf("ResolveGatewayToken returned error: %v", err)
	}
	if got != "generated-by-cubeops" {
		t.Fatalf("ResolveGatewayToken = %q, want %q", got, "generated-by-cubeops")
	}
}

// TestResolveGatewayToken_RejectsAllCandidates verifies a confirmed rejection is
// reported as such, since the caller destroys the sandbox on that signal.
func TestResolveGatewayToken_RejectsAllCandidates(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "wrong-file-token")
	freshenLastGood(t, dir, "wrong-file-token")

	client := fakeEnvd(t, gatewayStub("a-token-nobody-has"))

	got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "also-wrong")
	if !errors.Is(err, ErrGatewayTokenRejected) {
		t.Fatalf("error = %v, want ErrGatewayTokenRejected", err)
	}
	if got != "" {
		t.Errorf("ResolveGatewayToken = %q, want empty", got)
	}
}

// TestResolveGatewayToken_MixedVerdictsAreNotRejection locks the per-candidate
// contract: one 401 plus one inconclusive 502 is not "all rejected".
func TestResolveGatewayToken_MixedVerdictsAreNotRejection(t *testing.T) {
	shrinkGatewayPolling(t)

	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "config-token") // second candidate
	freshenLastGood(t, dir, "config-token")

	client := fakeEnvd(t, func(_ string, envs map[string]string) string {
		switch envs["OPENCLAW_PROBE_TOKEN"] {
		case "": // anonymous readiness probe
			return "401"
		case "apply-token": // first candidate: genuinely wrong
			return "401"
		default: // config-token: gateway hiccup, no verdict
			return "502"
		}
	})

	got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "apply-token")
	if err != nil {
		t.Fatalf("mixed verdicts must not fail creation, got error: %v", err)
	}
	if got != "config-token" {
		t.Fatalf("ResolveGatewayToken = %q, want config-token", got)
	}
}

// TestResolveGatewayToken_NoCandidates covers the no-token fast path: only an
// enforcing gateway fails; open/unprobeable keep the token-less URL.
func TestResolveGatewayToken_NoCandidates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		wantErr bool
	}{
		{"gateway enforcing auth", "401", true},
		{"gateway with auth disabled", "200", false},
		{"gateway not probeable", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkGatewayPolling(t)

			dir := t.TempDir()
			writeOpenclawConfig(t, dir, "")
			freshenLastGood(t, dir, "")

			client := fakeEnvd(t, func(string, map[string]string) string { return tc.status })

			got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "")
			if tc.wantErr && !errors.Is(err, ErrGatewayTokenRejected) {
				t.Fatalf("error = %v, want ErrGatewayTokenRejected", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("error = %v, want nil (token-less URL is allowed here)", err)
			}
			if got != "" {
				t.Errorf("ResolveGatewayToken = %q, want empty", got)
			}
		})
	}
}

// TestResolveGatewayToken_OnlyUnauthorizedIsRejection guards the destructive
// path against statuses that say nothing about the token: a 403 from a second
// authz layer or a transient 5xx must not cost the user their sandbox.
func TestResolveGatewayToken_OnlyUnauthorizedIsRejection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		anonStatus  string
		tokenStatus string
	}{
		{"gateway overloaded", "401", "503"},
		{"extra authz layer", "401", "403"},
		{"endpoint moved", "401", "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkGatewayPolling(t)

			dir := t.TempDir()
			writeOpenclawConfig(t, dir, "file-token")
			freshenLastGood(t, dir, "file-token")

			client := fakeEnvd(t, func(_ string, envs map[string]string) string {
				if envs["OPENCLAW_PROBE_TOKEN"] == "" {
					return tc.anonStatus
				}
				return tc.tokenStatus
			})

			got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "apply-token")
			if err != nil {
				t.Fatalf("status %s must not fail creation, got error: %v", tc.tokenStatus, err)
			}
			if got == "" {
				t.Fatal("no token persisted despite an inconclusive probe")
			}
		})
	}
}

// TestWaitGatewayAuthMode_SettledNonAuthStatusStopsPolling verifies a stable
// non-auth answer is not retried until the timeout expires.
func TestWaitGatewayAuthMode_SettledNonAuthStatusStopsPolling(t *testing.T) {
	shrinkGatewayPollingTo(t, time.Minute, time.Second)

	client := fakeEnvd(t, func(string, map[string]string) string { return "403" })

	start := time.Now()
	mode, status := waitGatewayAuthMode(context.Background(), client, "sb", "cube.app")
	if mode != gatewayAuthUnknown || status != 403 {
		t.Fatalf("mode=%v status=%d, want unknown/403", mode, status)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s, want an early return", elapsed)
	}
}

// TestResolveGatewayToken_InconclusiveProbesDoNotFail guards the destructive
// path: an envd hiccup must not be reported as a rejection, or the caller would
// delete a perfectly good sandbox.
func TestResolveGatewayToken_InconclusiveProbesDoNotFail(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "file-token")
	freshenLastGood(t, dir, "file-token")

	client := fakeEnvd(t, func(_ string, envs map[string]string) string {
		if envs["OPENCLAW_PROBE_TOKEN"] == "" {
			return "401" // the gateway is up and enforcing
		}
		return "envd blew up" // unparseable: no verdict on the token
	})

	got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "apply-token")
	if err != nil {
		t.Fatalf("inconclusive probes must not fail creation, got error: %v", err)
	}
	if got != "file-token" {
		t.Fatalf("ResolveGatewayToken = %q, want the unverified fallback", got)
	}
}

// TestResolveGatewayToken_FallsBackWhenGatewayNotProbeable pins the
// no-regression contract: unprobeable images keep reading openclaw.json.
func TestResolveGatewayToken_FallsBackWhenGatewayNotProbeable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"nothing listening on the gateway port", "0"},
		{"probe path no longer exists", "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkGatewayPolling(t)

			dir := t.TempDir()
			writeOpenclawConfig(t, dir, "file-token")
			freshenLastGood(t, dir, "file-token")

			client := fakeEnvd(t, func(string, map[string]string) string { return tc.status })

			got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "generated-by-cubeops")
			if err != nil {
				t.Fatalf("ResolveGatewayToken returned error: %v", err)
			}
			if got != "file-token" {
				t.Fatalf("ResolveGatewayToken = %q, want %q", got, "file-token")
			}
		})
	}
}

// TestWaitGatewayAuthMode_RetriesUntilReady covers the slow-start path: the
// gateway is silent at first and only later begins enforcing auth.
func TestWaitGatewayAuthMode_RetriesUntilReady(t *testing.T) {
	shrinkGatewayPollingTo(t, time.Second, time.Millisecond)

	probes := 0
	client := fakeEnvd(t, func(string, map[string]string) string {
		probes++
		if probes < 3 {
			return "0"
		}
		return "401"
	})

	mode, status := waitGatewayAuthMode(context.Background(), client, "sb", "cube.app")
	if mode != gatewayAuthEnforced {
		t.Fatalf("mode = %v, want enforced", mode)
	}
	if status != 401 || probes < 3 {
		t.Errorf("status = %d after %d probes, want 401 after retries", status, probes)
	}
}

// TestWaitGatewayAuthMode_StopsOnCancel verifies a cancelled request does not
// keep polling a sandbox nobody is waiting for.
func TestWaitGatewayAuthMode_StopsOnCancel(t *testing.T) {
	shrinkGatewayPollingTo(t, time.Minute, time.Millisecond)

	client := fakeEnvd(t, func(string, map[string]string) string { return "0" })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan gatewayAuthMode, 1)
	go func() {
		mode, _ := waitGatewayAuthMode(ctx, client, "sb", "cube.app")
		done <- mode
	}()

	select {
	case mode := <-done:
		if mode != gatewayAuthUnknown {
			t.Fatalf("mode = %v, want unknown", mode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitGatewayAuthMode ignored the cancelled context")
	}
}

// TestWaitGatewayAuthMode_NilClientReturnsImmediately verifies the probe-less
// case gives up at once instead of burning the whole timeout.
func TestWaitGatewayAuthMode_NilClientReturnsImmediately(t *testing.T) {
	shrinkGatewayPollingTo(t, time.Minute, time.Second)

	start := time.Now()
	mode, _ := waitGatewayAuthMode(context.Background(), nil, "sb", "cube.app")
	if mode != gatewayAuthUnknown {
		t.Fatalf("mode = %v, want unknown", mode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, want an immediate return", elapsed)
	}
}

// TestGatewayProbePath_Overridable verifies a renamed Control-UI route can be
// pointed at without a rebuild.
func TestGatewayProbePath_Overridable(t *testing.T) {
	if got := gatewayProbePath(); got != defaultGatewayProbePath {
		t.Fatalf("gatewayProbePath() = %q, want the default", got)
	}
	t.Setenv("AGENTHUB_GATEWAY_PROBE_PATH", "/__openclaw/v2-config.json")
	if got := gatewayProbePath(); got != "/__openclaw/v2-config.json" {
		t.Fatalf("gatewayProbePath() = %q, want the override", got)
	}
}

// TestResolveGatewayToken_GatewayWithAuthDisabled verifies an open gateway still
// yields a token instead of failing the create.
func TestResolveGatewayToken_GatewayWithAuthDisabled(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "file-token")
	freshenLastGood(t, dir, "file-token")

	client := fakeEnvd(t, func(string, map[string]string) string { return "200" })

	got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "generated-by-cubeops")
	if err != nil {
		t.Fatalf("ResolveGatewayToken returned error: %v", err)
	}
	if got != "file-token" {
		t.Fatalf("ResolveGatewayToken = %q, want %q", got, "file-token")
	}
}

// TestGatewayTokenCandidates_Deduplicates verifies identical tokens are probed
// once, keeping the highest-priority source.
func TestGatewayTokenCandidates_Deduplicates(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "same-token")
	writeOpenclawLastGood(t, dir, "same-token")

	got := gatewayTokenCandidates(nil, "sb", "cube.app", dir, "same-token")
	if len(got) != 1 {
		t.Fatalf("gatewayTokenCandidates returned %d candidates, want 1", len(got))
	}
	if got[0].source != gatewayTokenSourceApply {
		t.Errorf("candidate source = %q, want %q", got[0].source, gatewayTokenSourceApply)
	}
}

// TestSyncGatewayTokenConfig_HostFile verifies the host-side config converges on
// the enforced token, and that a matching file keeps its mtime untouched.
func TestSyncGatewayTokenConfig_HostFile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fileToken string
	}{
		{"stale config is rewritten", "stale-token"},
		{"matching config is untouched", "enforced-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeOpenclawConfig(t, dir, tc.fileToken)

			SyncGatewayTokenConfig(context.Background(), nil, "sb", "cube.app", dir, "enforced-token")

			if got := readOpenclawGatewayTokenFromHostFile(dir, "openclaw.json"); got != "enforced-token" {
				t.Fatalf("token after sync = %q, want %q", got, "enforced-token")
			}
		})
	}
}

// TestSyncGatewayTokenConfig_PreservesOtherFields guards against the rewrite
// dropping unrelated configuration.
func TestSyncGatewayTokenConfig_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	cfg := map[string]interface{}{
		"gateway": map[string]interface{}{
			"auth":           map[string]interface{}{"mode": "token", "token": "stale-token"},
			"trustedProxies": []string{"10.0.0.1"},
		},
		"models": map[string]interface{}{"primary": "deepseek"},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "openclaw.json"), data, 0o600); err != nil {
		t.Fatalf("write openclaw.json: %v", err)
	}

	SyncGatewayTokenConfig(context.Background(), nil, "sb", "cube.app", dir, "enforced-token")

	raw, err := os.ReadFile(filepath.Join(dir, "openclaw.json"))
	if err != nil {
		t.Fatalf("read openclaw.json: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal openclaw.json: %v", err)
	}
	gateway := got["gateway"].(map[string]interface{})
	if token := gateway["auth"].(map[string]interface{})["token"]; token != "enforced-token" {
		t.Errorf("token = %v, want enforced-token", token)
	}
	if gateway["trustedProxies"] == nil {
		t.Error("rewrite dropped gateway.trustedProxies")
	}
	if got["models"] == nil {
		t.Error("rewrite dropped the models section")
	}
}

// TestSyncGatewayTokenConfig_SandboxFile verifies the envd path rewrites through
// the sandbox when there is no host state dir.
func TestSyncGatewayTokenConfig_SandboxFile(t *testing.T) {
	rewritten := false
	client := fakeEnvd(t, func(script string, envs map[string]string) string {
		if strings.Contains(script, "os.replace") {
			rewritten = envs["OPENCLAW_SYNC_TOKEN"] == "enforced-token"
			return ""
		}
		return `{"gateway":{"auth":{"token":"stale-token"}}}`
	})

	SyncGatewayTokenConfig(context.Background(), client, "sb", "cube.app", "", "enforced-token")
	if !rewritten {
		t.Fatal("sandbox config was not rewritten with the enforced token")
	}
}

// TestWaitLastGoodFresherThanConfig_AlreadyFresh verifies an already-fresh
// last-good returns immediately without polling.
func TestWaitLastGoodFresherThanConfig_AlreadyFresh(t *testing.T) {
	shrinkGatewayPollingTo(t, time.Minute, time.Second)

	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "tok")
	freshenLastGood(t, dir, "tok") // mtime set strictly after openclaw.json

	start := time.Now()
	waitLastGoodFresherThanConfig(context.Background(), dir)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, want immediate return when last-good is already fresh", elapsed)
	}
}

// TestWaitLastGoodFresherThanConfig_WaitsUntilFresh verifies the function
// blocks until last-good becomes fresher, then returns promptly.
func TestWaitLastGoodFresherThanConfig_WaitsUntilFresh(t *testing.T) {
	shrinkGatewayPollingTo(t, 2*time.Second, time.Millisecond)

	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "tok")
	// Write last-good with same mtime as json (not yet fresher).
	writeOpenclawLastGood(t, dir, "tok")

	// After a short delay, freshen last-good so the wait unblocks.
	go func() {
		time.Sleep(20 * time.Millisecond)
		freshenLastGood(t, dir, "tok")
	}()

	start := time.Now()
	waitLastGoodFresherThanConfig(context.Background(), dir)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, want to unblock once last-good is fresh", elapsed)
	}
}

// TestWaitLastGoodFresherThanConfig_TimesOutGracefully verifies the function
// returns silently on timeout rather than blocking forever: if last-good never
// becomes fresher, the caller falls through to its existing logic unchanged.
func TestWaitLastGoodFresherThanConfig_TimesOutGracefully(t *testing.T) {
	shrinkGatewayPollingTo(t, 50*time.Millisecond, time.Millisecond)

	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "tok")
	// last-good absent: condition can never be satisfied.

	start := time.Now()
	waitLastGoodFresherThanConfig(context.Background(), dir)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, want silent return on timeout", elapsed)
	}
}

// TestResolveGatewayToken_WaitsForOpenclawSelfPatch covers the post-restart
// token rewrite race: candidates must not be read before last-good is fresh.
func TestResolveGatewayToken_WaitsForOpenclawSelfPatch(t *testing.T) {
	shrinkGatewayPollingTo(t, 2*time.Second, time.Millisecond)

	dir := t.TempDir()
	// Simulate: apply wrote apply-token into openclaw.json, but OpenClaw has
	// not yet finished its self-patch (last-good not written yet).
	writeOpenclawConfig(t, dir, "apply-token")

	// After a short delay, simulate OpenClaw finishing its self-patch:
	// json gets a new stable token and last-good is written fresher.
	go func() {
		time.Sleep(30 * time.Millisecond)
		writeOpenclawConfig(t, dir, "stable-token")
		freshenLastGood(t, dir, "stable-token")
	}()

	// Gateway accepts only the stable token (the one OpenClaw settled on).
	client := fakeEnvd(t, gatewayStub("stable-token"))

	got, err := ResolveGatewayToken(context.Background(), client, "sb", "cube.app", dir, "apply-token")
	if err != nil {
		t.Fatalf("ResolveGatewayToken returned error: %v", err)
	}
	if got != "stable-token" {
		t.Fatalf("ResolveGatewayToken = %q, want stable-token (not the transient apply-token)", got)
	}
}
