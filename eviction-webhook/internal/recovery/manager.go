// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package recovery orchestrates the full pressure-relief lifecycle:
//
//  1. OnEviction — called by the admission handler immediately after an
//     eviction is intercepted. Records the sandbox, cordons the node via
//     CubeMaster, and freezes the MicroVM via CubeMaster.
//
//  2. OnPressureRelief — called by the node watcher when a node's
//     MemoryPressure condition transitions True → False. Resumes every
//     previously-paused sandbox on that node and uncordons it.
//
// All CubeMaster calls are fire-and-forget goroutines so they never block
// the admission path.
package recovery

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/metrics"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

const (
	apiEvictionReliefDelay = 5 * time.Minute
	reliefRetryDelay       = 30 * time.Second
	protectionRetryDelay   = 30 * time.Second
	instanceTypeLabel      = "cube.master.instance.type"
)

// cubeMasterIface abstracts the CubeMaster client for testability.
type cubeMasterIface interface {
	IsolateNode(ctx context.Context, nodeID string) error
	UnisolateNode(ctx context.Context, nodeID string) error
	PauseSandbox(ctx context.Context, sandboxID, instanceType, requestID string) error
	ResumeSandbox(ctx context.Context, sandboxID, instanceType, requestID string) error
	ListSandboxesByNode(ctx context.Context, hostID string) ([]cubemaster.SandboxBrief, error)
	ResolveHostID(ctx context.Context, identifier string) (string, error)
}

// PressureChecker returns whether nodeName is currently under MemoryPressure.
type PressureChecker func(ctx context.Context, nodeName string) (bool, error)

// PausedSandbox records a sandbox that was paused due to node memory pressure.
type PausedSandbox struct {
	SandboxID    string
	InstanceType string
	EventID      string // original AdmissionReview UID, used as requestID
}

// Manager tracks per-node isolation and paused-sandbox state, then drives
// CubeMaster to cordon/uncordon nodes and pause/resume MicroVMs.
type Manager struct {
	cm      *cubemaster.Client
	cmIface cubeMasterIface // used when non-nil (tests override this)

	mu sync.Mutex
	// paused maps nodeName → list of sandboxes that were paused because of that node's pressure.
	paused map[string][]PausedSandbox
	// isolated maps K8s node names to the resolved CubeMaster node IDs that
	// were isolated. Keeping both avoids mixing node identifiers across
	// isolate/list/unisolate operations.
	isolated map[string]string
	// evictionInFlight deduplicates concurrent recovery work for the same node.
	evictionInFlight map[string]bool
	reliefInFlight   map[string]bool
	// desiredProtected is the latest desired state from pressure events. It
	// prevents a relief event that arrives during pause from being lost.
	desiredProtected map[string]bool
	pressureChecker  PressureChecker
	retryDelay       time.Duration
	// persister saves state to disk for restart recovery.
	persister *persister
}

// New creates a Manager backed by the given CubeMaster client.
func New(cm *cubemaster.Client) *Manager {
	return &Manager{
		cm:               cm,
		paused:           make(map[string][]PausedSandbox),
		isolated:         make(map[string]string),
		evictionInFlight: make(map[string]bool),
		reliefInFlight:   make(map[string]bool),
		desiredProtected: make(map[string]bool),
	}
}

// NewWithPersister creates a Manager that persists state to the given path
// for recovery after restart.
func NewWithPersister(cm *cubemaster.Client, statePath string) (*Manager, error) {
	p := newPersister(statePath)
	state, err := p.load()
	if err != nil {
		return nil, fmt.Errorf("load recovery state: %w", err)
	}

	m := &Manager{
		cm:               cm,
		paused:           state.Paused,
		isolated:         state.Isolated,
		evictionInFlight: make(map[string]bool),
		reliefInFlight:   make(map[string]bool),
		desiredProtected: state.DesiredProtected,
		persister:        p,
	}

	if len(m.paused) > 0 || len(m.isolated) > 0 {
		log.Printf("[recovery] restored state: %d isolated nodes, %d nodes with paused sandboxes",
			len(m.isolated), len(m.paused))
	}

	return m, nil
}

// SetPressureChecker wires a live Kubernetes Node pressure lookup into the
// manager for startup reconciliation and delayed API-eviction relief.
func (m *Manager) SetPressureChecker(checker PressureChecker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pressureChecker = checker
}

