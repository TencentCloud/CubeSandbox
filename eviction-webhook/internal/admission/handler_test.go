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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/store"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

// ── Test doubles ────────────────────────────────────────────────────────────

type mockPodGetter struct {
	pods map[string]*corev1.Pod
}

func (m *mockPodGetter) Get(namespace, name string) (*corev1.Pod, bool) {
	pod, ok := m.pods[namespace+"/"+name]
	return pod, ok
}

func newMockPodGetter(pods ...*corev1.Pod) *mockPodGetter {
	m := &mockPodGetter{pods: make(map[string]*corev1.Pod)}
	for _, p := range pods {
		m.pods[p.Namespace+"/"+p.Name] = p
	}
	return m
}

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

// ── Helper ───────────────────────────────────────────────────────────────────

func newSandboxPod(name, namespace, instanceType string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{instanceTypeLabelKey: instanceType},
		},
	}
}

func kubeletRequest(uid, podName, namespace, nodeName string) *admissionv1.AdmissionRequest {
	return &admissionv1.AdmissionRequest{
		UID:       k8stypes.UID(uid),
		Name:      podName,
		Namespace: namespace,
		UserInfo:  authenticationv1.UserInfo{Username: "system:node:" + nodeName},
	}
}

// ── Unit tests ───────────────────────────────────────────────────────────────

func TestHandleNilRequest(t *testing.T) {
	resp := New(newMockPodGetter(), nil, nil).handle(nil)

	assert.False(t, resp.Allowed)
	assert.Empty(t, resp.UID)
	require.NotNil(t, resp.Result)
	assert.NotEmpty(t, resp.Result.Message)
}

func TestHandleEvictionEvent(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.ndjson")
	auditStore, err := store.New(auditPath)
	require.NoError(t, err)
	defer auditStore.Close()

	stub := &stubReporter{}
	h := New(
		newMockPodGetter(newSandboxPod("sandbox-abc", "cube-system", "cubebox")),
		auditStore,
		stub,
	)

	resp := h.handle(kubeletRequest("uid-12345-test", "sandbox-abc", "cube-system", "worker-01"))

	assert.False(t, resp.Allowed)
	assert.Equal(t, k8stypes.UID("uid-12345-test"), resp.UID)
	require.NotNil(t, resp.Result)
	assert.NotEmpty(t, resp.Result.Message)

	require.Len(t, stub.events, 1)
	ev := stub.events[0]
	assert.Equal(t, "uid-12345-test", ev.EventID)
	assert.Equal(t, "sandbox-abc", ev.PodName)
	assert.Equal(t, "cube-system", ev.Namespace)
	assert.Equal(t, "worker-01", ev.NodeName)
	assert.Equal(t, "cubebox", ev.InstanceType)

	// Verify persisted to audit log
	auditStore.Close()
	data, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	var recovered types.EvictionEvent
	require.NoError(t, json.Unmarshal(data[:len(data)-1], &recovered))
	assert.Equal(t, "uid-12345-test", recovered.EventID)
}

func TestHandlePodNotInCache(t *testing.T) {
	auditStore, _ := store.New(filepath.Join(t.TempDir(), "audit.ndjson"))
	defer auditStore.Close()

	stub := &stubReporter{}
	h := New(newMockPodGetter(), auditStore, stub)

	resp := h.handle(kubeletRequest("uid-missing", "sandbox-gone", "cube-system", "worker-02"))

	assert.False(t, resp.Allowed, "should deny even when Pod not in cache")
	require.Len(t, stub.events, 1)
	assert.Empty(t, stub.events[0].InstanceType, "InstanceType should be empty for missing pod")
}

func TestHandleTriggersRecoveryOnlyForNodeUser(t *testing.T) {
	auditStore, _ := store.New(filepath.Join(t.TempDir(), "audit.ndjson"))
	defer auditStore.Close()

	recoveryMgr := &stubRecoveryManager{}
	h := NewWithRecovery(newMockPodGetter(), auditStore, nil, recoveryMgr)

	// kubelet eviction → denied, recovery triggered
	resp := h.handle(kubeletRequest("uid-node", "sandbox-node", "cube-system", "worker-01"))
	assert.False(t, resp.Allowed)
	assert.Len(t, recoveryMgr.events, 1)

	// admin eviction → allowed, recovery NOT triggered
	resp = h.handle(&admissionv1.AdmissionRequest{
		UID:       "uid-admin",
		Name:      "sandbox-admin",
		Namespace: "cube-system",
		UserInfo:  authenticationv1.UserInfo{Username: "admin@example.com"},
	})
	assert.True(t, resp.Allowed)
	assert.Len(t, recoveryMgr.events, 1, "admin eviction must not trigger recovery")
}

func TestHandleKubeletEvictionRequiresMemoryPressure(t *testing.T) {
	tests := []struct {
		name          string
		underPressure bool
		checkErr      error
		wantAllowed   bool
		wantRecovery  int
	}{
		{"memory_pressure", true, nil, false, 1},
		{"not_memory_pressure", false, nil, true, 0},
		{"pressure_check_error", false, fmt.Errorf("node lookup failed"), true, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recoveryMgr := &stubRecoveryManager{}
			h := NewWithRecovery(newMockPodGetter(), nil, nil, recoveryMgr)
			h.SetPressureChecker(func(context.Context, string) (bool, error) {
				return tc.underPressure, tc.checkErr
			})

			resp := h.handle(kubeletRequest("uid-"+tc.name, "sandbox-"+tc.name, "cube-system", "worker-01"))
			assert.Equal(t, tc.wantAllowed, resp.Allowed)
			assert.Len(t, recoveryMgr.events, tc.wantRecovery)
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
		{"system:kube-scheduler", "system:kube-scheduler"},
		{"", ""},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.expected, nodeFromUserInfo(tc.username), "input: %q", tc.username)
	}
}

