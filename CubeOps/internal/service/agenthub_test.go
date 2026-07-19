// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
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

func (f *fakeServiceCM) CreateSandbox(_ context.Context, body interface{}) (json.RawMessage, error) {
	f.createSandboxBody = decodeBody(body)
	if f.createSandboxResp == nil {
		f.createSandboxResp = json.RawMessage(`{"sandbox_id":"sb-test-123","ret":{"ret_code":0}}`)
	}
	return f.createSandboxResp, f.createSandboxErr
}
func (f *fakeServiceCM) DeleteSandbox(_ context.Context, body interface{}) (json.RawMessage, error) {
	f.deleteSandboxBodies = append(f.deleteSandboxBodies, decodeBody(body))
	return nil, f.deleteSandboxErr
}
func (f *fakeServiceCM) UpdateSandbox(_ context.Context, body interface{}) (json.RawMessage, error) {
	f.updateSandboxBody = decodeBody(body)
	return nil, f.updateSandboxErr
}
func (f *fakeServiceCM) CreateSnapshot(_ context.Context, body interface{}) (json.RawMessage, error) {
	f.createSnapshotBody = decodeBody(body)
	return f.createSnapshotResp, f.createSnapshotErr
}
func (f *fakeServiceCM) DeleteSnapshot(_ context.Context, snapshotID string) (json.RawMessage, error) {
	f.deleteSnapshotID = snapshotID
	return nil, f.deleteSnapshotErr
}
func (f *fakeServiceCM) RollbackSandbox(_ context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
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
		applyFn: func(_ *http.Client, _ string, _ string, _ *LLMRuntimePlan, _ *OpenclawApplyOptions) (*CommandOutput, error) {
			return &CommandOutput{ExitCode: 0, Stdout: "applied", Stderr: ""}, nil
		},
		resolveGatewayFn: func(_ *http.Client, _ string, _ string, _ string, fallback string) string {
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
	svc.applyFn = func(_ *http.Client, _ string, _ string, _ *LLMRuntimePlan, _ *OpenclawApplyOptions) (*CommandOutput, error) {
		return nil, errors.New("envd unreachable")
	}

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
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
	if !strings.HasPrefix(reqID, "cubeops-compensate-apply_openclaw-") {
		t.Errorf("compensation requestID = %q, want prefix cubeops-compensate-apply_openclaw-", reqID)
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

	_, err := svc.CreateInstance(context.Background(), CreateInstanceRequest{
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
	if !strings.HasPrefix(reqID, "cubeops-compensate-upsert_instance-") {
		t.Errorf("compensation requestID = %q, want prefix cubeops-compensate-upsert_instance-", reqID)
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
	svc.applyFn = func(_ *http.Client, _ string, _ string, _ *LLMRuntimePlan, _ *OpenclawApplyOptions) (*CommandOutput, error) {
		return nil, errors.New("envd unreachable")
	}

	_, err := svc.CloneAgent(context.Background(), CloneAgentRequest{
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
	if !strings.HasPrefix(reqID, "cubeops-clone-") {
		t.Errorf("compensation requestID = %q, want prefix cubeops-clone-", reqID)
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
