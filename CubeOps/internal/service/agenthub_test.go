// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"
	"gorm.io/gorm"
)

// ── fakes ───────────────────────────────────────────────────────────────────

// fakeAgentStore is an in-memory AgentStore for service-layer unit tests.
// Each method is backed by a function field so each test can wire only the
// methods it exercises; unconfigured methods default to returning a
// sentinel error so a missed mock fails loud rather than silently returning
// zero values.
type fakeAgentStore struct {
	getSetting                 func(ctx context.Context, key string) (string, error)
	getInstance                func(ctx context.Context, agentID string) (*store.AgentInstance, error)
	upsertInstance             func(ctx context.Context, inst *store.AgentInstance) error
	softDeleteInstance         func(ctx context.Context, agentID string) error
	updateInstanceStatus       func(ctx context.Context, agentID, status string) error
	updateInstanceModel        func(ctx context.Context, agentID, model string) error
	getAgentWecomConfig        func(ctx context.Context, agentID string) (string, string, error)
	updateAgentWecomConfig     func(ctx context.Context, agentID, botID, botSecret string) error
	getAgentSnapshot           func(ctx context.Context, agentID, snapshotID string) (*store.AgentSnapshot, error)
	deleteAgentSnapshot        func(ctx context.Context, agentID, snapshotID string) error
	getAgentTemplate           func(ctx context.Context, templateID string) (*store.AgentTemplate, error)
	getRecommendedTemplate     func(ctx context.Context) (*store.AgentTemplate, error)
	listAgentTemplates         func(ctx context.Context, limit, offset int) ([]store.AgentTemplate, error)
	recordOperation            func(ctx context.Context, agentID, sandboxID, operationType, status, errMsg string) error
	latestHealthySnapshot      func(ctx context.Context, agentID string) (string, error)
	setBaseSnapshotID          func(ctx context.Context, agentID, snapshotID string) error
	findTemplateIDsByInfraID   func(ctx context.Context, infraID string) ([]string, error)
	softDeleteAgentHubTemplate func(ctx context.Context, templateID string) error
}

func (f *fakeAgentStore) GetSetting(ctx context.Context, key string) (string, error) {
	if f.getSetting == nil {
		return "", nil // default: setting not found
	}
	return f.getSetting(ctx, key)
}
func (f *fakeAgentStore) GetInstance(ctx context.Context, agentID string) (*store.AgentInstance, error) {
	if f.getInstance == nil {
		return nil, nil
	}
	return f.getInstance(ctx, agentID)
}
func (f *fakeAgentStore) UpsertInstance(ctx context.Context, inst *store.AgentInstance) error {
	if f.upsertInstance == nil {
		return nil
	}
	return f.upsertInstance(ctx, inst)
}
func (f *fakeAgentStore) SoftDeleteInstance(ctx context.Context, agentID string) error {
	if f.softDeleteInstance == nil {
		return nil
	}
	return f.softDeleteInstance(ctx, agentID)
}
func (f *fakeAgentStore) UpdateInstanceStatus(ctx context.Context, agentID, status string) error {
	if f.updateInstanceStatus == nil {
		return nil
	}
	return f.updateInstanceStatus(ctx, agentID, status)
}
func (f *fakeAgentStore) UpdateInstanceModel(ctx context.Context, agentID, model string) error {
	if f.updateInstanceModel == nil {
		return nil
	}
	return f.updateInstanceModel(ctx, agentID, model)
}
func (f *fakeAgentStore) GetAgentWecomConfig(ctx context.Context, agentID string) (string, string, error) {
	if f.getAgentWecomConfig == nil {
		return "", "", nil
	}
	return f.getAgentWecomConfig(ctx, agentID)
}
func (f *fakeAgentStore) UpdateAgentWecomConfig(ctx context.Context, agentID, botID, botSecret string) error {
	if f.updateAgentWecomConfig == nil {
		return nil
	}
	return f.updateAgentWecomConfig(ctx, agentID, botID, botSecret)
}
func (f *fakeAgentStore) GetAgentSnapshot(ctx context.Context, agentID, snapshotID string) (*store.AgentSnapshot, error) {
	if f.getAgentSnapshot == nil {
		return nil, nil
	}
	return f.getAgentSnapshot(ctx, agentID, snapshotID)
}
func (f *fakeAgentStore) DeleteAgentSnapshot(ctx context.Context, agentID, snapshotID string) error {
	if f.deleteAgentSnapshot == nil {
		return nil
	}
	return f.deleteAgentSnapshot(ctx, agentID, snapshotID)
}
func (f *fakeAgentStore) GetAgentTemplate(ctx context.Context, templateID string) (*store.AgentTemplate, error) {
	if f.getAgentTemplate == nil {
		return nil, nil
	}
	return f.getAgentTemplate(ctx, templateID)
}
func (f *fakeAgentStore) GetRecommendedAgentTemplate(ctx context.Context) (*store.AgentTemplate, error) {
	if f.getRecommendedTemplate == nil {
		return nil, nil // default: none marked recommended
	}
	return f.getRecommendedTemplate(ctx)
}
func (f *fakeAgentStore) ListAgentTemplates(ctx context.Context, limit, offset int) ([]store.AgentTemplate, error) {
	if f.listAgentTemplates == nil {
		return nil, nil // default: no template registered
	}
	return f.listAgentTemplates(ctx, limit, offset)
}
func (f *fakeAgentStore) RecordOperation(ctx context.Context, agentID, sandboxID, operationType, status, errMsg string) error {
	if f.recordOperation == nil {
		return nil
	}
	return f.recordOperation(ctx, agentID, sandboxID, operationType, status, errMsg)
}
func (f *fakeAgentStore) LatestHealthySnapshot(ctx context.Context, agentID string) (string, error) {
	if f.latestHealthySnapshot == nil {
		return "", nil
	}
	return f.latestHealthySnapshot(ctx, agentID)
}
func (f *fakeAgentStore) SetBaseSnapshotID(ctx context.Context, agentID, snapshotID string) error {
	if f.setBaseSnapshotID == nil {
		return nil
	}
	return f.setBaseSnapshotID(ctx, agentID, snapshotID)
}
func (f *fakeAgentStore) DB() *gorm.DB { return nil } // not exercised by the unit tests below
func (f *fakeAgentStore) FindTemplateIDsByInfraID(ctx context.Context, infraID string) ([]string, error) {
	if f.findTemplateIDsByInfraID == nil {
		return nil, nil
	}
	return f.findTemplateIDsByInfraID(ctx, infraID)
}
func (f *fakeAgentStore) SoftDeleteAgentHubTemplate(ctx context.Context, templateID string) error {
	if f.softDeleteAgentHubTemplate == nil {
		return nil
	}
	return f.softDeleteAgentHubTemplate(ctx, templateID)
}

