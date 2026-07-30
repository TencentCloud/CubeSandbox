// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package admission implements the ValidatingWebhook handler for Pod eviction
// requests. Every matching request is denied (allowed: false) so that kubelet
// cannot destroy the sandbox MicroVM. The eviction event is persisted locally,
// optionally reported to CubeMaster, and handed to the RecoveryManager which
// immediately cordons the node and freezes the sandbox MicroVM via CubeMaster APIs.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/store"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

const instanceTypeLabelKey = "cube.master.instance.type"

// PodGetter abstracts Pod lookup so Handler can be tested without a real Kubernetes cluster.
type PodGetter interface {
	Get(namespace, name string) (*corev1.Pod, bool)
}

// EventReporter abstracts the reporter so Handler can be tested without a real CubeMaster.
type EventReporter interface {
	Report(event *types.EvictionEvent) <-chan struct{}
}

// RecoveryManager abstracts the recovery.Manager for testability.
type RecoveryManager interface {
	OnEviction(event *types.EvictionEvent)
}

// PressureChecker returns whether nodeName is currently under MemoryPressure.
type PressureChecker func(ctx context.Context, nodeName string) (bool, error)

// Handler handles /webhook/eviction HTTP requests.
type Handler struct {
	podGetter       PodGetter
	store           *store.Store
	reporter        EventReporter
	pressureChecker PressureChecker
	// recoveryMgr may be nil when running without full CubeMaster integration (tests).
	recoveryMgr RecoveryManager
}

// New constructs a Handler. recoveryMgr may be nil.
func New(podGetter PodGetter, s *store.Store, r EventReporter) *Handler {
	return &Handler{podGetter: podGetter, store: s, reporter: r}
}

// NewWithRecovery constructs a Handler wired to a RecoveryManager.
func NewWithRecovery(podGetter PodGetter, s *store.Store, r EventReporter, mgr RecoveryManager) *Handler {
	return &Handler{podGetter: podGetter, store: s, reporter: r, recoveryMgr: mgr}
}

// SetPressureChecker wires a live Node MemoryPressure lookup into the admission
// path. When configured, kubelet evictions are denied only while the node is
// actually under MemoryPressure; DiskPressure/PIDPressure evictions pass through.
func (h *Handler) SetPressureChecker(checker PressureChecker) {
	h.pressureChecker = checker
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}

	response := h.handle(review.Request)
	review.Response = response

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		log.Printf("[eviction-admission] encode response: %v", err)
	}
}

// handle is the pure decision function, separated for testability.
func (h *Handler) handle(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req == nil {
		return denyResponse("", "empty admission request")
	}

	podName := req.Name
	namespace := req.Namespace
	nodeName := h.nodeName(namespace, podName, req.UserInfo.Username)
	instanceType := h.instanceType(namespace, podName)
	interceptedAt := time.Now().UTC().Format(time.RFC3339)

	event := &types.EvictionEvent{
		EventID:       string(req.UID),
		PodName:       podName,
		Namespace:     namespace,
		NodeName:      nodeName,
		InstanceType:  instanceType,
		InterceptedAt: interceptedAt,
	}

	// 1. Persist locally (audit trail).
	if h.store != nil {
		if err := h.store.Save(event); err != nil {
			log.Printf("[eviction-admission] store.Save eventID=%s: %v", event.EventID, err)
		}
	}

	// 2. Report event to CubeMaster (async, non-blocking).
	if h.reporter != nil {
		h.reporter.Report(event)
	}

	// 3. Only kubelet-shaped evictions are treated as pressure recovery signals.
	// Admin-initiated evictions such as drain/evict are allowed so maintenance
	// workflows do not hang behind a recovery path that will never run.
	if !isNodeUser(req.UserInfo.Username) {
		return allowResponse(string(req.UID), "non-kubelet eviction allowed by eviction-webhook")
	}
	if h.pressureChecker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		underPressure, err := h.pressureChecker(ctx, nodeName)
		cancel()
		if err != nil {
			log.Printf("[eviction-admission] MemoryPressure check failed node=%s err=%v; allowing eviction", nodeName, err)
			return allowResponse(string(req.UID), "MemoryPressure check failed; eviction allowed by eviction-webhook")
		}
		if !underPressure {
			return allowResponse(string(req.UID), "non-MemoryPressure eviction allowed by eviction-webhook")
		}
	}

	if h.recoveryMgr != nil {
		h.recoveryMgr.OnEviction(event)
	}

	return denyResponse(string(req.UID), "eviction intercepted by eviction-webhook; recovery initiated")
}

// instanceType looks up the cube.master.instance.type label from the Pod cache.
// Returns empty string when the Pod is not in cache — the eviction is still denied.
func (h *Handler) instanceType(namespace, podName string) string {
	pod, ok := h.podGetter.Get(namespace, podName)
	if !ok {
		log.Printf("[eviction-admission] pod %s/%s not in cache, instanceType will be empty", namespace, podName)
		return ""
	}
	return pod.Labels[instanceTypeLabelKey]
}

// nodeName resolves the K8s node name for the evicted pod. Resolution order:
//  1. pod.Spec.NodeName from the pod cache (most reliable)
//  2. Extract from kubelet service-account username "system:node:<name>"
//  3. Fall back to the raw username (best-effort; logged as warning)
func (h *Handler) nodeName(namespace, podName, username string) string {
	if pod, ok := h.podGetter.Get(namespace, podName); ok && pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	resolved := nodeFromUserInfo(username)
	if resolved == username && !strings.HasPrefix(username, "system:node:") {
		log.Printf("[eviction-admission] pod %s/%s not in cache, username %q is not a system:node identity — node resolution may be incorrect",
			namespace, podName, username)
	}
	return resolved
}

// nodeFromUserInfo extracts the node name from the kubelet service-account
// username format "system:node:<nodeName>".
func nodeFromUserInfo(username string) string {
	const prefix = "system:node:"
	if strings.HasPrefix(username, prefix) {
		return strings.TrimPrefix(username, prefix)
	}
	return username
}

func isNodeUser(username string) bool {
	return strings.HasPrefix(username, "system:node:")
}

func denyResponse(uid, message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:     k8stypes.UID(uid),
		Allowed: false,
		Result: &metav1.Status{
			Message: message,
		},
	}
}

func allowResponse(uid, message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:     k8stypes.UID(uid),
		Allowed: true,
		Result: &metav1.Status{
			Message: message,
		},
	}
}
