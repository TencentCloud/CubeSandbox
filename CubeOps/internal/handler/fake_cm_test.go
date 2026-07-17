// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeCM is a stub CubeMasterClient that returns canned responses. Each
// field controls one method; nil fields return an error so tests fail loud
// if a handler calls a method the test didn't set up.
type fakeCM struct {
	getNodes                    func(ctx context.Context) (json.RawMessage, error)
	clusterVersions             func(ctx context.Context) (json.RawMessage, error)
	getNode                     func(ctx context.Context, nodeID string) (json.RawMessage, error)
	listSandboxes               func(ctx context.Context) (json.RawMessage, error)
	getSandbox                  func(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error)
	createSandbox               func(ctx context.Context, body interface{}) (json.RawMessage, error)
	deleteSandbox               func(ctx context.Context, body interface{}) (json.RawMessage, error)
	updateSandbox               func(ctx context.Context, body interface{}) (json.RawMessage, error)
	connectSandboxWithBody      func(ctx context.Context, body interface{}) (json.RawMessage, error)
	setSandboxTimeout           func(ctx context.Context, body interface{}) (json.RawMessage, error)
	refreshSandbox              func(ctx context.Context, body interface{}) (json.RawMessage, error)
	getSandboxLogs              func(ctx context.Context, body interface{}) (json.RawMessage, error)
	listSandboxesWithBody       func(ctx context.Context, body interface{}) (json.RawMessage, error)
	createSnapshot              func(ctx context.Context, body interface{}) (json.RawMessage, error)
	listSnapshots               func(ctx context.Context, params map[string]string) (json.RawMessage, error)
	deleteSnapshot              func(ctx context.Context, snapshotID string) (json.RawMessage, error)
	rollbackSandbox             func(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error)
	listTemplates               func(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error)
	createTemplateFromImage     func(ctx context.Context, body interface{}) (json.RawMessage, error)
	redoTemplate                func(ctx context.Context, body interface{}) (json.RawMessage, error)
	deleteTemplate              func(ctx context.Context, body interface{}) (json.RawMessage, error)
	getTemplateBuildStatus      func(ctx context.Context, buildID string) (json.RawMessage, error)
	startTemplateBuild          func(ctx context.Context, buildID string, body interface{}) (json.RawMessage, error)
	getTemplateCompat           func(ctx context.Context) (json.RawMessage, error)
	adoptTemplateCompatBaseline func(ctx context.Context, body interface{}) (json.RawMessage, error)
}