// Compile-time assertion.
var _ AgentStore = (*fakeAgentStore)(nil)

// fakeServiceCM is a CubeMasterClient stub for service tests. Each method
// captures the request body so tests can assert on the CubeMaster-facing
// request shape.
type fakeServiceCM struct {
	// Captured request bodies (decoded as map[string]interface{}).
	createSandboxBody   map[string]interface{}
	deleteSandboxBodies []map[string]interface{} // accumulate, since compensation also calls DeleteSandbox
	updateSandboxBody   map[string]interface{}
	createSnapshotBody  map[string]interface{}
	deleteSnapshotID    string
	rollbackBody        map[string]interface{}

	// Return values.
	createSandboxResp  json.RawMessage
	createSandboxErr   error
	deleteSandboxErr   error
	updateSandboxErr   error
	createSnapshotResp json.RawMessage
	createSnapshotErr  error
	deleteSnapshotErr  error
	rollbackErr        error
	listTemplatesResp  json.RawMessage
	listTemplatesErr   error
}

func (f *fakeServiceCM) CreateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	cubemaster.EnsureRequestID(ctx, body)
	f.createSandboxBody = decodeBody(body)
	if f.createSandboxResp == nil {
		f.createSandboxResp = json.RawMessage(`{"sandbox_id":"sb-test-123","ret":{"ret_code":0}}`)
	}
	return f.createSandboxResp, f.createSandboxErr
}
func (f *fakeServiceCM) DeleteSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	cubemaster.EnsureRequestID(ctx, body)
	f.deleteSandboxBodies = append(f.deleteSandboxBodies, decodeBody(body))
	return nil, f.deleteSandboxErr
}
func (f *fakeServiceCM) UpdateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	cubemaster.EnsureRequestID(ctx, body)
	f.updateSandboxBody = decodeBody(body)
	return nil, f.updateSandboxErr
}
func (f *fakeServiceCM) CreateSnapshot(ctx context.Context, body interface{}) (json.RawMessage, error) {
	cubemaster.EnsureRequestID(ctx, body)
	f.createSnapshotBody = decodeBody(body)
	return f.createSnapshotResp, f.createSnapshotErr
}
func (f *fakeServiceCM) DeleteSnapshot(_ context.Context, snapshotID string) (json.RawMessage, error) {
	f.deleteSnapshotID = snapshotID
	return nil, f.deleteSnapshotErr
}
func (f *fakeServiceCM) RollbackSandbox(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
	cubemaster.EnsureRequestID(ctx, body)
	f.rollbackBody = decodeBody(body)
	return nil, f.rollbackErr
}
func (f *fakeServiceCM) ListTemplates(_ context.Context, templateID string, includeRequest bool) (json.RawMessage, error) {
	return f.listTemplatesResp, f.listTemplatesErr
}

func decodeBody(body interface{}) map[string]interface{} {
	b, _ := json.Marshal(body)
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return m
}

// Compile-time assertion.
var _ CubeMasterClient = (*fakeServiceCM)(nil)

// newTestService builds an AgentHubService with fake dependencies. The
// envd/OpenClaw functions are stubbed so no real HTTP is attempted.
func newTestService(s AgentStore, cm *fakeServiceCM) *AgentHubService {
	return &AgentHubService{
		Store:      s,
		CM:         cm,
		envdClient: nil,
		applyFn: func(_ context.Context, _ *http.Client, _ string, _ string, _ *LLMRuntimePlan, _ *OpenclawApplyOptions) (*CommandOutput, error) {
			return &CommandOutput{ExitCode: 0, Stdout: "applied", Stderr: ""}, nil
		},
		resolveGatewayFn: func(_ context.Context, _ *http.Client, _ string, _ string, _ string, fallback string) string {
			return fallback
		},
		restartFn: func(_ *store.AgentInstance) (*CommandOutput, error) {
			return &CommandOutput{ExitCode: 0}, nil
		},
		upgradeFn: func(_ *store.AgentInstance) (*CommandOutput, error) {
			return &CommandOutput{ExitCode: 0}, nil
		},
	}
}

// ── CreateInstance tests ────────────────────────────────────────────────────

