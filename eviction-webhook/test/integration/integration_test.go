// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package integration provides integration tests for eviction-webhook using BDD scenarios.
package integration

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/admission"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/podinformer"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/store"
)

// TestIntegration_KubeletEvictionIsIntercepted verifies the core eviction interception flow.
//
// Scenario: kubelet eviction is intercepted and recovery initiated
// Given: A sandbox pod in running state
// When: kubelet sends an eviction request for the pod
// Then: webhook denies eviction and triggers recovery
func TestIntegration_KubeletEvictionIsIntercepted(t *testing.T) {
	// Given: Setup audit store
	auditPath := t.TempDir() + "/audit.ndjson"
	auditStore, err := store.New(auditPath)
	require.NoError(t, err)
	defer auditStore.Close()

	// Given: Create a pod getter with test pod
	podGetter := NewMockPodGetter(map[string]*corev1.Pod{
		"cube-system/sandbox-test-01": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-test-01",
				Namespace: "cube-system",
				Labels: map[string]string{
					"cube.master.instance.type": "cubebox",
				},
			},
		},
	})

	// Given: Create admission handler
	handler := admission.New(podGetter, auditStore, nil)

	// When: Send eviction request from kubelet
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       k8stypes.UID("test-uid-001"),
			Name:      "sandbox-test-01",
			Namespace: "cube-system",
			UserInfo: authenticationv1.UserInfo{
				Username: "system:node:worker-01",
			},
		},
	}

	body, _ := json.Marshal(review)
	req := httptest.NewRequest("POST", "/webhook/eviction", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Parse response
	var responseReview admissionv1.AdmissionReview
	json.NewDecoder(w.Body).Decode(&responseReview)

	// Then: Eviction should be denied
	require.NotNil(t, responseReview.Response, "response should not be nil")
	assert.False(t, responseReview.Response.Allowed, "eviction should be intercepted (denied)")
	assert.Contains(t, responseReview.Response.Result.Message, "intercepted")
}

// TestIntegration_AdminEvictionIsAllowed verifies that admin-initiated evictions pass through.
//
// Scenario: admin eviction is allowed to proceed
// Given: A sandbox pod in running state
// When: Admin initiates drain/eviction
// Then: webhook allows eviction to maintain cluster operability
func TestIntegration_AdminEvictionIsAllowed(t *testing.T) {
	auditPath := t.TempDir() + "/audit.ndjson"
	auditStore, err := store.New(auditPath)
	require.NoError(t, err)
	defer auditStore.Close()

	podGetter := NewMockPodGetter(map[string]*corev1.Pod{
		"cube-system/sandbox-admin": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-admin",
				Namespace: "cube-system",
				Labels: map[string]string{
					"cube.master.instance.type": "cubebox",
				},
			},
		},
	})

	handler := admission.New(podGetter, auditStore, nil)

	// When: Admin sends eviction request
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       k8stypes.UID("test-uid-admin"),
			Name:      "sandbox-admin",
			Namespace: "cube-system",
			UserInfo: authenticationv1.UserInfo{
				Username: "admin@example.com",  // Non-kubelet user
			},
		},
	}

	body, _ := json.Marshal(review)
	req := httptest.NewRequest("POST", "/webhook/eviction", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Parse response
	var responseReview admissionv1.AdmissionReview
	json.NewDecoder(w.Body).Decode(&responseReview)

	// Then: Admin eviction should be allowed
	require.NotNil(t, responseReview.Response)
	assert.True(t, responseReview.Response.Allowed, "admin eviction should be allowed")
}

// NewMockPodGetter creates a podinformer.Fake with test data.
func NewMockPodGetter(pods map[string]*corev1.Pod) *podinformer.Fake {
	return &podinformer.Fake{Pods: pods}
}


// TestIntegration_EvictionAuditLogPersisted verifies that the audit store persists the event.
//
// Scenario: eviction audit log is persisted
// Given: A handler with an audit store, and a sandbox pod in cache
// When: kubelet sends an eviction AdmissionReview request
// Then: webhook returns denied and the audit file contains the EventID
func TestIntegration_EvictionAuditLogPersisted(t *testing.T) {
	// Given: Setup audit store at a temp path
	auditPath := t.TempDir() + "/audit.ndjson"
	auditStore, err := store.New(auditPath)
	require.NoError(t, err)
	defer auditStore.Close()

	podGetter := NewMockPodGetter(map[string]*corev1.Pod{
		"cube-system/sandbox-audit-pod": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-audit-pod",
				Namespace: "cube-system",
				Labels: map[string]string{
					"cube.master.instance.type": "cubebox",
				},
			},
		},
	})

	handler := admission.New(podGetter, auditStore, nil)

	evictionUID := k8stypes.UID("audit-uid-999")
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       evictionUID,
			Name:      "sandbox-audit-pod",
			Namespace: "cube-system",
			UserInfo: authenticationv1.UserInfo{
				Username: "system:node:worker-77",
			},
		},
	}

	// When: send the eviction request through the full HTTP path
	body, _ := json.Marshal(review)
	req := httptest.NewRequest("POST", "/webhook/eviction", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Then: webhook denies the eviction
	var responseReview admissionv1.AdmissionReview
	json.NewDecoder(w.Body).Decode(&responseReview)

	require.NotNil(t, responseReview.Response)
	assert.False(t, responseReview.Response.Allowed, "kubelet eviction should be denied")

	// Flush the file by closing the store, then read its content
	require.NoError(t, auditStore.Close())

	rawBytes, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	require.NotEmpty(t, rawBytes, "audit file must not be empty")

	// Parse each NDJSON line and look for the matching EventID
	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(rawBytes), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event map[string]interface{}
		require.NoError(t, json.Unmarshal(line, &event), "each line must be valid JSON")
		if event["requestID"] == string(evictionUID) {
			found = true
			break
		}
	}
	assert.True(t, found, "audit file must contain a line with EventID == %s", evictionUID)
}

