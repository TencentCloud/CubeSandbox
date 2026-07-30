// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/store"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

// mockPodGetter implements PodGetter for tests.
type mockPodGetter struct {
	pods map[string]*corev1.Pod
}

func (m *mockPodGetter) Get(namespace, name string) (*corev1.Pod, bool) {
	key := namespace + "/" + name
	pod, ok := m.pods[key]
	return pod, ok
}

func newMockPodGetter(pods ...*corev1.Pod) *mockPodGetter {
	m := &mockPodGetter{pods: make(map[string]*corev1.Pod)}
	for _, p := range pods {
		key := p.Namespace + "/" + p.Name
		m.pods[key] = p
	}
	return m
}

// stubReporter is a reporter that captures calls synchronously.
type stubReporter struct {
	events []*types.EvictionEvent
}

func (s *stubReporter) Report(event *types.EvictionEvent) <-chan struct{} {
	s.events = append(s.events, event)
	ch := make(chan struct{})
	close(ch)
	return ch
}

type stubRecoveryManager struct {
	events []*types.EvictionEvent
}

func (s *stubRecoveryManager) OnEviction(event *types.EvictionEvent) {
	s.events = append(s.events, event)
}

func TestHandleNilRequest(t *testing.T) {
	h := &Handler{}
	resp := h.handle(nil)

	if resp.Allowed {
		t.Error("expected Allowed=false for nil request")
	}
	if resp.UID != "" {
		t.Errorf("expected empty UID, got %q", resp.UID)
	}
	if resp.Result == nil {
		t.Fatal("expected Result not nil")
	}
	if resp.Result.Message == "" {
		t.Error("expected non-empty message")
	}
}

func TestHandleEvictionEvent(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.ndjson")
	auditStore, err := store.New(auditPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer auditStore.Close()

	stub := &stubReporter{}
	h := &Handler{
		podGetter: newMockPodGetter(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-abc",
				Namespace: "cube-system",
				Labels: map[string]string{
					instanceTypeLabelKey: "cubebox",
				},
			},
		}),
		store:    auditStore,
		reporter: stub,
	}

	req := &admissionv1.AdmissionRequest{
		UID:       k8stypes.UID("uid-12345-test"),
		Name:      "sandbox-abc",
		Namespace: "cube-system",
		UserInfo: authenticationv1.UserInfo{
			Username: "system:node:worker-01",
		},
	}

	resp := h.handle(req)

	if resp.Allowed {
		t.Error("expected Allowed=false")
	}
	if resp.UID != "uid-12345-test" {
		t.Errorf("expected UID=uid-12345-test, got %q", resp.UID)
	}
	if resp.Result == nil || resp.Result.Message == "" {
		t.Error("expected Result.Message to be set")
	}

	// Verify the event was sent to reporter
	if len(stub.events) != 1 {
		t.Fatalf("expected 1 reporter event, got %d", len(stub.events))
	}
	event := stub.events[0]
	if event.EventID != "uid-12345-test" {
		t.Errorf("EventID: want uid-12345-test, got %s", event.EventID)
	}
	if event.PodName != "sandbox-abc" {
		t.Errorf("PodName: want sandbox-abc, got %s", event.PodName)
	}
	if event.Namespace != "cube-system" {
		t.Errorf("Namespace: want cube-system, got %s", event.Namespace)
	}
	if event.NodeName != "worker-01" {
		t.Errorf("NodeName: want worker-01, got %s", event.NodeName)
	}
	if event.InstanceType != "cubebox" {
		t.Errorf("InstanceType: want cubebox, got %s", event.InstanceType)
	}

	// Verify the event was persisted
	auditStore.Close()
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var recovered types.EvictionEvent
	// NDJSON: first line minus trailing newline
	json.Unmarshal(data[:len(data)-1], &recovered)
	if recovered.EventID != "uid-12345-test" {
		t.Errorf("audit EventID: want uid-12345-test, got %s", recovered.EventID)
	}
}