// client returns the active CubeMaster interface (test override takes precedence).
func (m *Manager) client() cubeMasterIface {
	if m.cmIface != nil {
		return m.cmIface
	}
	return m.cm
}

// persist saves the current state to disk (best-effort).
func (m *Manager) persist() {
	if m.persister == nil {
		return
	}
	m.mu.Lock()
	state := persistState{
		Version:          currentPersistVersion,
		Paused:           clonePaused(m.paused),
		Isolated:         cloneIsolated(m.isolated),
		DesiredProtected: cloneDesired(m.desiredProtected),
	}
	m.mu.Unlock()
	if err := m.persister.save(state); err != nil {
		log.Printf("[recovery] persist state failed: %v", err)
	}
}

func cloneDesired(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for node, desired := range src {
		dst[node] = desired
	}
	return dst
}

func clonePaused(src map[string][]PausedSandbox) map[string][]PausedSandbox {
	dst := make(map[string][]PausedSandbox, len(src))
	for node, sandboxes := range src {
		dst[node] = append([]PausedSandbox{}, sandboxes...)
	}
	return dst
}

func cloneIsolated(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for node, cubeMasterNodeID := range src {
		dst[node] = cubeMasterNodeID
	}
	return dst
}

// OnPressureDetected is called by the node watcher when MemoryPressure
// transitions False → True. This handles the case where kubelet's internal
// eviction bypasses the API server (and thus the webhook never sees an
// Eviction request). We proactively cordon the node and pause sandboxes.
func (m *Manager) OnPressureDetected(nodeName string) {
	m.mu.Lock()
	if m.desiredProtected == nil {
		m.desiredProtected = make(map[string]bool)
	}
	m.desiredProtected[nodeName] = true
	m.mu.Unlock()
	m.persist()

	log.Printf("[recovery] pressure detected node=%s (proactive isolation)", nodeName)
	// Use empty instanceType — will default to "cubebox".
	if m.startEviction(nodeName, "pressure-detected-"+nodeName, "") {
		// Use the same delayed safety reconciliation as the admission path so
		// a missed watcher relief transition cannot strand paused sandboxes.
		m.scheduleAPIEvictionRelief(nodeName)
	}
}

// OnEviction is called by the admission handler on each intercepted eviction.
// It cordons the node and pauses ALL sandboxes on that node, since the entire
// node is under memory pressure. All CubeMaster calls are asynchronous.
func (m *Manager) OnEviction(event *types.EvictionEvent) {
	m.mu.Lock()
	if m.desiredProtected == nil {
		m.desiredProtected = make(map[string]bool)
	}
	m.desiredProtected[event.NodeName] = true
	m.mu.Unlock()
	m.persist()
	if m.startEviction(event.NodeName, event.EventID, event.InstanceType) {
		m.scheduleAPIEvictionRelief(event.NodeName)
	}
}

// ReconcileRestored converges state loaded from disk with the current cluster
// state on startup. If pressure already cleared while the webhook was down, it
// resumes/uncordons immediately; if pressure still exists, it reasserts node
// isolation so CubeMaster and local state stay aligned.
func (m *Manager) ReconcileRestored(ctx context.Context) {
	m.mu.Lock()
	nodes := make(map[string]string, len(m.isolated)+len(m.paused))
	for nodeName, cubeMasterNodeID := range m.isolated {
		nodes[nodeName] = cubeMasterNodeID
	}
	for nodeName := range m.paused {
		if _, ok := nodes[nodeName]; !ok {
			nodes[nodeName] = ""
		}
	}
	checker := m.pressureChecker
	m.mu.Unlock()

	if len(nodes) == 0 {
		return
	}
	if checker == nil {
		log.Printf("[recovery] restored state found but no pressure checker configured; skipping startup reconciliation")
		return
	}

	for nodeName, cubeMasterNodeID := range nodes {
		underPressure, err := checker(ctx, nodeName)
		if err != nil {
			log.Printf("[recovery] startup reconciliation skipped node=%s err=%v", nodeName, err)
			continue
		}
		if !underPressure {
			log.Printf("[recovery] startup reconciliation relieving restored node=%s", nodeName)
			m.OnPressureRelief(nodeName)
			continue
		}
		if cubeMasterNodeID != "" {
			go func(nodeName, cubeMasterNodeID string) {
				reassertCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := m.client().IsolateNode(reassertCtx, cubeMasterNodeID); err != nil {
					log.Printf("[recovery] startup reconciliation re-isolate failed node=%s cubeMasterNodeID=%s err=%v", nodeName, cubeMasterNodeID, err)
					m.scheduleProtectionRetry(nodeName, "startup-reconcile-"+nodeName, "")
					return
				}
				log.Printf("[recovery] startup reconciliation reasserted isolation node=%s cubeMasterNodeID=%s", nodeName, cubeMasterNodeID)
			}(nodeName, cubeMasterNodeID)
		}
		m.OnPressureDetected(nodeName)
	}
}