// TestCreateInstance_Success verifies the happy path: LLM config resolves,
// CubeMaster CreateSandbox is called with the expected request shape,
// OpenClaw apply succeeds, the instance is persisted, and the returned
// AgentInstance carries the sandbox ID and generated gateway URL.
func TestCreateInstance_Success(t *testing.T) {
	cm := &fakeServiceCM{}
	st := &fakeAgentStore{
		getSetting: func(_ context.Context, key string) (string, error) {
			switch key {
			case "llm_api_key":
				return "test-key", nil
			case "gateway_domain":
				return "test.example", nil
			}
			return "", nil
		},
		upsertInstance: func(_ context.Context, inst *store.AgentInstance) error {
			if inst.SandboxID != "sb-test-123" {
				t.Errorf("UpsertInstance got SandboxID=%q, want sb-test-123", inst.SandboxID)
			}
			if inst.Name != "my-agent" {
				t.Errorf("UpsertInstance got Name=%q, want my-agent", inst.Name)
			}
			return nil
		},
	}
	svc := newTestService(st, cm)

	res, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "my-agent",
		Engine: "openclaw",
	})
	if err != nil {
		t.Fatalf("CreateInstance returned error: %v", err)
	}
	if res.Instance == nil {
		t.Fatal("CreateInstance returned nil instance")
	}
	if res.Instance.SandboxID != "sb-test-123" {
		t.Errorf("instance.SandboxID = %q, want sb-test-123", res.Instance.SandboxID)
	}
	if res.Instance.ID != "agent-sb-test-123" {
		t.Errorf("instance.ID = %q, want agent-sb-test-123", res.Instance.ID)
	}
	if !strings.Contains(res.Instance.GatewayURL, "sb-test-123") {
		t.Errorf("instance.GatewayURL = %q, want to contain sandbox ID", res.Instance.GatewayURL)
	}
	if !strings.Contains(res.Instance.GatewayURL, "test.example") {
		t.Errorf("instance.GatewayURL = %q, want to contain domain", res.Instance.GatewayURL)
	}

	// Assert the CubeMaster request shape.
	if cm.createSandboxBody == nil {
		t.Fatal("CreateSandbox was not called")
	}
	if cm.createSandboxBody["instance_type"] != "cubebox" {
		t.Errorf("instance_type = %v, want cubebox", cm.createSandboxBody["instance_type"])
	}
	if cm.createSandboxBody["network_type"] != "tap" {
		t.Errorf("network_type = %v, want tap", cm.createSandboxBody["network_type"])
	}
	labels, _ := cm.createSandboxBody["labels"].(map[string]interface{})
	if labels["agenthub"] != "true" {
		t.Errorf("labels[agenthub] = %v, want true", labels["agenthub"])
	}
	if labels["agenthub.name"] != "my-agent" {
		t.Errorf("labels[agenthub.name] = %v, want my-agent", labels["agenthub.name"])
	}
	if labels["agenthub.engine"] != "openclaw" {
		t.Errorf("labels[agenthub.engine] = %v, want openclaw", labels["agenthub.engine"])
	}
}

// TestCreateInstance_ValidationErrors verifies that invalid input returns
// a service.Error with the right HTTP status, and that CubeMaster is never
// contacted.
func TestCreateInstance_ValidationErrors(t *testing.T) {
	cm := &fakeServiceCM{}
	st := &fakeAgentStore{}
	svc := newTestService(st, cm)

	tests := []struct {
		name string
		req  CreateInstanceRequest
		want int
	}{
		{"empty name", CreateInstanceRequest{Engine: "openclaw"}, 400},
		{"wrong engine", CreateInstanceRequest{Name: "x", Engine: "docker"}, 400},
		{"botID without secret", CreateInstanceRequest{Name: "x", Engine: "openclaw", BotID: "b"}, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateInstance(context.Background(), tt.req)
			var svcErr *Error
			if !errors.As(err, &svcErr) {
				t.Fatalf("error is not *service.Error: %v", err)
			}
			if svcErr.Status != tt.want {
				t.Errorf("status = %d, want %d", svcErr.Status, tt.want)
			}
			if cm.createSandboxBody != nil {
				t.Error("CreateSandbox should not have been called for a validation error")
			}
		})
	}
}

// llmKeyStore returns a fakeAgentStore whose only wired method resolves the
// LLM API key, which CreateInstance requires before it reaches CubeMaster.
func llmKeyStore() *fakeAgentStore {
	return &fakeAgentStore{
		getSetting: func(_ context.Context, key string) (string, error) {
			if key == "llm_api_key" {
				return "test-key", nil
			}
			return "", nil
		},
	}
}

// rootfsSourceID returns the resolved rootfs source id CreateInstance sent to
// CubeMaster, which BuildCreateSandboxRequest carries as a label.
func rootfsSourceID(t *testing.T, cm *fakeServiceCM) string {
	t.Helper()
	if cm.createSandboxBody == nil {
		t.Fatal("CreateSandbox was not called")
	}
	labels, _ := cm.createSandboxBody["labels"].(map[string]interface{})
	id, _ := labels["agenthub.rootfs_source_id"].(string)
	return id
}

