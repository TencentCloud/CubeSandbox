// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

type fakeTerminalStore struct {
	createdGrant     *store.TerminalGrant
	createGrantErr   error
	consumedGrant    *store.TerminalGrant
	consumeErr       error
	session          *store.TerminalSession
	getSessionErr    error
	createdSession   *store.TerminalSession
	createSessionErr error
	resumeErr        error
	resumeCalls      int
}

func (f *fakeTerminalStore) CreateTerminalGrant(_ context.Context, grant *store.TerminalGrant, _ int) error {
	copy := *grant
	f.createdGrant = &copy
	return f.createGrantErr
}
func (f *fakeTerminalStore) ConsumeTerminalGrant(context.Context, string) (*store.TerminalGrant, error) {
	return f.consumedGrant, f.consumeErr
}
func (f *fakeTerminalStore) CleanupTerminalGrants(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (f *fakeTerminalStore) GetTerminalSession(context.Context, string) (*store.TerminalSession, error) {
	return f.session, f.getSessionErr
}
func (f *fakeTerminalStore) CreateTerminalSession(_ context.Context, session *store.TerminalSession, _ int, _ time.Time) error {
	copy := *session
	f.createdSession = &copy
	return f.createSessionErr
}
func (f *fakeTerminalStore) ResumeTerminalSession(context.Context, string, string, string, string, time.Time, time.Time) error {
	f.resumeCalls++
	return f.resumeErr
}
func (f *fakeTerminalStore) TouchTerminalSession(context.Context, string, time.Time, int64, int64) error {
	return nil
}
func (f *fakeTerminalStore) CloseTerminalSession(context.Context, string, time.Time, string, *int32, int64, int64) error {
	return nil
}
func (f *fakeTerminalStore) ReconcileOrphanTerminalSessions(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

type fakeTerminalCM struct {
	response         json.RawMessage
	err              error
	requestedSandbox string
	requestedType    string
}

func (f *fakeTerminalCM) GetSandbox(_ context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
	f.requestedSandbox = sandboxID
	f.requestedType = instanceType
	return f.response, f.err
}

func testTerminalConfig() config.TerminalConfig {
	return config.TerminalConfig{
		Enabled:               true,
		GrantTTLSeconds:       60,
		ReconnectGraceSeconds: 30,
		MaxSessionsPerUser:    5,
		InternalToken:         "test-shared-terminal-token",
	}
}

func runningTerminalTargetJSON() json.RawMessage {
	return json.RawMessage(`{
		"ret":{"ret_code":0},
		"data":[{
			"sandbox_id":"sandbox-a","status":1,"host_id":"node-a",
			"containers":[
				{"name":"primary","container_id":"container-a","status":1,"type":"sandbox"},
				{"name":"sidecar","container_id":"container-b","status":1,"type":"sidecar"}
			]
		}]
	}`)
}

func TestCanOpenTerminal(t *testing.T) {
	if !CanOpenTerminal(TerminalPrincipal{UserID: "admin", Role: "admin"}) {
		t.Fatal("admin principal was rejected")
	}
	for _, principal := range []TerminalPrincipal{{UserID: "admin", Role: "viewer"}, {Role: "admin"}} {
		if CanOpenTerminal(principal) {
			t.Errorf("CanOpenTerminal(%+v) = true, want false", principal)
		}
	}
}

func TestIssueTerminalGrantOpenBindsCanonicalTargetAndStoresOnlyHash(t *testing.T) {
	fixedNow := time.Date(2026, 7, 29, 22, 0, 0, 0, time.UTC)
	fakeStore := &fakeTerminalStore{}
	fakeCM := &fakeTerminalCM{response: runningTerminalTargetJSON()}
	svc := NewTerminalService(fakeStore, fakeCM, testTerminalConfig())
	svc.now = func() time.Time { return fixedNow }
	svc.mint = func() (string, error) { return "AAECAwQFBgcICQoLDA0ODw", nil }

	response, terminalErr := svc.IssueTerminalGrant(context.Background(), TerminalPrincipal{UserID: "admin", Role: "admin"}, TerminalGrantRequest{
		SandboxID: "sandbox-a",
		Cols:      120,
		Rows:      40,
	})
	if terminalErr != nil {
		t.Fatalf("IssueTerminalGrant: %v", terminalErr)
	}
	if fakeCM.requestedSandbox != "sandbox-a" || fakeCM.requestedType != terminalInstanceType {
		t.Errorf("CubeMaster request = sandbox:%q type:%q", fakeCM.requestedSandbox, fakeCM.requestedType)
	}
	if response.ContainerID != "container-a" || response.SandboxID != "sandbox-a" {
		t.Errorf("response target = %s/%s", response.SandboxID, response.ContainerID)
	}
	if response.WSURL != terminalPublicWSPath || response.SessionID == "" || len(response.Containers) != 2 {
		t.Errorf("response metadata = %+v", response)
	}
	if fakeStore.createdGrant == nil {
		t.Fatal("grant was not persisted")
	}
	digest := sha256.Sum256([]byte(response.Token))
	if fakeStore.createdGrant.TokenHash != hex.EncodeToString(digest[:]) {
		t.Error("persisted token hash does not match the returned one-time credential")
	}
	if fakeStore.createdGrant.TokenHash == response.Token {
		t.Error("raw grant was persisted")
	}
	if fakeStore.createdGrant.SessionID != response.SessionID || fakeStore.createdGrant.ExpiresAt != fixedNow.Add(time.Minute) {
		t.Errorf("persisted binding = %+v", fakeStore.createdGrant)
	}
}

func TestIssueTerminalGrantRejectsNonRunningAndMissingContainer(t *testing.T) {
	tests := []struct {
		name        string
		response    json.RawMessage
		containerID string
		wantStatus  int
		wantCode    string
	}{
		{
			name:       "sandbox not running",
			response:   json.RawMessage(`{"ret":{"ret_code":0},"data":[{"sandbox_id":"sandbox-a","status":4,"containers":[]}]}`),
			wantStatus: http.StatusConflict,
			wantCode:   "TARGET_NOT_RUNNING",
		},
		{
			name:        "container not found",
			response:    runningTerminalTargetJSON(),
			containerID: "missing",
			wantStatus:  http.StatusNotFound,
			wantCode:    "TARGET_NOT_FOUND",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := NewTerminalService(&fakeTerminalStore{}, &fakeTerminalCM{response: test.response}, testTerminalConfig())
			_, terminalErr := svc.IssueTerminalGrant(context.Background(), TerminalPrincipal{UserID: "admin", Role: "admin"}, TerminalGrantRequest{
				SandboxID: "sandbox-a", ContainerID: test.containerID, Cols: 80, Rows: 24,
			})
			if terminalErr == nil || terminalErr.Status != test.wantStatus || terminalErr.Code != test.wantCode {
				t.Fatalf("error = %+v, want status=%d code=%s", terminalErr, test.wantStatus, test.wantCode)
			}
		})
	}
}

func TestIssueTerminalResumeGrantUsesOriginalUserAndTarget(t *testing.T) {
	fixedNow := time.Date(2026, 7, 29, 22, 0, 0, 0, time.UTC)
	fakeStore := &fakeTerminalStore{session: &store.TerminalSession{
		ID: "de305d54-75b4-431b-adb2-eb6b9e546014", UserID: "admin",
		SandboxID: "sandbox-a", ContainerID: "container-a", LastSeenAt: fixedNow.Add(-time.Second),
	}}
	svc := NewTerminalService(fakeStore, &fakeTerminalCM{response: runningTerminalTargetJSON()}, testTerminalConfig())
	svc.now = func() time.Time { return fixedNow }
	svc.mint = func() (string, error) { return "AAECAwQFBgcICQoLDA0ODw", nil }

	response, terminalErr := svc.IssueTerminalGrant(context.Background(), TerminalPrincipal{UserID: "admin", Role: "admin"}, TerminalGrantRequest{
		Kind: "resume", SessionID: fakeStore.session.ID, LastOffset: 42, Cols: 100, Rows: 30,
	})
	if terminalErr != nil {
		t.Fatalf("IssueTerminalGrant resume: %v", terminalErr)
	}
	if response.SessionID != fakeStore.session.ID || fakeStore.createdGrant.ResumeOffset != 42 {
		t.Errorf("resume response/grant = %+v / %+v", response, fakeStore.createdGrant)
	}

	_, terminalErr = svc.IssueTerminalGrant(context.Background(), TerminalPrincipal{UserID: "other", Role: "admin"}, TerminalGrantRequest{
		Kind: "resume", SessionID: fakeStore.session.ID, Cols: 100, Rows: 30,
	})
	if terminalErr == nil || terminalErr.Status != http.StatusForbidden || terminalErr.Code != "FORBIDDEN" {
		t.Fatalf("cross-user resume error = %+v, want FORBIDDEN", terminalErr)
	}
}

func TestConsumeAndPrepareTerminalGrant(t *testing.T) {
	fixedNow := time.Date(2026, 7, 29, 22, 0, 0, 0, time.UTC)
	fakeStore := &fakeTerminalStore{consumedGrant: &store.TerminalGrant{
		ID: "grant-id", Kind: "open", UserID: "admin", SandboxID: "sandbox-a",
		ContainerID: "container-a", SessionID: "de305d54-75b4-431b-adb2-eb6b9e546014",
		Cols: 80, Rows: 24,
	}}
	svc := NewTerminalService(fakeStore, &fakeTerminalCM{response: runningTerminalTargetJSON()}, testTerminalConfig())
	svc.now = func() time.Time { return fixedNow }
	grant, terminalErr := svc.ConsumeTerminalGrant(context.Background(), "AAECAwQFBgcICQoLDA0ODw")
	if terminalErr != nil {
		t.Fatalf("ConsumeTerminalGrant: %v", terminalErr)
	}
	if terminalErr = svc.PrepareTerminalSession(context.Background(), grant); terminalErr != nil {
		t.Fatalf("PrepareTerminalSession: %v", terminalErr)
	}
	if fakeStore.createdSession == nil || fakeStore.createdSession.ID != grant.SessionID || fakeStore.createdSession.CubeletHost != "node-a" {
		t.Errorf("created session = %+v", fakeStore.createdSession)
	}

	_, terminalErr = svc.ConsumeTerminalGrant(context.Background(), "not-a-grant")
	if terminalErr == nil || terminalErr.Code != "GRANT_INVALID" {
		t.Fatalf("invalid raw grant error = %+v", terminalErr)
	}

	fakeStore.consumeErr = store.ErrTerminalGrantInvalid
	_, terminalErr = svc.ConsumeTerminalGrant(context.Background(), "AAECAwQFBgcICQoLDA0ODw")
	if terminalErr == nil || terminalErr.Code != "GRANT_INVALID" || terminalErr.Cause != nil {
		t.Fatalf("replayed grant error = %+v", terminalErr)
	}
}

func TestIssueTerminalGrantValidationAndLimitMapping(t *testing.T) {
	validPrincipal := TerminalPrincipal{UserID: "admin", Role: "admin"}
	validRequest := TerminalGrantRequest{SandboxID: "sandbox-a", Cols: 80, Rows: 24}
	tests := []struct {
		name       string
		principal  TerminalPrincipal
		request    TerminalGrantRequest
		configure  func(*config.TerminalConfig, *fakeTerminalStore)
		wantStatus int
		wantCode   string
	}{
		{
			name: "disabled", principal: validPrincipal, request: validRequest,
			configure:  func(cfg *config.TerminalConfig, _ *fakeTerminalStore) { cfg.Enabled = false },
			wantStatus: http.StatusServiceUnavailable, wantCode: "INTERNAL",
		},
		{name: "forbidden role", principal: TerminalPrincipal{UserID: "admin", Role: "viewer"}, request: validRequest, wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN"},
		{name: "bad kind", principal: validPrincipal, request: TerminalGrantRequest{Kind: "exec", SandboxID: "sandbox-a", Cols: 80, Rows: 24}, wantStatus: http.StatusBadRequest, wantCode: "PROTOCOL_ERROR"},
		{name: "zero dimensions", principal: validPrincipal, request: TerminalGrantRequest{SandboxID: "sandbox-a", Rows: 24}, wantStatus: http.StatusBadRequest, wantCode: "PROTOCOL_ERROR"},
		{name: "oversized dimensions", principal: validPrincipal, request: TerminalGrantRequest{SandboxID: "sandbox-a", Cols: 1001, Rows: 24}, wantStatus: http.StatusBadRequest, wantCode: "PROTOCOL_ERROR"},
		{name: "open resume cursor", principal: validPrincipal, request: TerminalGrantRequest{SandboxID: "sandbox-a", Cols: 80, Rows: 24, LastOffset: 1}, wantStatus: http.StatusBadRequest, wantCode: "PROTOCOL_ERROR"},
		{
			name: "pending grant limit", principal: validPrincipal, request: validRequest,
			configure: func(_ *config.TerminalConfig, fakeStore *fakeTerminalStore) {
				fakeStore.createGrantErr = store.ErrTerminalGrantLimit
			},
			wantStatus: http.StatusTooManyRequests, wantCode: "LIMIT_EXCEEDED",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testTerminalConfig()
			fakeStore := &fakeTerminalStore{}
			if test.configure != nil {
				test.configure(&cfg, fakeStore)
			}
			svc := NewTerminalService(fakeStore, &fakeTerminalCM{response: runningTerminalTargetJSON()}, cfg)
			svc.mint = func() (string, error) { return "AAECAwQFBgcICQoLDA0ODw", nil }
			_, terminalErr := svc.IssueTerminalGrant(context.Background(), test.principal, test.request)
			if terminalErr == nil || terminalErr.Status != test.wantStatus || terminalErr.Code != test.wantCode {
				t.Fatalf("error = %+v, want status=%d code=%s", terminalErr, test.wantStatus, test.wantCode)
			}
		})
	}
}

func TestIssueTerminalResumeGrantRejectsLostAndChangedSessions(t *testing.T) {
	fixedNow := time.Date(2026, 7, 29, 22, 0, 0, 0, time.UTC)
	sessionID := "de305d54-75b4-431b-adb2-eb6b9e546014"
	tests := []struct {
		name     string
		session  *store.TerminalSession
		request  TerminalGrantRequest
		wantCode string
	}{
		{
			name:     "grace expired",
			session:  &store.TerminalSession{ID: sessionID, UserID: "admin", SandboxID: "sandbox-a", ContainerID: "container-a", LastSeenAt: fixedNow.Add(-time.Minute)},
			request:  TerminalGrantRequest{Kind: "resume", SessionID: sessionID, Cols: 80, Rows: 24},
			wantCode: "SESSION_LOST",
		},
		{
			name:     "changed sandbox binding",
			session:  &store.TerminalSession{ID: sessionID, UserID: "admin", SandboxID: "sandbox-a", ContainerID: "container-a", LastSeenAt: fixedNow},
			request:  TerminalGrantRequest{Kind: "resume", SessionID: sessionID, SandboxID: "sandbox-b", Cols: 80, Rows: 24},
			wantCode: "FORBIDDEN",
		},
		{
			name:     "changed container binding",
			session:  &store.TerminalSession{ID: sessionID, UserID: "admin", SandboxID: "sandbox-a", ContainerID: "container-a", LastSeenAt: fixedNow},
			request:  TerminalGrantRequest{Kind: "resume", SessionID: sessionID, ContainerID: "container-b", Cols: 80, Rows: 24},
			wantCode: "FORBIDDEN",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeStore := &fakeTerminalStore{session: test.session}
			svc := NewTerminalService(fakeStore, &fakeTerminalCM{response: runningTerminalTargetJSON()}, testTerminalConfig())
			svc.now = func() time.Time { return fixedNow }
			_, terminalErr := svc.IssueTerminalGrant(context.Background(), TerminalPrincipal{UserID: "admin", Role: "admin"}, test.request)
			if terminalErr == nil || terminalErr.Code != test.wantCode {
				t.Fatalf("error = %+v, want %s", terminalErr, test.wantCode)
			}
		})
	}
}