func (m *Manager) startEviction(nodeName, eventID, instanceType string) bool {
	m.mu.Lock()
	if m.evictionInFlight == nil {
		m.evictionInFlight = make(map[string]bool)
	}
	if m.reliefInFlight[nodeName] {
		m.mu.Unlock()
		log.Printf("[recovery] relief in flight node=%s, protection will reconcile after relief", nodeName)
		return false
	}
	if m.evictionInFlight[nodeName] {
		m.mu.Unlock()
		log.Printf("[recovery] eviction already in flight node=%s, skipping duplicate trigger", nodeName)
		return false
	}
	m.evictionInFlight[nodeName] = true
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.evictionInFlight, nodeName)
			desired := m.desiredProtected[nodeName]
			m.mu.Unlock()
			if !desired {
				m.reconcileRelief(nodeName)
			}
		}()
		m.applyEviction(nodeName, eventID, instanceType)
	}()
	return true
}

func (m *Manager) applyEviction(nodeName, eventID, instanceType string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cubeMasterNodeID, err := m.client().ResolveHostID(ctx, nodeName)
	if err != nil {
		log.Printf("[recovery] ResolveHostID failed node=%s err=%v", nodeName, err)
		m.scheduleProtectionRetry(nodeName, eventID, instanceType)
		return
	}

	m.mu.Lock()
	isolatedNodeID, alreadyIsolated := m.isolated[nodeName]
	m.mu.Unlock()
	if alreadyIsolated {
		cubeMasterNodeID = isolatedNodeID
	}

	// 1. Cordon the node (idempotent). Set isolated=true only after success.
	if !alreadyIsolated {
		if err := m.client().IsolateNode(ctx, cubeMasterNodeID); err != nil {
			log.Printf("[recovery] IsolateNode failed node=%s cubeMasterNodeID=%s err=%v", nodeName, cubeMasterNodeID, err)
			metrics.CubeMasterAPIErrorsTotal.WithLabelValues("IsolateNode", "error").Inc()
			m.scheduleProtectionRetry(nodeName, eventID, instanceType)
			// Continue — still try to pause sandboxes.
		} else {
			metrics.IsolatedNodesTotal.Inc()
			m.mu.Lock()
			m.isolated[nodeName] = cubeMasterNodeID
			m.mu.Unlock()
			m.persist()
		}
	}

	// 2. List all sandboxes on the node to discover real sandbox IDs.
	sandboxes, err := m.client().ListSandboxesByNode(ctx, cubeMasterNodeID)
	if err != nil {
		log.Printf("[recovery] ListSandboxesByNode failed node=%s err=%v", nodeName, err)
		m.scheduleProtectionRetry(nodeName, eventID, instanceType)
		return
	}
	if len(sandboxes) == 0 {
		log.Printf("[recovery] no sandboxes found on node=%s, nothing to pause", nodeName)
		return
	}

	// 3. Pause each sandbox and record it for later resumption.
	var paused []PausedSandbox
	needsRetry := false
	for _, sb := range sandboxes {
		if m.isSandboxPaused(nodeName, sb.SandboxID) {
			log.Printf("[recovery] sandbox %s already paused for node=%s, skipping", sb.SandboxID, nodeName)
			continue
		}
		it := sandboxInstanceType(sb, instanceType)
		requestID := fmt.Sprintf("eviction-pause-%s", eventID)
		if err := m.client().PauseSandbox(ctx, sb.SandboxID, it, requestID); err != nil {
			log.Printf("[recovery] PauseSandbox failed sandboxID=%s err=%v", sb.SandboxID, err)
			needsRetry = true
			continue // try the rest
		}
		ps := PausedSandbox{
			SandboxID:    sb.SandboxID,
			InstanceType: it,
			EventID:      eventID,
		}
		if m.recordPaused(nodeName, ps) {
			paused = append(paused, ps)
		}
	}

	if len(paused) > 0 {
		m.persist()
	}
	if needsRetry {
		m.scheduleProtectionRetry(nodeName, eventID, instanceType)
	}

	log.Printf("[recovery] eviction applied node=%s paused=%d/%d", nodeName, len(paused), len(sandboxes))
}