// TestIntegration_RecoveryManagerReceivesEvictionEvent verifies that the recovery manager
// is called with the correct event when a kubelet eviction is intercepted.
//
// Scenario: recovery manager is notified on kubelet eviction
// Given: A handler wired to an admission.FakeRecoveryManager
// When: kubelet sends an eviction request from worker-99
// Then: FakeRecoveryManager.OnEviction is called exactly once with NodeName == "worker-99"
func TestIntegration_RecoveryManagerReceivesEvictionEvent(t *testing.T) {
	// Given: audit store (required by New)
	auditPath := t.TempDir() + "/audit.ndjson"
	auditStore, err := store.New(auditPath)
	require.NoError(t, err)
	defer auditStore.Close()

	podGetter := NewMockPodGetter(map[string]*corev1.Pod{
		"cube-system/sandbox-recovery-pod": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-recovery-pod",
				Namespace: "cube-system",
				Labels: map[string]string{
					"cube.master.instance.type": "cubebox",
				},
			},
		},
	})

	stub := &admission.FakeRecoveryManager{}
	handler := admission.NewWithRecovery(podGetter, auditStore, nil, stub)

	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       k8stypes.UID("recovery-uid-001"),
			Name:      "sandbox-recovery-pod",
			Namespace: "cube-system",
			UserInfo: authenticationv1.UserInfo{
				Username: "system:node:worker-99",
			},
		},
	}

	// When: send the eviction request through the full HTTP path
	body, _ := json.Marshal(review)
	req := httptest.NewRequest("POST", "/webhook/eviction", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Then: eviction is denied and recovery manager received the event
	var responseReview admissionv1.AdmissionReview
	json.NewDecoder(w.Body).Decode(&responseReview)
	require.NotNil(t, responseReview.Response)
	assert.False(t, responseReview.Response.Allowed, "kubelet eviction should be denied")

	require.Len(t, stub.Events, 1, "OnEviction must be called exactly once")
	assert.Equal(t, "worker-99", stub.Events[0].NodeName, "NodeName must equal the kubelet node identity")
}

// TestIntegration_MultipleEvictionsDenied verifies that multiple independent eviction
// requests are each denied with their own UID.
//
// Scenario: multiple kubelet evictions are all denied
// Given: A single handler with two distinct sandbox pods
// When: two separate kubelet eviction requests are sent
// Then: both responses are denied and UIDs match the respective requests
func TestIntegration_MultipleEvictionsDenied(t *testing.T) {
	auditPath := t.TempDir() + "/audit.ndjson"
	auditStore, err := store.New(auditPath)
	require.NoError(t, err)
	defer auditStore.Close()

	podGetter := NewMockPodGetter(map[string]*corev1.Pod{
		"cube-system/sandbox-multi-01": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-multi-01",
				Namespace: "cube-system",
				Labels:    map[string]string{"cube.master.instance.type": "cubebox"},
			},
		},
		"cube-system/sandbox-multi-02": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-multi-02",
				Namespace: "cube-system",
				Labels:    map[string]string{"cube.master.instance.type": "cubebox"},
			},
		},
	})

	handler := admission.New(podGetter, auditStore, nil)

	type testCase struct {
		uid     k8stypes.UID
		podName string
	}
	cases := []testCase{
		{uid: "multi-uid-alpha", podName: "sandbox-multi-01"},
		{uid: "multi-uid-beta", podName: "sandbox-multi-02"},
	}

	for _, tc := range cases {
		review := admissionv1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "admission.k8s.io/v1",
				Kind:       "AdmissionReview",
			},
			Request: &admissionv1.AdmissionRequest{
				UID:       tc.uid,
				Name:      tc.podName,
				Namespace: "cube-system",
				UserInfo: authenticationv1.UserInfo{
					Username: "system:node:worker-multi",
				},
			},
		}

		// When: send eviction through the full HTTP path
		body, _ := json.Marshal(review)
		req := httptest.NewRequest("POST", "/webhook/eviction", bytes.NewReader(body))
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		// Then: each eviction must be denied with its own UID
		var responseReview admissionv1.AdmissionReview
		json.NewDecoder(w.Body).Decode(&responseReview)
		require.NotNil(t, responseReview.Response, "response must not be nil for pod %s", tc.podName)
		assert.False(t, responseReview.Response.Allowed, "kubelet eviction must be denied for pod %s", tc.podName)
		assert.Equal(t, tc.uid, responseReview.Response.UID, "response UID must match request UID for pod %s", tc.podName)
	}
}

