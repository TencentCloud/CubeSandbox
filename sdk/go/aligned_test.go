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
	"testing"
	"time"
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
	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-env", Timeout: 300 * time.Second})
	_, err := client.Create(context.Background(), CreateOptions{
		Network: NetworkOptions{
			AllowPublicTraffic: &allowPublic,
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

func TestCloneKillsSiblingsOnFailure(t *testing.T) {
	var created, killed int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshots"):
			fmt.Fprint(w, `{"snapshotID":"snap-1"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			created++
			if created == 2 { // fail the second clone
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, sandboxJSON(fmt.Sprintf("clone-%d", created), "snap-1"))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
			killed++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/templates/snap-1":
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
	if killed != 1 {
		t.Fatalf("killed=%d, want 1 surviving sibling cleaned up", killed)
	}
}

func TestFilesWriteOctetStreamThenMultipartFallback(t *testing.T) {
	var contentTypes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/files" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		ct := r.Header.Get("Content-Type")
		contentTypes = append(contentTypes, ct)
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

	if err := sb.Files().Write(context.Background(), "/tmp/x.txt", []byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(contentTypes) != 2 {
		t.Fatalf("attempts=%d, want 2 (octet-stream then multipart)", len(contentTypes))
	}
	if _, _, err := mime.ParseMediaType(contentTypes[1]); err != nil {
		t.Fatalf("multipart content-type=%q: %v", contentTypes[1], err)
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