func sandboxInstanceType(sb cubemaster.SandboxBrief, eventInstanceType string) string {
	if instanceType := sb.Labels[instanceTypeLabel]; instanceType != "" {
		return instanceType
	}
	if eventInstanceType != "" {
		return eventInstanceType
	}
	// Backward compatibility for old sandboxes that predate the instance type
	// label and for events whose Pod metadata is unavailable.
	return "cubebox"
}

func (m *Manager) isSandboxPaused(nodeName, sandboxID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sandboxPausedLocked(m.paused[nodeName], sandboxID)
}

func (m *Manager) recordPaused(nodeName string, ps PausedSandbox) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sandboxPausedLocked(m.paused[nodeName], ps.SandboxID) {
		return false
	}
	m.paused[nodeName] = append(m.paused[nodeName], ps)
	return true
}

func sandboxPausedLocked(sandboxes []PausedSandbox, sandboxID string) bool {
	for _, ps := range sandboxes {
		if ps.SandboxID == sandboxID {
			return true
		}
	}
	return false
}

// OnPressureRelief is called by the node watcher when MemoryPressure clears.
// It resumes all paused sandboxes on the node and uncordons it.
func (m *Manager) OnPressureRelief(nodeName string) {
	m.mu.Lock()
	if m.desiredProtected == nil {
		m.desiredProtected = make(map[string]bool)
	}
	m.desiredProtected[nodeName] = false
	inFlight := m.evictionInFlight[nodeName]
	m.mu.Unlock()
	m.persist()
	if inFlight {
		log.Printf("[recovery] pressure relief recorded while protection is in flight node=%s", nodeName)
		return
	}
	m.reconcileRelief(nodeName)
}

func (m *Manager) reconcileRelief(nodeName string) {
	m.mu.Lock()
	sandboxes := append([]PausedSandbox{}, m.paused[nodeName]...)
	isolatedNodeID := m.isolated[nodeName]
	wasIsolated := isolatedNodeID != ""
	checker := m.pressureChecker
	if m.desiredProtected[nodeName] || m.evictionInFlight[nodeName] || m.reliefInFlight[nodeName] {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	if len(sandboxes) == 0 && !wasIsolated {
		m.mu.Lock()
		delete(m.desiredProtected, nodeName)
		m.mu.Unlock()
		m.persist()
		return
	}
	if checker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		underPressure, err := checker(ctx, nodeName)
		cancel()
		if err != nil {
			log.Printf("[recovery] pressure relief check failed node=%s err=%v; retrying", nodeName, err)
			m.scheduleReliefRetry(nodeName)
			return
		}
		if underPressure {
			log.Printf("[recovery] pressure relief deferred node=%s: resource pressure still true", nodeName)
			m.scheduleReliefRetry(nodeName)
			return
		}
	}

	log.Printf("[recovery] pressure relieved node=%s resuming %d sandboxes", nodeName, len(sandboxes))
	m.mu.Lock()
	if m.reliefInFlight == nil {
		m.reliefInFlight = make(map[string]bool)
	}
	if m.reliefInFlight[nodeName] {
		m.mu.Unlock()
		return
	}
	m.reliefInFlight[nodeName] = true
	m.mu.Unlock()
	go func() {
		m.applyRelief(nodeName, isolatedNodeID, sandboxes, wasIsolated)
		m.mu.Lock()
		delete(m.reliefInFlight, nodeName)
		desired := m.desiredProtected[nodeName]
		m.mu.Unlock()
		if desired {
			m.startEviction(nodeName, "pressure-redetected-"+nodeName, "")
		}
	}()
}