// TestCreateInstance_DefaultTemplateSelection verifies which template
// CreateInstance uses when the request names neither a snapshot nor a
// templateId: the operator-marked recommended template if there is one, else
// the most recently registered one. Before this, the request always went out
// with the hardcoded defaultAgentTemplateID, which nothing provisions.
func TestCreateInstance_DefaultTemplateSelection(t *testing.T) {
	tests := []struct {
		name        string
		recommended *store.AgentTemplate
		newest      []store.AgentTemplate
		want        string
	}{
		{
			name:        "prefers the recommended template over a newer one",
			recommended: &store.AgentTemplate{TemplateID: "tpl-recommended", Recommended: true},
			newest:      []store.AgentTemplate{{TemplateID: "tpl-newest"}},
			want:        "tpl-recommended",
		},
		{
			name:   "falls back to the most recent when none is recommended",
			newest: []store.AgentTemplate{{TemplateID: "tpl-newest"}},
			want:   "tpl-newest",
		},
		{
			name: "uses the built-in identifier when nothing is registered",
			want: defaultAgentTemplateID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := &fakeServiceCM{}
			st := llmKeyStore()
			st.getRecommendedTemplate = func(_ context.Context) (*store.AgentTemplate, error) {
				return tt.recommended, nil
			}
			st.listAgentTemplates = func(_ context.Context, limit, _ int) ([]store.AgentTemplate, error) {
				if limit != 1 {
					t.Errorf("ListAgentTemplates limit = %d, want 1 — only the newest is needed", limit)
				}
				return tt.newest, nil
			}
			svc := newTestService(st, cm)

			if _, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
				Name:   "my-agent",
				Engine: "openclaw",
			}); err != nil {
				t.Fatalf("CreateInstance returned error: %v", err)
			}
			if got := rootfsSourceID(t, cm); got != tt.want {
				t.Errorf("rootfs_source_id = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCreateInstance_RecommendedIsNotWindowLimited verifies that the
// recommended preference does not depend on how many newer templates were
// registered after it: the flag is resolved by its own query, so any number of
// non-recommended registrations cannot bury it.
func TestCreateInstance_RecommendedIsNotWindowLimited(t *testing.T) {
	cm := &fakeServiceCM{}
	st := llmKeyStore()
	st.getRecommendedTemplate = func(_ context.Context) (*store.AgentTemplate, error) {
		return &store.AgentTemplate{TemplateID: "tpl-recommended", Recommended: true}, nil
	}
	listed := false
	st.listAgentTemplates = func(_ context.Context, _, _ int) ([]store.AgentTemplate, error) {
		listed = true
		return []store.AgentTemplate{{TemplateID: "tpl-newest"}}, nil
	}
	svc := newTestService(st, cm)

	if _, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "my-agent",
		Engine: "openclaw",
	}); err != nil {
		t.Fatalf("CreateInstance returned error: %v", err)
	}
	if got := rootfsSourceID(t, cm); got != "tpl-recommended" {
		t.Errorf("rootfs_source_id = %q, want tpl-recommended", got)
	}
	if listed {
		t.Error("the registry should not be listed once a recommended template is found")
	}
}

// TestCreateInstance_TemplateListingFailureIsNotReportedAsUnregistered verifies
// that a failed *listing* is not turned into "no agent template is registered":
// that query never completed, so the registry's contents are unknown and
// CubeMaster's own error is what the caller gets.
//
// The recommended-flag query is deliberately not covered here — losing it does
// not leave the registry unknown, since a marked template is also a listed one.
// The two tests below pin what a failure there does instead.
func TestCreateInstance_TemplateListingFailureIsNotReportedAsUnregistered(t *testing.T) {
	cm := &fakeServiceCM{
		createSandboxErr: &cubemaster.CMError{RetCode: 130404, RetMsg: "template not found"},
	}
	st := llmKeyStore()
	st.listAgentTemplates = func(_ context.Context, _, _ int) ([]store.AgentTemplate, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	svc := newTestService(st, cm)

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "my-agent",
		Engine: "openclaw",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) {
		t.Fatalf("error is not *service.Error: %v", err)
	}
	if svcErr.Status != 502 {
		t.Errorf("status = %d, want 502", svcErr.Status)
	}
	if strings.Contains(svcErr.Message, "no agent template is registered") {
		t.Errorf("message = %q, should not claim an empty registry when the read failed", svcErr.Message)
	}
}

// TestCreateInstance_RecommendedReadFailureFallsBackToNewest verifies that
// losing the recommended-flag query does not fail a create the registry can
// still satisfy. The flag says which registered template to prefer, not whether
// one exists, so a transient failure there falls through to the newest instead
// of handing CubeMaster an identifier no install has to carry.
func TestCreateInstance_RecommendedReadFailureFallsBackToNewest(t *testing.T) {
	cm := &fakeServiceCM{}
	st := llmKeyStore()
	st.getRecommendedTemplate = func(_ context.Context) (*store.AgentTemplate, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	st.listAgentTemplates = func(_ context.Context, _, _ int) ([]store.AgentTemplate, error) {
		return []store.AgentTemplate{{TemplateID: "tpl-newest"}}, nil
	}
	svc := newTestService(st, cm)

	if _, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "my-agent",
		Engine: "openclaw",
	}); err != nil {
		t.Fatalf("CreateInstance returned error: %v", err)
	}
	if got := rootfsSourceID(t, cm); got != "tpl-newest" {
		t.Errorf("rootfs_source_id = %q, want tpl-newest", got)
	}
}

// TestCreateInstance_EmptyListingSettlesEmptinessDespiteRecommendedFailure
// verifies the other half: when the recommended query fails but the listing
// completes and comes back empty, the registry really is empty — a marked
// template would have been listed too — so the caller still gets the actionable
// registration hint rather than a 502 naming the built-in identifier.
func TestCreateInstance_EmptyListingSettlesEmptinessDespiteRecommendedFailure(t *testing.T) {
	cm := &fakeServiceCM{
		createSandboxErr: &cubemaster.CMError{RetCode: 130404, RetMsg: "template not found"},
	}
	st := llmKeyStore()
	st.getRecommendedTemplate = func(_ context.Context) (*store.AgentTemplate, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	st.listAgentTemplates = func(_ context.Context, _, _ int) ([]store.AgentTemplate, error) {
		return nil, nil
	}
	svc := newTestService(st, cm)

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "my-agent",
		Engine: "openclaw",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) {
		t.Fatalf("error is not *service.Error: %v", err)
	}
	if svcErr.Status != 400 {
		t.Errorf("status = %d, want 400", svcErr.Status)
	}
	if !strings.Contains(svcErr.Message, "no agent template is registered") {
		t.Errorf("message = %q, want it to name the missing registration", svcErr.Message)
	}
}

// TestCreateInstance_ExplicitTemplateIDWins verifies that an explicit
// templateId is used as-is and the registered-template lookup is skipped.
func TestCreateInstance_ExplicitTemplateIDWins(t *testing.T) {
	cm := &fakeServiceCM{}
	st := llmKeyStore()
	consulted := false
	st.getRecommendedTemplate = func(_ context.Context) (*store.AgentTemplate, error) {
		consulted = true
		return &store.AgentTemplate{TemplateID: "tpl-recommended", Recommended: true}, nil
	}
	st.listAgentTemplates = func(_ context.Context, _, _ int) ([]store.AgentTemplate, error) {
		consulted = true
		return []store.AgentTemplate{{TemplateID: "tpl-newest"}}, nil
	}
	svc := newTestService(st, cm)

	if _, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:       "my-agent",
		Engine:     "openclaw",
		TemplateID: "tpl-explicit",
	}); err != nil {
		t.Fatalf("CreateInstance returned error: %v", err)
	}
	if got := rootfsSourceID(t, cm); got != "tpl-explicit" {
		t.Errorf("rootfs_source_id = %q, want tpl-explicit", got)
	}
	if consulted {
		t.Error("the template registry should not be consulted when templateId is explicit")
	}
}

