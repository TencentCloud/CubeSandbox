// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateSerializesPolicyAndPublicTraffic(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	allowPublic := false
	maskRequestHost := "localhost:${PORT}"
	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env", Timeout: 300 * time.Second})
	_, err := client.Create(context.Background(), CreateOptions{
		Network: NetworkOptions{
			AllowPublicTraffic: &allowPublic,
			MaskRequestHost:    &maskRequestHost,
			AllowOut:           []string{"172.67.0.0/16"},
			Rules: []Rule{{
				Name:   "gh",
				Match:  Match{Host: "api.github.com", Scheme: "https"},
				Action: Action{Allow: true, Audit: "metadata", Inject: []Inject{{Header: "Authorization", Secret: "s", Format: "Bearer ${SECRET}"}}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	network, ok := got["network"].(map[string]any)
	if !ok {
		t.Fatalf("network=%#v", got["network"])
	}
	if network["allowPublicTraffic"] != false {
		t.Fatalf("allowPublicTraffic=%#v, want false", network["allowPublicTraffic"])
	}
	require.Equal(t, maskRequestHost, network["maskRequestHost"])
	rules, ok := network["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("rules=%#v", network["rules"])
	}
	rule := rules[0].(map[string]any)
	assertString(t, rule, "name", "gh")
	assertMapString(t, rule["match"], "host", "api.github.com")
	action := rule["action"].(map[string]any)
	if action["allow"] != true {
		t.Fatalf("action.allow=%#v", action["allow"])
	}
}

func TestCreateSerializesRulePortAndNormalizesScheme(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env", Timeout: 300 * time.Second})
	_, err := client.Create(context.Background(), CreateOptions{
		Network: NetworkOptions{
			Rules: []Rule{{
				Name:   "custom-https",
				Match:  Match{Host: "api.example.com", Port: 8443, Scheme: " HTTPS "},
				Action: Action{Allow: true},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	network := got["network"].(map[string]any)
	rules := network["rules"].([]any)
	rule := rules[0].(map[string]any)
	match := rule["match"].(map[string]any)
	if match["port"] != float64(8443) {
		t.Fatalf("match.port=%#v, want 8443", match["port"])
	}
	if match["scheme"] != "https" {
		t.Fatalf("match.scheme=%#v, want normalized https", match["scheme"])
	}
}

func TestCreateRejectsInvalidRulePortScheme(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	tests := []struct {
		name    string
		match   Match
		wantErr string
	}{
		{"port without scheme", Match{Host: "a.com", Port: 8443}, "port requires match.scheme"},
		{"port too low", Match{Host: "a.com", Port: 0, Scheme: "https"}, "not-reached"}, // 0 means unset
		{"port negative", Match{Host: "a.com", Port: -1, Scheme: "https"}, "[1, 65535]"},
		{"port too high", Match{Host: "a.com", Port: 65536, Scheme: "https"}, "[1, 65535]"},
		{"unknown scheme", Match{Host: "a.com", Scheme: "gopher"}, "must be 'http' or 'https'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr == "not-reached" {
				return // Port 0 is the zero value (unset); covered by the "port without scheme" case
			}
			client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env"})
			_, err := client.Create(context.Background(), CreateOptions{
				Network: NetworkOptions{
					Rules: []Rule{{Name: "r1", Match: tt.match, Action: Action{Allow: true}}},
				},
			})
			if err == nil {
				t.Fatalf("err=nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err=%q, want substring %q", err, tt.wantErr)
			}
		})
	}
	if called {
		t.Fatal("server was called despite client-side validation failure")
	}
}

func TestCreateRejectsOversizedInjectSecret(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env"})
	_, err := client.Create(context.Background(), CreateOptions{
		Network: NetworkOptions{
			Rules: []Rule{{
				Name:  "r1",
				Match: Match{Host: "api.example.com"},
				Action: Action{
					Allow:  true,
					Inject: []Inject{{Header: "Authorization", Secret: strings.Repeat("x", secretMaxBytes+1)}},
				},
			}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds 2048 bytes") {
		t.Fatalf("err=%v, want inject.secret cap", err)
	}
	if called {
		t.Fatal("server was called despite client-side validation failure")
	}
}

func TestCreateAcceptsInjectSecretAtCap(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env", Timeout: 300 * time.Second})
	_, err := client.Create(context.Background(), CreateOptions{
		Network: NetworkOptions{
			Rules: []Rule{{
				Name:  "r1",
				Match: Match{Host: "api.example.com"},
				Action: Action{
					Allow:  true,
					Inject: []Inject{{Header: "Authorization", Secret: strings.Repeat("x", secretMaxBytes)}},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestMatchValidateAcceptsLegacySchemeOnly(t *testing.T) {
	// Scheme alone (no port) filters HTTP vs HTTPS on the default {80, 443}
	// set — the classic behavior, not the port-scoped feature.
	m := Match{Host: "api.example.com", Scheme: "HTTPS"}
	if err := m.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := m.normalized().Scheme; got != "https" {
		t.Fatalf("normalized scheme=%q, want https", got)
	}
}

func TestCreateRejectsAllowOutDomainWithoutDenyAll(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env"})
	_, err := client.Create(context.Background(), CreateOptions{
		Network: NetworkOptions{AllowOut: []string{"example.com"}},
	})
	if err == nil || !strings.Contains(err.Error(), "deny_out") {
		t.Fatalf("err=%v, want allow_out domain guard", err)
	}
	if called {
		t.Fatal("request should not be sent when validation fails")
	}
}

func TestCreateRejectsAllowOutDomainWhenOnlyAllowPublicTrafficDisabled(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	allowPublic := false
	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env"})
	_, err := client.Create(context.Background(), CreateOptions{
		Network: NetworkOptions{
			AllowPublicTraffic: &allowPublic,
			AllowOut:           []string{"api.example.com"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "deny_out") {
		t.Fatalf("err=%v, want allow_out domain guard", err)
	}
	if called {
		t.Fatal("request should not be sent when validation fails")
	}
}

func TestCreateAcceptsAllowOutDomainWhenInternetAccessDisabled(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	disableInternet := false
	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env"})
	_, err := client.Create(context.Background(), CreateOptions{
		AllowInternetAccess: &disableInternet,
		Network: NetworkOptions{
			AllowOut: []string{"api.example.com"},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got["allowInternetAccess"] != false {
		t.Fatalf("allowInternetAccess=%#v, want false", got["allowInternetAccess"])
	}
	network, ok := got["network"].(map[string]any)
	if !ok {
		t.Fatalf("network=%#v", got["network"])
	}
	assertStringSlice(t, network["allowOut"], []string{"api.example.com"})
}

func TestInjectRender(t *testing.T) {
	if got := (Inject{Secret: "tok"}).Render(); got != "tok" {
		t.Fatalf("default render=%q", got)
	}
	if got := (Inject{Secret: "tok", Format: "Bearer ${SECRET}"}).Render(); got != "Bearer tok" {
		t.Fatalf("formatted render=%q", got)
	}
}

func TestSnapshotLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/"+testSandboxID+"/snapshots":
			// The server rejects an empty/null body with 422, so an empty name
			// must still produce a JSON object body (not a nil/absent body).
			raw, _ := io.ReadAll(r.Body)
			trimmed := strings.TrimSpace(string(raw))
			if trimmed == "" || trimmed == "null" {
				t.Fatalf("CreateSnapshot sent empty/null body: %q", trimmed)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("CreateSnapshot body not a JSON object: %q (%v)", trimmed, err)
			}
			fmt.Fprint(w, `{"snapshotID":"snap-1","names":["n1"]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots":
			if r.URL.Query().Get("sandboxID") != testSandboxID || r.URL.Query().Get("limit") != "50" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			w.Header().Set("x-next-token", "tok-2")
			fmt.Fprint(w, `[{"snapshotID":"snap-1","names":["n1"]}]`)
		case r.Method == http.MethodDelete && r.URL.Path == "/templates/snap-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/"+testSandboxID+"/rollback":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["snapshotID"] != "snap-1" {
				t.Fatalf("rollback body=%#v", body)
			}
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, Timeout: 300 * time.Second})
	sb := &Sandbox{client: client, SandboxID: testSandboxID}
	ctx := context.Background()

	snap, err := sb.CreateSnapshot(ctx, "")
	if err != nil || snap.SnapshotID != "snap-1" || len(snap.Names) != 1 {
		t.Fatalf("CreateSnapshot=%#v err=%v", snap, err)
	}

	items, next, err := client.ListSnapshots(ctx, ListSnapshotsOptions{SandboxID: testSandboxID, Limit: 50})
	if err != nil || len(items) != 1 || next != "tok-2" {
		t.Fatalf("ListSnapshots items=%#v next=%q err=%v", items, next, err)
	}

	if err := client.DeleteSnapshot(ctx, "snap-1"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if err := client.DeleteSnapshot(ctx, ""); err == nil {
		t.Fatal("DeleteSnapshot without id returned nil error")
	}

	result, err := sb.Rollback(ctx, "snap-1")
	if err != nil || result["status"] != "success" {
		t.Fatalf("Rollback=%#v err=%v", result, err)
	}
}

func TestCloneDeletesSnapshotAfterLastCloneIsKilled(t *testing.T) {
	var created atomic.Int32
	var snapshotDeletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshots"):
			fmt.Fprint(w, `{"snapshotID":"snap-clone"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			id := created.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, sandboxJSON(fmt.Sprintf("clone-%d", id), "snap-clone"))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/templates/snap-clone":
			snapshotDeletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, Timeout: 300 * time.Second})
	source := &Sandbox{client: client, SandboxID: testSandboxID}
	clones, err := source.Clone(context.Background(), CloneOptions{N: 2})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if snapshotDeletes.Load() != 0 {
		t.Fatalf("snapshot deleted before clones were killed")
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(clones))
	for _, clone := range clones {
		wg.Add(1)
		go func(clone *Sandbox) {
			defer wg.Done()
			errs <- clone.Kill(context.Background())
		}(clone)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("kill clone: %v", err)
		}
	}
	if err := clones[1].Kill(context.Background()); err != nil {
		t.Fatalf("repeat kill last clone: %v", err)
	}
	if got := snapshotDeletes.Load(); got != 1 {
		t.Fatalf("snapshotDeletes=%d, want 1", got)
	}
}

func TestCloneKillsSiblingsOnFailure(t *testing.T) {
	var created, killed, snapshotDeletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshots"):
			fmt.Fprint(w, `{"snapshotID":"snap-1"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			id := created.Add(1)
			if id == 2 { // fail the second clone
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, sandboxJSON(fmt.Sprintf("clone-%d", id), "snap-1"))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
			killed.Add(1)
			http.Error(w, `{"message":"transient kill failure"}`, http.StatusInternalServerError)
		case r.Method == http.MethodDelete && r.URL.Path == "/templates/snap-1":
			snapshotDeletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, Timeout: 300 * time.Second})
	sb := &Sandbox{client: client, SandboxID: testSandboxID}
	if _, err := sb.Clone(context.Background(), CloneOptions{N: 2}); err == nil {
		t.Fatal("Clone with a failing create returned nil error")
	}
	if got := killed.Load(); got != 1 {
		t.Fatalf("killed=%d, want 1 surviving sibling cleanup attempted", got)
	}
	if got := snapshotDeletes.Load(); got != 1 {
		t.Fatalf("snapshotDeletes=%d, want 1 after sibling kill failure", got)
	}
}

func TestFilesWriteOctetStreamThenMultipartFallback(t *testing.T) {
	var contentTypes []string
	var usernames []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/files" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		ct := r.Header.Get("Content-Type")
		contentTypes = append(contentTypes, ct)
		usernames = append(usernames, r.URL.Query().Get("username"))
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasPrefix(ct, "application/octet-stream") {
			http.Error(w, "use multipart", http.StatusBadRequest) // force fallback
			return
		}
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Fatalf("unexpected content-type %q", ct)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{ProxyNodeIP: host, ProxyPortHTTP: port, SandboxDomain: "cube.test", RequestTimeout: time.Second})
	sb := &Sandbox{client: client, SandboxID: "sb-files", EnvdAccessToken: "tok"}

	if err := sb.Files().ForUser("app").Write(context.Background(), "/tmp/x.txt", []byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(contentTypes) != 2 {
		t.Fatalf("attempts=%d, want 2 (octet-stream then multipart)", len(contentTypes))
	}
	if len(usernames) != 2 || usernames[0] != "app" || usernames[1] != "app" {
		t.Fatalf("usernames=%v, want app on both upload attempts", usernames)
	}
	if _, _, err := mime.ParseMediaType(contentTypes[1]); err != nil {
		t.Fatalf("multipart content-type=%q: %v", contentTypes[1], err)
	}

	contentTypes = nil
	usernames = nil
	if err := sb.Files().Write(context.Background(), "/tmp/unscoped.txt", []byte("hi")); err != nil {
		t.Fatalf("unscoped Write: %v", err)
	}
	if len(contentTypes) != 2 {
		t.Fatalf("unscoped attempts=%d, want 2 (octet-stream then multipart)", len(contentTypes))
	}
	if len(usernames) != 2 || usernames[0] != "" || usernames[1] != "" {
		t.Fatalf("unscoped usernames=%v, want empty on both upload attempts", usernames)
	}
	if _, _, err := mime.ParseMediaType(contentTypes[1]); err != nil {
		t.Fatalf("unscoped multipart content-type=%q: %v", contentTypes[1], err)
	}
}

func TestCommandsRunSendsUserAuthHeader(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
	}))
	defer server.Close()

	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{ProxyNodeIP: host, ProxyPortHTTP: port, SandboxDomain: "cube.test", RequestTimeout: time.Second})
	sb := &Sandbox{client: client, SandboxID: "sb-proc"}

	if _, err := sb.Commands().Run(context.Background(), "id", CommandOptions{User: "app"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if auth != basicAuthUser("app") {
		t.Fatalf("Authorization=%q, want %q", auth, basicAuthUser("app"))
	}
	// Empty user defaults to root.
	if basicAuthUser("") != basicAuthUser("root") {
		t.Fatal("empty user should default to root")
	}
}
