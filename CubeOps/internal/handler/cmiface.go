// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
)

// CubeMasterClient is the subset of *cubemaster.Client used by the handler
// package. Defined here so tests can substitute a fake; the real
// *cubemaster.Client satisfies it implicitly.
type CubeMasterClient interface {
	GetNodes(ctx context.Context) (json.RawMessage, error)
	ClusterVersions(ctx context.Context) (json.RawMessage, error)
	GetNode(ctx context.Context, nodeID string) (json.RawMessage, error)
	ListSandboxes(ctx context.Context) (json.RawMessage, error)

	GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error)
	CreateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error)
	DeleteSandbox(ctx context.Context, body interface{}) (json.RawMessage, error)
	UpdateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error)
	ConnectSandboxWithBody(ctx context.Context, body interface{}) (json.RawMessage, error)
	SetSandboxTimeout(ctx context.Context, body interface{}) (json.RawMessage, error)
	RefreshSandbox(ctx context.Context, body interface{}) (json.RawMessage, error)
	GetSandboxLogs(ctx context.Context, body interface{}) (json.RawMessage, error)
	ListSandboxesWithBody(ctx context.Context, body interface{}) (json.RawMessage, error)

	CreateSnapshot(ctx context.Context, body interface{}) (json.RawMessage, error)
	ListSnapshots(ctx context.Context, params map[string]string) (json.RawMessage, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) (json.RawMessage, error)
	RollbackSandbox(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error)

	ListTemplates(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error)
	CreateTemplateFromImage(ctx context.Context, body interface{}) (json.RawMessage, error)
	RedoTemplate(ctx context.Context, body interface{}) (json.RawMessage, error)
	DeleteTemplate(ctx context.Context, body interface{}) (json.RawMessage, error)
	GetTemplateBuildStatus(ctx context.Context, buildID string) (json.RawMessage, error)
	StartTemplateBuild(ctx context.Context, buildID string, body interface{}) (json.RawMessage, error)
	GetTemplateCompat(ctx context.Context) (json.RawMessage, error)
	AdoptTemplateCompatBaseline(ctx context.Context, body interface{}) (json.RawMessage, error)
}

// Compile-time assertion: *cubemaster.Client satisfies CubeMasterClient.
var _ CubeMasterClient = (*cubemaster.Client)(nil)