func (m *Manager) applyRelief(nodeName, isolatedNodeID string, sandboxes []PausedSandbox, wasIsolated bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Resume each paused sandbox.
	resumed := make(map[string]bool, len(sandboxes))
	start := time.Now()
	for _, ps := range sandboxes {
		instanceType := ps.InstanceType
		if instanceType == "" {
			instanceType = "cubebox"
		}
		requestID := fmt.Sprintf("eviction-resume-%s", ps.EventID)
		if err := m.client().ResumeSandbox(ctx, ps.SandboxID, instanceType, requestID); err != nil {
			log.Printf("[recovery] ResumeSandbox failed sandboxID=%s err=%v", ps.SandboxID, err)
			metrics.CubeMasterAPIErrorsTotal.WithLabelValues("ResumeSandbox", "error").Inc()
			continue
		}
		resumed[ps.SandboxID] = true
	}
	if len(resumed) > 0 {
		metrics.RecoveryDurationSeconds.WithLabelValues(nodeName, "success").Observe(time.Since(start).Seconds())
	}

	// Uncordon the node so new sandboxes can be scheduled again.
	allResumed := len(resumed) == len(sandboxes)
	unisolated := !wasIsolated
	m.mu.Lock()
	stillRelieved := !m.desiredProtected[nodeName]
	m.mu.Unlock()
	if wasIsolated && allResumed && stillRelieved {
		if err := m.client().UnisolateNode(ctx, isolatedNodeID); err != nil {
			log.Printf("[recovery] UnisolateNode failed node=%s cubeMasterNodeID=%s err=%v", nodeName, isolatedNodeID, err)
			metrics.CubeMasterAPIErrorsTotal.WithLabelValues("UnisolateNode", "error").Inc()
		} else {
			unisolated = true
		}
	}

	m.mu.Lock()
	remaining := m.paused[nodeName][:0]
	for _, ps := range m.paused[nodeName] {
		if !resumed[ps.SandboxID] {
			remaining = append(remaining, ps)
		}
	}
	if len(remaining) == 0 {
		delete(m.paused, nodeName)
	} else {
		m.paused[nodeName] = remaining
	}
	if unisolated {
		delete(m.isolated, nodeName)
	}
	if len(remaining) == 0 && unisolated {
		delete(m.desiredProtected, nodeName)
	}
	m.mu.Unlock()
	m.persist()

	if len(remaining) > 0 || !unisolated {
		m.scheduleReliefRetry(nodeName)
	}
}

func (m *Manager) scheduleReliefRetry(nodeName string) {
	go func() {
		time.Sleep(m.effectiveRetryDelay(reliefRetryDelay))
		m.reconcileRelief(nodeName)
	}()
}

func (m *Manager) scheduleProtectionRetry(nodeName, eventID, instanceType string) {
	go func() {
		time.Sleep(m.effectiveRetryDelay(protectionRetryDelay))
		m.mu.Lock()
		desired := m.desiredProtected[nodeName]
		m.mu.Unlock()
		if desired {
			m.startEviction(nodeName, eventID, instanceType)
		} else {
			m.reconcileRelief(nodeName)
		}
	}()
}

func (m *Manager) effectiveRetryDelay(fallback time.Duration) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retryDelay > 0 {
		return m.retryDelay
	}
	return fallback
}

func (m *Manager) scheduleAPIEvictionRelief(nodeName string) {
	go func() {
		time.Sleep(apiEvictionReliefDelay)
		m.mu.Lock()
		checker := m.pressureChecker
		m.mu.Unlock()
		if checker == nil {
			log.Printf("[recovery] delayed API eviction relief skipped node=%s: no pressure checker configured", nodeName)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		underPressure, err := checker(ctx, nodeName)
		if err != nil {
			log.Printf("[recovery] delayed API eviction relief skipped node=%s err=%v", nodeName, err)
			return
		}
		if underPressure {
			log.Printf("[recovery] delayed API eviction relief deferred node=%s: MemoryPressure still true", nodeName)
			return
		}
		m.OnPressureRelief(nodeName)
	}()
}

// IsolatedNodes returns a snapshot of currently isolated nodes (for diagnostics).
func (m *Manager) IsolatedNodes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.isolated))
	for n := range m.isolated {
		out = append(out, n)
	}
	return out
}