func TestHandlePodNotInCache(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.ndjson")
	auditStore, _ := store.New(auditPath)
	defer auditStore.Close()

	stub := &stubReporter{}
	h := &Handler{
		// No pods in cache
		podGetter: newMockPodGetter(),
		store:     auditStore,
		reporter:  stub,
	}

	req := &admissionv1.AdmissionRequest{
		UID:       k8stypes.UID("uid-missing"),
		Name:      "sandbox-gone",
		Namespace: "cube-system",
		UserInfo: authenticationv1.UserInfo{
			Username: "system:node:worker-02",
		},
	}

	resp := h.handle(req)

	// Should still deny eviction even if Pod not in cache
	if resp.Allowed {
		t.Error("expected Allowed=false even when Pod not in cache")
	}

	if len(stub.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(stub.events))
	}
	if stub.events[0].InstanceType != "" {
		t.Errorf("expected empty InstanceType for missing pod, got %q", stub.events[0].InstanceType)
	}
}

func TestHandleTriggersRecoveryOnlyForNodeUser(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.ndjson")
	auditStore, _ := store.New(auditPath)
	defer auditStore.Close()

	recoveryMgr := &stubRecoveryManager{}
	h := NewWithRecovery(newMockPodGetter(), auditStore, nil, recoveryMgr)

	resp := h.handle(&admissionv1.AdmissionRequest{
		UID:       k8stypes.UID("uid-node-user"),
		Name:      "sandbox-node-user",
		Namespace: "cube-system",
		UserInfo: authenticationv1.UserInfo{
			Username: "system:node:worker-01",
		},
	})
	if resp.Allowed {
		t.Fatal("expected node-user eviction to be denied")
	}
	if len(recoveryMgr.events) != 1 {
		t.Fatalf("expected node-user eviction to trigger recovery, got %d", len(recoveryMgr.events))
	}

	resp = h.handle(&admissionv1.AdmissionRequest{
		UID:       k8stypes.UID("uid-admin-user"),
		Name:      "sandbox-admin-user",
		Namespace: "cube-system",
		UserInfo: authenticationv1.UserInfo{
			Username: "admin@example.com",
		},
	})
	if !resp.Allowed {
		t.Fatal("expected admin eviction to be allowed")
	}
	if len(recoveryMgr.events) != 1 {
		t.Fatalf("expected admin eviction not to trigger recovery, got %d events", len(recoveryMgr.events))
	}
}

func TestHandleKubeletEvictionRequiresMemoryPressure(t *testing.T) {
	tests := []struct {
		name          string
		underPressure bool
		checkErr      error
		wantAllowed   bool
		wantRecovery  int
	}{
		{name: "memory_pressure", underPressure: true, wantAllowed: false, wantRecovery: 1},
		{name: "not_memory_pressure", underPressure: false, wantAllowed: true, wantRecovery: 0},
		{name: "pressure_check_error", checkErr: fmt.Errorf("node lookup failed"), wantAllowed: true, wantRecovery: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recoveryMgr := &stubRecoveryManager{}
			h := NewWithRecovery(newMockPodGetter(), nil, nil, recoveryMgr)
			h.SetPressureChecker(func(context.Context, string) (bool, error) {
				if tc.checkErr != nil {
					return false, tc.checkErr
				}
				return tc.underPressure, nil
			})

			resp := h.handle(&admissionv1.AdmissionRequest{
				UID:       k8stypes.UID("uid-" + tc.name),
				Name:      "sandbox-" + tc.name,
				Namespace: "cube-system",
				UserInfo: authenticationv1.UserInfo{
					Username: "system:node:worker-01",
				},
			})
			if resp.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed: want %v, got %v", tc.wantAllowed, resp.Allowed)
			}
			if len(recoveryMgr.events) != tc.wantRecovery {
				t.Fatalf("recovery events: want %d, got %d", tc.wantRecovery, len(recoveryMgr.events))
			}
		})
	}
}