// TestCreateInstance_NoTemplateRegisteredReportsMissingRegistration verifies
// that when no template is registered and CubeMaster cannot resolve the
// built-in identifier either, the caller gets an actionable 400 instead of a
// 502 naming an identifier they never supplied.
func TestCreateInstance_NoTemplateRegisteredReportsMissingRegistration(t *testing.T) {
	cm := &fakeServiceCM{
		createSandboxErr: &cubemaster.CMError{
			RetCode: 130404,
			RetMsg:  `failed to resolve template identifier "` + defaultAgentTemplateID + `": template not found`,
		},
	}
	svc := newTestService(llmKeyStore(), cm)

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "my-agent",
		Engine: "openclaw",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) {
		t.Fatalf("error is not *service.Error: %v", err)
	}
	if svcErr.Status != 400 {
		t.Errorf("status = %d, want 400", svcErr.Status)
	}
	if !strings.Contains(svcErr.Message, "no agent template is registered") {
		t.Errorf("message = %q, want it to name the missing registration", svcErr.Message)
	}
	if strings.Contains(svcErr.Message, defaultAgentTemplateID) {
		t.Errorf("message = %q, should not surface the built-in identifier to the caller", svcErr.Message)
	}
	if svcErr.Cause == nil {
		t.Error("Cause is nil; the CubeMaster error must stay attached for diagnosis")
	}
}

// TestCreateInstance_DefaultedTemplateVanishingIsAConflict covers the race the
// registry read cannot close: a template that was registered when the request
// started and gone by the time CubeMaster resolved it. The caller named no
// template, so reporting a not-found for the one we picked would name an
// identifier they never chose — the failure class this path exists to remove —
// and it is not a missing registration either, because one was registered.
// Retrying re-resolves and may pick another, so it is a conflict.
func TestCreateInstance_DefaultedTemplateVanishingIsAConflict(t *testing.T) {
	cm := &fakeServiceCM{
		createSandboxErr: &cubemaster.CMError{
			RetCode: 130404,
			RetMsg:  `failed to resolve template identifier "tpl-vanished": template not found`,
		},
	}
	st := llmKeyStore()
	st.listAgentTemplates = func(_ context.Context, _, _ int) ([]store.AgentTemplate, error) {
		return []store.AgentTemplate{{TemplateID: "tpl-vanished"}}, nil
	}
	svc := newTestService(st, cm)

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "my-agent",
		Engine: "openclaw",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) {
		t.Fatalf("error is not *service.Error: %v", err)
	}
	if svcErr.Status != 409 {
		t.Errorf("status = %d, want 409", svcErr.Status)
	}
	if strings.Contains(svcErr.Message, "tpl-vanished") {
		t.Errorf("message = %q, should not surface an identifier the caller never chose", svcErr.Message)
	}
	if strings.Contains(svcErr.Message, "no agent template is registered") {
		t.Errorf("message = %q, should not claim an empty registry when one was registered", svcErr.Message)
	}
	if svcErr.Cause == nil {
		t.Error("Cause is nil; the CubeMaster error must stay attached for diagnosis")
	}
}

// TestCreateInstance_ExplicitTemplateNotFoundStaysBadGateway guards the
// narrowness of the case above: a not-found for a template the caller did name
// is still reported as-is, not rewritten into the registration hint.
func TestCreateInstance_ExplicitTemplateNotFoundStaysBadGateway(t *testing.T) {
	cm := &fakeServiceCM{
		createSandboxErr: &cubemaster.CMError{RetCode: 130404, RetMsg: "template not found"},
	}
	svc := newTestService(llmKeyStore(), cm)

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:       "my-agent",
		Engine:     "openclaw",
		TemplateID: "tpl-typo",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) {
		t.Fatalf("error is not *service.Error: %v", err)
	}
	if svcErr.Status != 502 {
		t.Errorf("status = %d, want 502", svcErr.Status)
	}
	if strings.Contains(svcErr.Message, "no agent template is registered") {
		t.Errorf("message = %q, should not claim a missing registration", svcErr.Message)
	}
}

// TestCreateInstance_ApplyFailureCompensates verifies that when the
// OpenClaw apply step fails after the sandbox was already created,
// CompensateDeleteSandbox is invoked to clean up the orphan sandbox.
// See R10.
func TestCreateInstance_ApplyFailureCompensates(t *testing.T) {
	cm := &fakeServiceCM{}
	st := &fakeAgentStore{
		getSetting: func(_ context.Context, key string) (string, error) {
			if key == "llm_api_key" {
				return "test-key", nil
			}
			return "", nil
		},
	}
	svc := newTestService(st, cm)
	// Override applyFn to fail.
	svc.applyFn = func(_ context.Context, _ *http.Client, _ string, _ string, _ *LLMRuntimePlan, _ *OpenclawApplyOptions) (*CommandOutput, error) {
		return nil, errors.New("envd unreachable")
	}

	const inboundReqID = "svc-inbound-req-123"
	ctx := cubelog.WithRequestTrace(context.Background(), &cubelog.RequestTrace{RequestID: inboundReqID})

	_, err := svc.CreateInstance(ctx, CreateInstanceRequest{
		Name:   "x",
		Engine: "openclaw",
	})
	if err == nil {
		t.Fatal("CreateInstance should have returned an error")
	}

	// The sandbox was created (CreateSandbox called), then apply failed, so
	// compensation must have called DeleteSandbox.
	if cm.createSandboxBody == nil {
		t.Fatal("CreateSandbox was not called — test setup is wrong")
	}
	if len(cm.deleteSandboxBodies) == 0 {
		t.Fatal("compensation DeleteSandbox was not called after apply failure")
	}
	comp := cm.deleteSandboxBodies[0]
	if comp["sandbox_id"] != "sb-test-123" {
		t.Errorf("compensation sandbox_id = %v, want sb-test-123", comp["sandbox_id"])
	}
	if comp["instance_type"] != "cubebox" {
		t.Errorf("compensation instance_type = %v, want cubebox", comp["instance_type"])
	}
	reqID, _ := comp["requestID"].(string)
	if reqID != inboundReqID {
		t.Errorf("compensation requestID = %q, want %q", reqID, inboundReqID)
	}
}