func TestPrepareTerminalSessionMapsOpenLimitAndResumeLoss(t *testing.T) {
	openStore := &fakeTerminalStore{createSessionErr: store.ErrTerminalSessionLimit}
	svc := NewTerminalService(openStore, &fakeTerminalCM{response: runningTerminalTargetJSON()}, testTerminalConfig())
	terminalErr := svc.PrepareTerminalSession(context.Background(), &ConsumedTerminalGrant{
		Kind: "open", UserID: "admin", SandboxID: "sandbox-a", ContainerID: "container-a",
		SessionID: "de305d54-75b4-431b-adb2-eb6b9e546014", Cols: 80, Rows: 24,
	})
	if terminalErr == nil || terminalErr.Status != http.StatusTooManyRequests || terminalErr.Code != "LIMIT_EXCEEDED" {
		t.Fatalf("open limit error = %+v", terminalErr)
	}

	resumeStore := &fakeTerminalStore{resumeErr: store.ErrTerminalSessionLost}
	svc = NewTerminalService(resumeStore, &fakeTerminalCM{response: runningTerminalTargetJSON()}, testTerminalConfig())
	terminalErr = svc.PrepareTerminalSession(context.Background(), &ConsumedTerminalGrant{
		Kind: "resume", UserID: "admin", SandboxID: "sandbox-a", ContainerID: "container-a",
		SessionID: "de305d54-75b4-431b-adb2-eb6b9e546014", Cols: 80, Rows: 24,
	})
	if terminalErr == nil || terminalErr.Status != http.StatusConflict || terminalErr.Code != "SESSION_LOST" || resumeStore.resumeCalls != 1 {
		t.Fatalf("resume loss error = %+v calls=%d", terminalErr, resumeStore.resumeCalls)
	}
}
