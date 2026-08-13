// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package admission implements the ValidatingWebhook handler for Pod eviction requests.
// Every matching kubelet eviction is denied (allowed: false) to protect the sandbox MicroVM.
// The eviction event is persisted locally, reported to CubeMaster, and handed to
// RecoveryManager which cordons the node and freezes the MicroVM via CubeMaster APIs.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/metrics"
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
	recoveryMgr     RecoveryManager
	logger          *zap.Logger
}

// New constructs a Handler with a no-op logger.
func New(podGetter PodGetter, s *store.Store, r EventReporter) *Handler {
	return &Handler{
		podGetter: podGetter,
		store:     s,
		reporter:  r,
		logger:    zap.NewNop(),
	}
}

// NewWithRecovery constructs a Handler wired to a RecoveryManager.
func NewWithRecovery(podGetter PodGetter, s *store.Store, r EventReporter, mgr RecoveryManager) *Handler {
	return &Handler{
		podGetter:   podGetter,
		store:       s,
		reporter:    r,
		recoveryMgr: mgr,
		logger:      zap.NewNop(),
	}
}

// NewWithLogger constructs a Handler with a structured logger.
func NewWithLogger(podGetter PodGetter, s *store.Store, r EventReporter, mgr RecoveryManager, logger *zap.Logger) *Handler {
	return &Handler{
		podGetter:   podGetter,
		store:       s,
		reporter:    r,
		recoveryMgr: mgr,
		logger:      logger,
	}
}

// SetPressureChecker wires a live Node MemoryPressure lookup into the admission path.
func (h *Handler) SetPressureChecker(checker PressureChecker) {
	h.pressureChecker = checker
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}

	response := h.handle(review.Request)
	review.Response = response

	allowed := "false"
	if response.Allowed {
		allowed = "true"
	}
	metrics.WebhookRequestsTotal.WithLabelValues("eviction", allowed).Inc()
	metrics.WebhookLatencySeconds.WithLabelValues(allowed).Observe(time.Since(start).Seconds())

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		h.logger.Error("encode response failed", zap.Error(err))
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

	logger := h.logger.With(
		zap.String("TraceID", string(req.UID)),
		zap.String("PodName", podName),
		zap.String("Namespace", namespace),
		zap.String("NodeName", nodeName),
	)

	event := &types.EvictionEvent{
		EventID:       string(req.UID),
		PodName:       podName,
		Namespace:     namespace,
		NodeName:      nodeName,
		InstanceType:  instanceType,
		InterceptedAt: interceptedAt,
	}
	dryRun := req.DryRun != nil && *req.DryRun

	// 1. Persist locally (audit trail).
	if !dryRun && h.store != nil {
		if err := h.store.Save(event); err != nil {
			logger.Error("audit store save failed", zap.Error(err))
		}
	}

	// 2. Report event to CubeMaster (async, non-blocking).
	if !dryRun && h.reporter != nil {
		h.reporter.Report(event)
	}

	// 3. Allow admin-initiated evictions (drain/evict) so maintenance workflows are not blocked.
	if !isNodeUser(req.UserInfo.Username) {
		logger.Info("non-kubelet eviction allowed",
			zap.String("Username", req.UserInfo.Username),
		)
		return allowResponse(string(req.UID), "non-kubelet eviction allowed by eviction-webhook")
	}

	// 4. Optional: only intercept when node is under actual MemoryPressure.
	if h.pressureChecker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		underPressure, err := h.pressureChecker(ctx, nodeName)
		cancel()
		if err != nil {
			logger.Warn("MemoryPressure check failed, allowing eviction", zap.Error(err))
			return allowResponse(string(req.UID), "MemoryPressure check failed; eviction allowed by eviction-webhook")
		}
		if !underPressure {
			logger.Info("no MemoryPressure, eviction allowed")
			return allowResponse(string(req.UID), "non-MemoryPressure eviction allowed by eviction-webhook")
		}
	}

	// 5. Intercept: trigger recovery.
	if !dryRun {
		metrics.EvictionInterceptedTotal.WithLabelValues(nodeName, instanceType, "kubelet").Inc()
	}
	if !dryRun && h.recoveryMgr != nil {
		h.recoveryMgr.OnEviction(event)
	}
	if dryRun {
		return denyResponse(string(req.UID), "dry-run: eviction would be intercepted by eviction-webhook; no recovery side effects were initiated")
	}

	logger.Info("eviction intercepted, recovery initiated",
		zap.String("InstanceType", instanceType),
	)
	return denyResponse(string(req.UID), "eviction intercepted by eviction-webhook; recovery initiated")
}

func (h *Handler) instanceType(namespace, podName string) string {
	pod, ok := h.podGetter.Get(namespace, podName)
	if !ok {
		h.logger.Warn("pod not in cache, instanceType empty",
			zap.String("Namespace", namespace),
			zap.String("PodName", podName),
		)
		return ""
	}
	return pod.Labels[instanceTypeLabelKey]
}

func (h *Handler) nodeName(namespace, podName, username string) string {
	if pod, ok := h.podGetter.Get(namespace, podName); ok && pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	resolved := nodeFromUserInfo(username)
	if resolved == username && !strings.HasPrefix(username, "system:node:") {
		h.logger.Warn("pod not in cache and username is not system:node identity",
			zap.String("Namespace", namespace),
			zap.String("PodName", podName),
			zap.String("Username", username),
		)
	}
	return resolved
}

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
		Result:  &metav1.Status{Message: message},
	}
}

func allowResponse(uid, message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:     k8stypes.UID(uid),
		Allowed: true,
		Result:  &metav1.Status{Message: message},
	}
}