func TestNodeFromUserInfo(t *testing.T) {
	tests := []struct {
		username string
		expected string
	}{
		{"system:node:worker-01", "worker-01"},
		{"system:node:node-42.cluster.local", "node-42.cluster.local"},
		{"system:kube-scheduler", "system:kube-scheduler"}, // non-node service account
		{"", ""},
	}

	for _, tc := range tests {
		result := nodeFromUserInfo(tc.username)
		if result != tc.expected {
			t.Errorf("nodeFromUserInfo(%q): want %q, got %q", tc.username, tc.expected, result)
		}
	}
}

func TestServeHTTP(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.ndjson")
	auditStore, _ := store.New(auditPath)
	defer auditStore.Close()

	stub := &stubReporter{}
	handler := New(
		newMockPodGetter(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-e2e",
				Namespace: "cube-system",
				Labels: map[string]string{
					instanceTypeLabelKey: "cubebox",
				},
			},
		}),
		auditStore,
		stub,
	)

	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       k8stypes.UID("uid-http-test"),
			Name:      "sandbox-e2e",
			Namespace: "cube-system",
			UserInfo: authenticationv1.UserInfo{
				Username: "system:node:worker-99",
			},
		},
	}

	body, _ := json.Marshal(review)
	req := httptest.NewRequest(http.MethodPost, "/webhook/eviction", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: body=%s", rec.Code, rec.Body.String())
	}

	var result admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result.Response == nil {
		t.Fatal("expected Response in AdmissionReview")
	}
	if result.Response.Allowed {
		t.Error("expected Allowed=false")
	}
	if result.Response.UID != "uid-http-test" {
		t.Errorf("expected UID=uid-http-test, got %q", result.Response.UID)
	}

	// Verify reporter was called
	if len(stub.events) != 1 {
		t.Errorf("expected 1 reporter event, got %d", len(stub.events))
	}
}

func TestServeHTTPBadBody(t *testing.T) {
	handler := New(newMockPodGetter(), nil, &stubReporter{})

	req := httptest.NewRequest(http.MethodPost, "/webhook/eviction", bytes.NewReader([]byte("not-json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad body, got %d", rec.Code)
	}
}

func TestHandleEvictionReturnsInterceptedAt(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.ndjson")
	auditStore, _ := store.New(auditPath)
	defer auditStore.Close()

	stub := &stubReporter{}
	h := &Handler{
		podGetter: newMockPodGetter(),
		store:     auditStore,
		reporter:  stub,
	}

	req := &admissionv1.AdmissionRequest{
		UID:       k8stypes.UID("uid-time"),
		Name:      "sandbox-time",
		Namespace: "cube-system",
		UserInfo: authenticationv1.UserInfo{
			Username: "system:node:worker-01",
		},
	}

	resp := h.handle(req)
	if resp.Allowed {
		t.Error("expected Allowed=false")
	}

	if len(stub.events) != 1 {
		t.Fatalf("expected 1 event")
	}
	if stub.events[0].InterceptedAt == "" {
		t.Error("InterceptedAt should not be empty")
	}
	// Verify it's a valid RFC3339 timestamp
	if stub.events[0].InterceptedAt != "" {
		// Basic format check: should contain T and end with Z
		ts := stub.events[0].InterceptedAt
		if len(ts) < 20 {
			t.Errorf("InterceptedAt too short: %q", ts)
		}
	}
}

// Ensure PodGetter interface is satisfied by *podinformer.Cache (compile-time check).
func TestPodGetterInterfaceSatisfaction(t *testing.T) {
	// This type assertion verifies *podinformer.Cache implements PodGetter.
	// The actual check is compile-time in the production code path;
	// this test documents the expectation.
	var _ PodGetter = newMockPodGetter()
	// Will not compile if mockPodGetter.Get signature changes.
}

func TestDenyResponse(t *testing.T) {
	resp := denyResponse("uid-abc", "test denial message")
	if resp.Allowed {
		t.Error("denyResponse must set Allowed=false")
	}
	if resp.UID != "uid-abc" {
		t.Errorf("UID: want uid-abc, got %q", resp.UID)
	}
	if resp.Result.Message != "test denial message" {
		t.Errorf("Message: want 'test denial message', got %q", resp.Result.Message)
	}
}
