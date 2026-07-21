// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
)

// fakeCMHandler is a stub CubeMasterClient for integration tests. It's in the
// handler_test package (not handler) so it can be used alongside the real
// *store.Store. Only methods that agenthub integration tests touch are
// implemented; the rest return an error.
type fakeCMHandler struct {
	createSandbox   func(ctx context.Context, body interface{}) (json.RawMessage, error)
	deleteSandbox   func(ctx context.Context, body interface{}) (json.RawMessage, error)
	updateSandbox   func(ctx context.Context, body interface{}) (json.RawMessage, error)
	createSnapshot  func(ctx context.Context, body interface{}) (json.RawMessage, error)
	deleteSnapshot  func(ctx context.Context, snapshotID string) (json.RawMessage, error)
	rollbackSandbox func(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error)
	listTemplates   func(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error)
}

func (f *fakeCMHandler) GetNodes(ctx context.Context) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("GetNodes")
}
func (f *fakeCMHandler) ClusterVersions(ctx context.Context) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("ClusterVersions")
}
func (f *fakeCMHandler) GetNode(ctx context.Context, nodeID string) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("GetNode")
}
func (f *fakeCMHandler) ListSandboxes(ctx context.Context) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("ListSandboxes")
}
func (f *fakeCMHandler) GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("GetSandbox")
}
func (f *fakeCMHandler) CreateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.createSandbox == nil {
		return raw(`{"ret":{"ret_code":0},"data":{"sandbox_id":"sb-fake"}}`), nil
	}
	return f.createSandbox(ctx, body)
}
func (f *fakeCMHandler) DeleteSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.deleteSandbox == nil {
		return raw(`{"ret": {"ret_code": 0}}`), nil // default: success
	}
	return f.deleteSandbox(ctx, body)
}
func (f *fakeCMHandler) UpdateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.updateSandbox == nil {
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}
	return f.updateSandbox(ctx, body)
}
func (f *fakeCMHandler) ConnectSandboxWithBody(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("ConnectSandboxWithBody")
}
func (f *fakeCMHandler) SetSandboxTimeout(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("SetSandboxTimeout")
}
func (f *fakeCMHandler) RefreshSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("RefreshSandbox")
}
func (f *fakeCMHandler) GetSandboxLogs(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("GetSandboxLogs")
}
func (f *fakeCMHandler) ListSandboxesWithBody(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("ListSandboxesWithBody")
}
func (f *fakeCMHandler) CreateSnapshot(ctx context.Context, body interface{}) (json.RawMessage, error) {
	if f.createSnapshot == nil {
		return nil, errMethodNotConfigured("CreateSnapshot")
	}
	return f.createSnapshot(ctx, body)
}
func (f *fakeCMHandler) ListSnapshots(ctx context.Context, params map[string]string) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("ListSnapshots")
}
func (f *fakeCMHandler) DeleteSnapshot(ctx context.Context, snapshotID string) (json.RawMessage, error) {
	if f.deleteSnapshot == nil {
		return raw(`{"ret": {"ret_code": 0}}`), nil // default: success
	}
	return f.deleteSnapshot(ctx, snapshotID)
}
func (f *fakeCMHandler) RollbackSandbox(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
	if f.rollbackSandbox == nil {
		return nil, errMethodNotConfigured("RollbackSandbox")
	}
	return f.rollbackSandbox(ctx, sandboxID, body)
}
func (f *fakeCMHandler) ListTemplates(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error) {
	if f.listTemplates == nil {
		return nil, errMethodNotConfigured("ListTemplates")
	}
	return f.listTemplates(ctx, templateID, includeRequest)
}
func (f *fakeCMHandler) CreateTemplateFromImage(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("CreateTemplateFromImage")
}
func (f *fakeCMHandler) RedoTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("RedoTemplate")
}
func (f *fakeCMHandler) DeleteTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("DeleteTemplate")
}
func (f *fakeCMHandler) GetTemplateBuildStatus(ctx context.Context, buildID string) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("GetTemplateBuildStatus")
}
func (f *fakeCMHandler) StartTemplateBuild(ctx context.Context, buildID string, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("StartTemplateBuild")
}
func (f *fakeCMHandler) GetTemplateCompat(ctx context.Context) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("GetTemplateCompat")
}
func (f *fakeCMHandler) AdoptTemplateCompatBaseline(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return nil, errMethodNotConfigured("AdoptTemplateCompatBaseline")
}

// Compile-time assertion.
var _ handler.CubeMasterClient = (*fakeCMHandler)(nil)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

type fakeCMErr struct{ method string }

func (e *fakeCMErr) Error() string {
	return fmt.Sprintf("fake CubeMasterClient: %s not configured for this test", e.method)
}

func errMethodNotConfigured(method string) error { return &fakeCMErr{method: method} }

// dockertestOpenMySQL opens a *sql.DB connection to the MySQL container on
// the given port. Used by the pool.Retry health check.
func dockertestOpenMySQL(port string) (*sql.DB, error) {
	dsn := fmt.Sprintf("root:root@tcp(127.0.0.1:%s)/cubeops_test?charset=utf8&parseTime=true", port)
	return sql.Open("mysql", dsn)
}