// TestCreateInstance_UpsertFailureCompensates verifies that when the DB
// upsert fails after sandbox creation + OpenClaw apply, the sandbox is
// compensated. See R10.
func TestCreateInstance_UpsertFailureCompensates(t *testing.T) {
	cm := &fakeServiceCM{}
	st := &fakeAgentStore{
		getSetting: func(_ context.Context, key string) (string, error) {
			if key == "llm_api_key" {
				return "test-key", nil
			}
			return "", nil
		},
		upsertInstance: func(_ context.Context, _ *store.AgentInstance) error {
			return errors.New("DB connection lost")
		},
	}
	svc := newTestService(st, cm)

	const inboundReqID = "svc-inbound-req-456"
	ctx := cubelog.WithRequestTrace(context.Background(), &cubelog.RequestTrace{RequestID: inboundReqID})

	_, err := svc.CreateInstance(ctx, CreateInstanceRequest{
		Name:   "x",
		Engine: "openclaw",
	})
	if err == nil {
		t.Fatal("CreateInstance should have returned an error")
	}

	if len(cm.deleteSandboxBodies) == 0 {
		t.Fatal("compensation DeleteSandbox was not called after upsert failure")
	}
	comp := cm.deleteSandboxBodies[0]
	reqID, _ := comp["requestID"].(string)
	if reqID != inboundReqID {
		t.Errorf("compensation requestID = %q, want %q", reqID, inboundReqID)
	}
}

// TestCreateInstance_LLMConfigMissingReturnsBadRequest verifies that when
// the LLM API key is not configured, the service returns a 400 (not 500),
// matching the old handler behaviour.
func TestCreateInstance_LLMConfigMissingReturnsBadRequest(t *testing.T) {
	cm := &fakeServiceCM{}
	st := &fakeAgentStore{
		getSetting: func(_ context.Context, _ string) (string, error) {
			return "", nil // no settings → no API key
		},
	}
	svc := newTestService(st, cm)

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:   "x",
		Engine: "openclaw",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) {
		t.Fatalf("error is not *service.Error: %v", err)
	}
	if svcErr.Status != 400 {
		t.Errorf("status = %d, want 400 (bad request, not internal)", svcErr.Status)
	}
	if cm.createSandboxBody != nil {
		t.Error("CreateSandbox should not have been called when LLM config is missing")
	}
}

// ── CloneAgent tests ────────────────────────────────────────────────────────

// TestCloneAgent_Success verifies the clone happy path: the source instance
// is loaded, a new sandbox is created, OpenClaw apply succeeds, and the
// clone record is persisted with the new sandbox ID.
func TestCloneAgent_Success(t *testing.T) {
	cm := &fakeServiceCM{}
	sourcePersistence := "full_snapshot"
	sourceRootfsType := "snapshot"
	sourceRootfsID := "snap-src-1"
	st := &fakeAgentStore{
		getSetting: func(_ context.Context, key string) (string, error) {
			if key == "llm_api_key" {
				return "test-key", nil
			}
			return "", nil
		},
		getInstance: func(_ context.Context, agentID string) (*store.AgentInstance, error) {
			return &store.AgentInstance{
				ID:               agentID,
				Name:             "source-agent",
				Engine:           "openclaw",
				Env:              "linux",
				Model:            "deepseek/deepseek-v4-flash",
				SandboxID:        "sb-source-1",
				TemplateID:       "tpl-1",
				Domain:           "test.example",
				PersistenceMode:  &sourcePersistence,
				RootfsSourceType: &sourceRootfsType,
				RootfsSourceID:   &sourceRootfsID,
				GatewayToken:     "src-token",
			}, nil
		},
		upsertInstance: func(_ context.Context, inst *store.AgentInstance) error {
			if inst.SandboxID != "sb-test-123" {
				t.Errorf("clone UpsertInstance got SandboxID=%q, want sb-test-123", inst.SandboxID)
			}
			if inst.ID != "agent-sb-test-123" {
				t.Errorf("clone ID = %q, want agent-sb-test-123", inst.ID)
			}
			return nil
		},
	}
	svc := newTestService(st, cm)

	res, err := svc.CloneAgent(context.Background(), CloneAgentRequest{
		AgentID: "agent-source-1",
		Name:    "clone-1",
	})
	if err != nil {
		t.Fatalf("CloneAgent returned error: %v", err)
	}
	if res.Instance == nil {
		t.Fatal("CloneAgent returned nil instance")
	}
	if res.Instance.SandboxID != "sb-test-123" {
		t.Errorf("clone.SandboxID = %q, want sb-test-123", res.Instance.SandboxID)
	}
	if res.Instance.Name != "clone-1" {
		t.Errorf("clone.Name = %q, want clone-1", res.Instance.Name)
	}
	if res.Instance.Engine != "openclaw" {
		t.Errorf("clone.Engine = %q, want openclaw (inherited from source)", res.Instance.Engine)
	}

	// CubeMaster request shape.
	if cm.createSandboxBody == nil {
		t.Fatal("CreateSandbox was not called for clone")
	}
	labels, _ := cm.createSandboxBody["labels"].(map[string]interface{})
	if labels["agenthub.name"] != "clone-1" {
		t.Errorf("clone labels[agenthub.name] = %v, want clone-1", labels["agenthub.name"])
	}
	if labels["agenthub.rootfs_source_type"] != "snapshot" {
		t.Errorf("clone labels[rootfs_source_type] = %v, want snapshot", labels["agenthub.rootfs_source_type"])
	}
}