func TestServeHTTP(t *testing.T) {
	auditStore, _ := store.New(filepath.Join(t.TempDir(), "audit.ndjson"))
	defer auditStore.Close()

	stub := &stubReporter{}
	handler := New(
		newMockPodGetter(newSandboxPod("sandbox-e2e", "cube-system", "cubebox")),
		auditStore, stub,
	)

	body, _ := json.Marshal(admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "uid-http-test",
			Name:      "sandbox-e2e",
			Namespace: "cube-system",
			UserInfo:  authenticationv1.UserInfo{Username: "system:node:worker-99"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook/eviction", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var result admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.NotNil(t, result.Response)
	assert.False(t, result.Response.Allowed)
	assert.Equal(t, k8stypes.UID("uid-http-test"), result.Response.UID)
	assert.Len(t, stub.events, 1)
}

func TestServeHTTPBadBody(t *testing.T) {
	handler := New(newMockPodGetter(), nil, &stubReporter{})
	req := httptest.NewRequest(http.MethodPost, "/webhook/eviction", bytes.NewReader([]byte("not-json")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleEvictionReturnsInterceptedAt(t *testing.T) {
	auditStore, _ := store.New(filepath.Join(t.TempDir(), "audit.ndjson"))
	defer auditStore.Close()

	stub := &stubReporter{}
	h := New(newMockPodGetter(), auditStore, stub)
	h.handle(kubeletRequest("uid-time", "sandbox-time", "cube-system", "worker-01"))

	require.Len(t, stub.events, 1)
	assert.NotEmpty(t, stub.events[0].InterceptedAt)
	assert.GreaterOrEqual(t, len(stub.events[0].InterceptedAt), 20, "should be valid RFC3339")
}

func TestPodGetterInterfaceSatisfaction(t *testing.T) {
	var _ PodGetter = newMockPodGetter()
}

func TestDenyResponse(t *testing.T) {
	resp := denyResponse("uid-abc", "test denial message")
	assert.False(t, resp.Allowed)
	assert.Equal(t, k8stypes.UID("uid-abc"), resp.UID)
	assert.Equal(t, "test denial message", resp.Result.Message)
}

// TestNewWithLogger verifies that NewWithLogger returns a non-nil Handler when
// all optional parameters (store, reporter, recoveryMgr) are nil.
func TestNewWithLogger(t *testing.T) {
	h := NewWithLogger(newMockPodGetter(), nil, nil, nil, zap.NewNop())
	require.NotNil(t, h, "NewWithLogger should return a non-nil Handler")
}

// TestHandleNodeNameFromPodSpec verifies that when a Pod is in the cache and
// has Spec.NodeName set, handle() uses pod.Spec.NodeName as the event NodeName
// rather than parsing it from the UserInfo username.
func TestHandleNodeNameFromPodSpec(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-specnode",
			Namespace: "cube-system",
			Labels:    map[string]string{instanceTypeLabelKey: "cubebox"},
		},
		Spec: corev1.PodSpec{
			NodeName: "spec-node",
		},
	}

	stub := &stubReporter{}
	h := New(newMockPodGetter(pod), nil, stub)

	req := &admissionv1.AdmissionRequest{
		UID:       "uid-specnode",
		Name:      "sandbox-specnode",
		Namespace: "cube-system",
		// UserInfo deliberately uses a different node name to confirm Spec.NodeName wins.
		UserInfo: authenticationv1.UserInfo{Username: "system:node:userinfo-node"},
	}

	h.handle(req)

	require.Len(t, stub.events, 1)
	assert.Equal(t, "spec-node", stub.events[0].NodeName,
		"expected NodeName from pod.Spec.NodeName, not from UserInfo")
}

// TestServeHTTPAllowedResponseIncrementsTrueLabel verifies the `allowed="true"`
// Prometheus label path in ServeHTTP. An admin (non-kubelet) eviction is
// allowed, so response.Allowed == true triggers the "true" branch.
func TestServeHTTPAllowedResponseIncrementsTrueLabel(t *testing.T) {
	handler := New(newMockPodGetter(), nil, &stubReporter{})

	body, _ := json.Marshal(admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "uid-allowed-label",
			Name:      "sandbox-admin",
			Namespace: "cube-system",
			// Non-kubelet user → handler returns Allowed: true.
			UserInfo: authenticationv1.UserInfo{Username: "admin@example.com"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook/eviction", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var result admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.NotNil(t, result.Response)
	assert.True(t, result.Response.Allowed, "expected allowed=true for non-kubelet eviction")
}

// TestHandleStoreSaveError verifies that a failed store.Save does not prevent
// the event from being reported or the admission response from being returned.
func TestHandleStoreSaveError(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.ndjson")
	auditStore, err := store.New(auditPath)
	require.NoError(t, err)
	// Close the store immediately so Save will fail.
	auditStore.Close()

	stub := &stubReporter{}
	h := New(
		newMockPodGetter(newSandboxPod("sandbox-storefail", "cube-system", "cubebox")),
		auditStore,
		stub,
	)

	resp := h.handle(kubeletRequest("uid-storefail", "sandbox-storefail", "cube-system", "worker-storefail"))
	// The handler should still deny and report even though store.Save failed.
	assert.False(t, resp.Allowed)
	require.Len(t, stub.events, 1)
	assert.Equal(t, "uid-storefail", stub.events[0].EventID)
}