func (f *fakeCM) GetNodes(ctx context.Context) (json.RawMessage, error) {
	if f.getNodes == nil {
		return nil, errFakeNotConfigured
	}
	return f.getNodes(ctx)
}
func (f *fakeCM) ClusterVersions(ctx context.Context) (json.RawMessage, error) {
	if f.clusterVersions == nil {
		return nil, errFakeNotConfigured
	}
	return f.clusterVersions(ctx)
}
func (f *fakeCM) GetNode(ctx context.Context, nodeID string) (json.RawMessage, error) {
	if f.getNode == nil {
		return nil, errFakeNotConfigured
	}
	return f.getNode(ctx, nodeID)
}
func (f *fakeCM) ListSandboxes(ctx context.Context) (json.RawMessage, error) {
	if f.listSandboxes == nil {
		return nil, errFakeNotConfigured
	}
	return f.listSandboxes(ctx)
}
func (f *fakeCM) GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
	if f.getSandbox == nil {
		return nil, errFakeNotConfigured
	}
	return f.getSandbox(ctx, sandboxID, instanceType)
}
func (f *fakeCM) CreateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.createSandbox == nil {
		return nil, errFakeNotConfigured
	}
	return f.createSandbox(ctx, body)
}
func (f *fakeCM) DeleteSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.deleteSandbox == nil {
		return nil, errFakeNotConfigured
	}
	return f.deleteSandbox(ctx, body)
}
func (f *fakeCM) UpdateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.updateSandbox == nil {
		return nil, errFakeNotConfigured
	}
	return f.updateSandbox(ctx, body)
}
func (f *fakeCM) ConnectSandboxWithBody(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.connectSandboxWithBody == nil {
		return nil, errFakeNotConfigured
	}
	return f.connectSandboxWithBody(ctx, body)
}
func (f *fakeCM) SetSandboxTimeout(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.setSandboxTimeout == nil {
		return nil, errFakeNotConfigured
	}
	return f.setSandboxTimeout(ctx, body)
}
func (f *fakeCM) RefreshSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.refreshSandbox == nil {
		return nil, errFakeNotConfigured
	}
	return f.refreshSandbox(ctx, body)
}
func (f *fakeCM) GetSandboxLogs(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.getSandboxLogs == nil {
		return nil, errFakeNotConfigured
	}
	return f.getSandboxLogs(ctx, body)
}
func (f *fakeCM) ListSandboxesWithBody(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.listSandboxesWithBody == nil {
		return nil, errFakeNotConfigured
	}
	return f.listSandboxesWithBody(ctx, body)
}
func (f *fakeCM) CreateSnapshot(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.createSnapshot == nil {
		return nil, errFakeNotConfigured
	}
	return f.createSnapshot(ctx, body)
}
func (f *fakeCM) ListSnapshots(ctx context.Context, params map[string]string) (json.RawMessage, error) {
	if f.listSnapshots == nil {
		return nil, errFakeNotConfigured
	}
	return f.listSnapshots(ctx, params)
}
func (f *fakeCM) DeleteSnapshot(ctx context.Context, snapshotID string) (json.RawMessage, error) {
	if f.deleteSnapshot == nil {
		return nil, errFakeNotConfigured
	}
	return f.deleteSnapshot(ctx, snapshotID)
}
func (f *fakeCM) RollbackSandbox(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
	if f.rollbackSandbox == nil {
		return nil, errFakeNotConfigured
	}
	return f.rollbackSandbox(ctx, sandboxID, body)
}
func (f *fakeCM) ListTemplates(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error) {
	if f.listTemplates == nil {
		return nil, errFakeNotConfigured
	}
	return f.listTemplates(ctx, templateID, includeRequest)
}
func (f *fakeCM) CreateTemplateFromImage(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.createTemplateFromImage == nil {
		return nil, errFakeNotConfigured
	}
	return f.createTemplateFromImage(ctx, body)
}
func (f *fakeCM) RedoTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.redoTemplate == nil {
		return nil, errFakeNotConfigured
	}
	return f.redoTemplate(ctx, body)
}
func (f *fakeCM) DeleteTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.deleteTemplate == nil {
		return nil, errFakeNotConfigured
	}
	return f.deleteTemplate(ctx, body)
}
func (f *fakeCM) GetTemplateBuildStatus(ctx context.Context, buildID string) (json.RawMessage, error) {
	if f.getTemplateBuildStatus == nil {
		return nil, errFakeNotConfigured
	}
	return f.getTemplateBuildStatus(ctx, buildID)
}
func (f *fakeCM) StartTemplateBuild(ctx context.Context, buildID string, body interface{}) (json.RawMessage, error) {
	if f.startTemplateBuild == nil {
		return nil, errFakeNotConfigured
	}
	return f.startTemplateBuild(ctx, buildID, body)
}
func (f *fakeCM) GetTemplateCompat(ctx context.Context) (json.RawMessage, error) {
	if f.getTemplateCompat == nil {
		return nil, errFakeNotConfigured
	}
	return f.getTemplateCompat(ctx)
}
func (f *fakeCM) AdoptTemplateCompatBaseline(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.adoptTemplateCompatBaseline == nil {
		return nil, errFakeNotConfigured
	}
	return f.adoptTemplateCompatBaseline(ctx, body)
}

// errFakeNotConfigured is returned by a fakeCM method that the test didn't
// set up. It makes "I forgot to mock X" failures obvious.
var errFakeNotConfigured = &fakeErr{"fake CubeMasterClient: method not configured for this test"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

// --- helpers shared by all handler tests ---

// newSDKRouter builds a gin engine with the SDK routes mounted at /api/v1/sdk
// using the supplied fake client. It is the smallest setup that exercises the
// real gin routing + middleware + handler code path.
func newSDKRouter(t *testing.T, cm CubeMasterClient) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := NewSDKHandler(cm)
	g := r.Group("/api/v1/sdk")
	h.Register(g)
	return r
}

// doJSON issues a request and returns the status code + decoded JSON body.
func doJSON(t *testing.T, r *gin.Engine, method, path string, body string) (int, map[string]interface{}) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]interface{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// raw wraps a JSON string in json.RawMessage for fakes that return canned data.
func raw(s string) json.RawMessage { return json.RawMessage(s) }

// httptestRecorder issues a request and returns the response recorder. For
// GET / DELETE / no-body requests; for POST with JSON body use doJSON.
func httptestRecorder(t *testing.T, r *gin.Engine, method, path string, body ...string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if len(body) > 0 && body[0] != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body[0]))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// contains is a tiny strings.Contains wrapper to avoid importing strings in tests.
func contains(s, sub string) bool { return len(s) > 0 && len(sub) > 0 && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