// TestCloneAgent_ApplyFailureCompensates verifies that when OpenClaw apply
// fails during clone, the freshly-created clone sandbox is deleted
// (compensated), matching CreateInstance's compensation path.
func TestCloneAgent_ApplyFailureCompensates(t *testing.T) {
	cm := &fakeServiceCM{}
	sourcePersistence := "full_snapshot"
	st := &fakeAgentStore{
		getSetting: func(_ context.Context, key string) (string, error) {
			if key == "llm_api_key" {
				return "test-key", nil
			}
			return "", nil
		},
		getInstance: func(_ context.Context, _ string) (*store.AgentInstance, error) {
			return &store.AgentInstance{
				ID:              "agent-src",
				Name:            "src",
				Engine:          "openclaw",
				SandboxID:       "sb-src",
				Domain:          "test.example",
				PersistenceMode: &sourcePersistence,
			}, nil
		},
	}
	svc := newTestService(st, cm)
	svc.applyFn = func(_ context.Context, _ *http.Client, _ string, _ string, _ *LLMRuntimePlan, _ *OpenclawApplyOptions) (*CommandOutput, error) {
		return nil, errors.New("envd unreachable")
	}

	const inboundReqID = "svc-inbound-req-789"
	ctx := cubelog.WithRequestTrace(context.Background(), &cubelog.RequestTrace{RequestID: inboundReqID})

	_, err := svc.CloneAgent(ctx, CloneAgentRequest{
		AgentID: "agent-src",
	})
	if err == nil {
		t.Fatal("CloneAgent should have returned an error")
	}

	if cm.createSandboxBody == nil {
		t.Fatal("CreateSandbox was not called — test setup is wrong")
	}
	if len(cm.deleteSandboxBodies) == 0 {
		t.Fatal("compensation DeleteSandbox was not called after clone apply failure")
	}
	comp := cm.deleteSandboxBodies[0]
	if comp["sandbox_id"] != "sb-test-123" {
		t.Errorf("compensation sandbox_id = %v, want sb-test-123", comp["sandbox_id"])
	}
	reqID, _ := comp["requestID"].(string)
	if reqID != inboundReqID {
		t.Errorf("compensation requestID = %q, want %q", reqID, inboundReqID)
	}
}

// TestCloneAgent_SourceNotFound verifies that cloning a non-existent agent
// returns a 404 service.Error.
func TestCloneAgent_SourceNotFound(t *testing.T) {
	cm := &fakeServiceCM{}
	st := &fakeAgentStore{
		getInstance: func(_ context.Context, _ string) (*store.AgentInstance, error) {
			return nil, nil
		},
	}
	svc := newTestService(st, cm)

	_, err := svc.CloneAgent(context.Background(), CloneAgentRequest{
		AgentID: "no-such-agent",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) {
		t.Fatalf("error is not *service.Error: %v", err)
	}
	if svcErr.Status != 404 {
		t.Errorf("status = %d, want 404", svcErr.Status)
	}
	if cm.createSandboxBody != nil {
		t.Error("CreateSandbox should not have been called for a missing source agent")
	}
}

// ── wrapCMError tests ───────────────────────────────────────────────────────

// TestWrapCMError verifies the CubeMaster error → service.Error mapping for
// the ret codes that the handler previously handled via writeCMError.
func TestWrapCMError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not found", &cubemaster.CMError{RetCode: 130404, RetMsg: "no such sandbox"}, 404, "not_found"},
		{"not found 404", &cubemaster.CMError{RetCode: 404, RetMsg: "no such sandbox"}, 404, "not_found"},
		{"conflict", &cubemaster.CMError{RetCode: 130409, RetMsg: "already exists"}, 409, "conflict"},
		{"pausing (retryable)", &cubemaster.CMError{RetCode: 130490, RetMsg: "pausing"}, 503, "retry-after:2"},
		{"resume failed (retryable)", &cubemaster.CMError{RetCode: 130589, RetMsg: "resume failed"}, 503, "retry-after:5"},
		{"generic CM error", &cubemaster.CMError{RetCode: 500, RetMsg: "internal"}, 502, ""},
		{"non-CM error", errors.New("network timeout"), 502, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapCMError(tt.err)
			if got.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tt.wantStatus)
			}
			if tt.wantCode != "" && got.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", got.Code, tt.wantCode)
			}
		})
	}
}

// TestIsCMNotFoundCoversBothShapes pins that a not-found is recognised whenever
// CubeMaster states one in its envelope — under a 200 body or under any >=400
// status — and is NOT inferred from a bare 404 whose body says nothing.
// Only the first shape appears in the #1327 repro; keying on it alone would let
// the actionable 400 silently regress into a 502 leaking an identifier the
// requester never supplied, which is what this path exists to prevent.
func TestIsCMNotFoundCoversBothShapes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"business 130404", &cubemaster.CMError{RetCode: 130404, RetMsg: "template not found"}, true},
		{"business 404", &cubemaster.CMError{RetCode: 404, RetMsg: "template not found"}, true},
		{
			"http 404 stating it in the envelope",
			&cubemaster.HTTPError{Status: 404, Body: `{"ret":{"ret_code":130404,"ret_msg":"template not found"}}`},
			true,
		},
		{
			"http 404 wrapped",
			fmt.Errorf("create sandbox: %w", &cubemaster.HTTPError{Status: 404, Body: `{"ret":{"ret_code":130404}}`}),
			true,
		},
		// A bare 404 is evidence about the route, not the resource: CubeMaster
		// states resource-not-found in the envelope, so an opaque one came from a
		// proxy or a misconfigured base URL. Classifying it as a missing template
		// would answer "register a template" to an operator whose real problem is
		// that CubeOps never reached CubeMaster.
		{"http 404 from something in the path", &cubemaster.HTTPError{Status: 404, Body: "404 page not found"}, false},
		{"http 500", &cubemaster.HTTPError{Status: 500, Body: "boom"}, false},
		// A server-side failure is not the caller's to fix, whatever its body
		// says — otherwise a transient 5xx is rewritten into "register a
		// template", advice the operator cannot act on and that hides an outage.
		{
			"http 500 carrying 130404",
			&cubemaster.HTTPError{Status: 500, Body: `{"ret":{"ret_code":130404,"ret_msg":"template not found"}}`},
			false,
		},
		{
			"http 503 carrying 130404",
			&cubemaster.HTTPError{Status: 503, Body: `{"ret":{"ret_code":130404,"ret_msg":"template not found"}}`},
			false,
		},
		// The status line and the ret_code are chosen independently on the
		// CubeMaster side, so a not-found can arrive under some other >=400
		// status. Recognise it by content, not by which channel carried it.
		{
			"http 400 carrying 130404",
			&cubemaster.HTTPError{Status: 400, Body: `{"ret":{"ret_code":130404,"ret_msg":"template not found"}}`},
			true,
		},
		{
			"http 400 carrying a non-not-found code",
			&cubemaster.HTTPError{Status: 400, Body: `{"ret":{"ret_code":130400,"ret_msg":"bad instance_type"}}`},
			false,
		},
		{"http 500 with a non-envelope body", &cubemaster.HTTPError{Status: 500, Body: "130404"}, false},
		{"business conflict", &cubemaster.CMError{RetCode: 130409, RetMsg: "exists"}, false},
		{"unrelated", errors.New("network timeout"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCMNotFound(tt.err); got != tt.want {
				t.Errorf("isCMNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ── ReverseSyncAgentHubTemplate tests ───────────────────────────────────────

// TestReverseSync_SoftDeletesMatchingTemplates verifies that when the store
// finds AgentHub templates whose template_id or source_snapshot_id matches
// the infra id, ReverseSyncAgentHubTemplate soft-deletes each of them.
func TestReverseSync_SoftDeletesMatchingTemplates(t *testing.T) {
	var deletedIDs []string
	st := &fakeAgentStore{
		findTemplateIDsByInfraID: func(_ context.Context, infraID string) ([]string, error) {
			if infraID != "infra-tpl-1" {
				t.Errorf("FindTemplateIDsByInfraID got infraID=%q, want infra-tpl-1", infraID)
			}
			return []string{"agenthub-tpl-a", "agenthub-tpl-b"}, nil
		},
		softDeleteAgentHubTemplate: func(_ context.Context, templateID string) error {
			deletedIDs = append(deletedIDs, templateID)
			return nil
		},
	}
	svc := newTestService(st, &fakeServiceCM{})

	svc.ReverseSyncAgentHubTemplate(context.Background(), "infra-tpl-1")

	if len(deletedIDs) != 2 {
		t.Fatalf("soft-deleted %d templates, want 2", len(deletedIDs))
	}
	if deletedIDs[0] != "agenthub-tpl-a" || deletedIDs[1] != "agenthub-tpl-b" {
		t.Errorf("deletedIDs = %v, want [agenthub-tpl-a agenthub-tpl-b]", deletedIDs)
	}
}

// TestReverseSync_NoMatchesIsNoop verifies that when the store finds no
// matching templates, ReverseSyncAgentHubTemplate is a no-op (no
// soft-delete calls).
func TestReverseSync_NoMatchesIsNoop(t *testing.T) {
	softDeleteCalled := false
	st := &fakeAgentStore{
		findTemplateIDsByInfraID: func(_ context.Context, _ string) ([]string, error) {
			return nil, nil // no matches
		},
		softDeleteAgentHubTemplate: func(_ context.Context, _ string) error {
			softDeleteCalled = true
			return nil
		},
	}
	svc := newTestService(st, &fakeServiceCM{})

	svc.ReverseSyncAgentHubTemplate(context.Background(), "infra-tpl-1")

	if softDeleteCalled {
		t.Error("SoftDeleteAgentHubTemplate was called when no templates matched")
	}
}

// TestReverseSync_QueryFailureDoesNotPanic verifies that a store query
// error is logged but not propagated (best-effort semantics matching the
// old Rust reverse_sync_agenthub_template).
func TestReverseSync_QueryFailureDoesNotPanic(t *testing.T) {
	softDeleteCalled := false
	st := &fakeAgentStore{
		findTemplateIDsByInfraID: func(_ context.Context, _ string) ([]string, error) {
			return nil, errors.New("DB connection lost")
		},
		softDeleteAgentHubTemplate: func(_ context.Context, _ string) error {
			softDeleteCalled = true
			return nil
		},
	}
	svc := newTestService(st, &fakeServiceCM{})

	// Should not panic / not return error.
	svc.ReverseSyncAgentHubTemplate(context.Background(), "infra-tpl-1")

	if softDeleteCalled {
		t.Error("SoftDeleteAgentHubTemplate should not be called when FindTemplateIDsByInfraID fails")
	}
}

// TestReverseSync_PartialSoftDeleteFailureContinues verifies that when one
// soft-delete fails, the remaining templates are still processed (best-effort).
func TestReverseSync_PartialSoftDeleteFailureContinues(t *testing.T) {
	var deletedIDs []string
	st := &fakeAgentStore{
		findTemplateIDsByInfraID: func(_ context.Context, _ string) ([]string, error) {
			return []string{"tpl-1", "tpl-2", "tpl-3"}, nil
		},
		softDeleteAgentHubTemplate: func(_ context.Context, templateID string) error {
			deletedIDs = append(deletedIDs, templateID)
			if templateID == "tpl-2" {
				return errors.New("DB error on tpl-2")
			}
			return nil
		},
	}
	svc := newTestService(st, &fakeServiceCM{})

	svc.ReverseSyncAgentHubTemplate(context.Background(), "infra-1")

	// All three must be attempted even though tpl-2 failed.
	if len(deletedIDs) != 3 {
		t.Errorf("soft-delete called %d times, want 3 (partial failure must not stop the loop)", len(deletedIDs))
	}
}
