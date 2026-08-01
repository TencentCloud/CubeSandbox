// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

const (
	terminalGrantKindOpen   = "open"
	terminalGrantKindResume = "resume"
	terminalInstanceType    = "cubebox"
	terminalGrantBytes      = 16
	terminalPendingLimit    = 10
	terminalOrphanTimeout   = 90 * time.Second
	terminalGrantRetention  = time.Hour
	terminalMaintenanceTick = time.Minute
	terminalPublicWSPath    = "/opsapi/v1/terminal/ws"
)

// Stable browser-safe terminal errors. Causes remain server-side only.
type TerminalError struct {
	Status int
	Code   string
	Cause  error
}

func (e *TerminalError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

func (e *TerminalError) Unwrap() error { return e.Cause }

func newTerminalError(status int, code string, cause error) *TerminalError {
	return &TerminalError{Status: status, Code: code, Cause: cause}
}

// TerminalPrincipal is the small authorization surface deliberately kept
// separate from the platform's future RBAC implementation.
type TerminalPrincipal struct {
	UserID string
	Role   string
}

// CanOpenTerminal is the single policy hook for terminal access. The current
// platform only issues admin access tokens; future RBAC changes stay isolated
// to this function.
func CanOpenTerminal(principal TerminalPrincipal) bool {
	return principal.UserID != "" && principal.Role == "admin"
}

type TerminalGrantRequest struct {
	Kind        string `json:"kind"`
	SandboxID   string `json:"sandboxId"`
	ContainerID string `json:"containerId"`
	SessionID   string `json:"sessionId"`
	Cols        uint32 `json:"cols"`
	Rows        uint32 `json:"rows"`
	LastOffset  uint64 `json:"lastOffset"`
}

type TerminalContainer struct {
	ContainerID string `json:"containerId"`
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	Status      int32  `json:"status"`
}

type TerminalGrantResponse struct {
	Token       string              `json:"token"`
	WSURL       string              `json:"wsUrl"`
	SessionID   string              `json:"sessionId"`
	SandboxID   string              `json:"sandboxId"`
	ContainerID string              `json:"containerId"`
	ExpiresAt   time.Time           `json:"expiresAt"`
	Containers  []TerminalContainer `json:"containers"`
}

// ConsumedTerminalGrant contains only the persisted target binding. RawToken
// is intentionally absent so it cannot accidentally reach logs or audit rows.
type ConsumedTerminalGrant struct {
	ID           string
	Kind         string
	UserID       string
	SandboxID    string
	ContainerID  string
	SessionID    string
	Cols         uint32
	Rows         uint32
	ResumeOffset uint64
}

type TerminalStore interface {
	CreateTerminalGrant(ctx context.Context, grant *store.TerminalGrant, maxPending int) error
	ConsumeTerminalGrant(ctx context.Context, tokenHash string) (*store.TerminalGrant, error)
	CleanupTerminalGrants(ctx context.Context, before time.Time) (int64, error)
	GetTerminalSession(ctx context.Context, sessionID string) (*store.TerminalSession, error)
	CreateTerminalSession(ctx context.Context, session *store.TerminalSession, maxActive int, activeSince time.Time) error
	ResumeTerminalSession(ctx context.Context, sessionID, userID, sandboxID, containerID string, seenAt, activeSince time.Time) error
	TouchTerminalSession(ctx context.Context, sessionID string, seenAt time.Time, bytesIn, bytesOut int64) error
	CloseTerminalSession(ctx context.Context, sessionID string, closedAt time.Time, reason string, exitCode *int32, bytesIn, bytesOut int64) error
	ReconcileOrphanTerminalSessions(ctx context.Context, staleBefore, closedAt time.Time) (int64, error)
}

type TerminalCubeMasterClient interface {
	GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error)
}

var (
	_ TerminalStore            = (*store.Store)(nil)
	_ TerminalCubeMasterClient = (*cubemaster.Client)(nil)
)

type TerminalService struct {
	store TerminalStore
	cm    TerminalCubeMasterClient
	cfg   config.TerminalConfig
	now   func() time.Time
	mint  func() (string, error)
}

func NewTerminalService(s TerminalStore, cm TerminalCubeMasterClient, cfg config.TerminalConfig) *TerminalService {
	return &TerminalService{
		store: s,
		cm:    cm,
		cfg:   cfg,
		now:   func() time.Time { return time.Now().UTC() },
		mint:  mintTerminalGrant,
	}
}

func (s *TerminalService) IssueTerminalGrant(ctx context.Context, principal TerminalPrincipal, request TerminalGrantRequest) (*TerminalGrantResponse, *TerminalError) {
	if !s.cfg.Enabled || s.cfg.InternalToken == "" {
		return nil, newTerminalError(http.StatusServiceUnavailable, "INTERNAL", errors.New("terminal gateway is unavailable"))
	}
	if !CanOpenTerminal(principal) {
		return nil, newTerminalError(http.StatusForbidden, "FORBIDDEN", nil)
	}
	if len(principal.UserID) > 64 {
		return nil, newTerminalError(http.StatusForbidden, "FORBIDDEN", nil)
	}

	request.Kind = strings.ToLower(strings.TrimSpace(request.Kind))
	if request.Kind == "" {
		request.Kind = terminalGrantKindOpen
	}
	if request.Kind != terminalGrantKindOpen && request.Kind != terminalGrantKindResume {
		return nil, newTerminalError(http.StatusBadRequest, "PROTOCOL_ERROR", errors.New("unsupported terminal grant kind"))
	}
	if request.Cols == 0 || request.Rows == 0 || request.Cols > 1000 || request.Rows > 1000 {
		return nil, newTerminalError(http.StatusBadRequest, "PROTOCOL_ERROR", errors.New("terminal dimensions are out of range"))
	}
	if request.LastOffset > math.MaxInt64 {
		return nil, newTerminalError(http.StatusBadRequest, "PROTOCOL_ERROR", errors.New("resume offset exceeds the portable database range"))
	}

	now := s.now()
	sandboxID := strings.TrimSpace(request.SandboxID)
	containerID := strings.TrimSpace(request.ContainerID)
	sessionID := ""

	if request.Kind == terminalGrantKindOpen {
		if request.SessionID != "" || request.LastOffset != 0 || sandboxID == "" || len(sandboxID) > 64 || len(containerID) > 128 {
			return nil, newTerminalError(http.StatusBadRequest, "PROTOCOL_ERROR", errors.New("terminal target identifiers are invalid"))
		}
		sessionID = uuid.NewString()
	} else {
		if _, err := uuid.Parse(request.SessionID); err != nil {
			return nil, newTerminalError(http.StatusBadRequest, "PROTOCOL_ERROR", errors.New("terminal session id is invalid"))
		}
		if s.cfg.ReconnectGraceSeconds == 0 {
			return nil, newTerminalError(http.StatusConflict, "SESSION_LOST", nil)
		}
		previous, err := s.store.GetTerminalSession(ctx, request.SessionID)
		if err != nil || previous == nil || previous.ClosedAt.Valid {
			return nil, newTerminalError(http.StatusConflict, "SESSION_LOST", err)
		}
		if previous.UserID != principal.UserID {
			return nil, newTerminalError(http.StatusForbidden, "FORBIDDEN", nil)
		}
		if (sandboxID != "" && sandboxID != previous.SandboxID) ||
			(containerID != "" && containerID != previous.ContainerID) {
			return nil, newTerminalError(http.StatusForbidden, "FORBIDDEN", nil)
		}
		if now.Sub(previous.LastSeenAt) > time.Duration(s.cfg.ReconnectGraceSeconds)*time.Second {
			return nil, newTerminalError(http.StatusConflict, "SESSION_LOST", nil)
		}
		sandboxID = previous.SandboxID
		containerID = previous.ContainerID
		sessionID = previous.ID
	}

	target, terminalErr := s.resolveTerminalTarget(ctx, sandboxID, containerID)
	if terminalErr != nil {
		return nil, terminalErr
	}

	rawGrant, err := s.mint()
	if err != nil {
		return nil, newTerminalError(http.StatusInternalServerError, "INTERNAL", err)
	}
	digest := sha256.Sum256([]byte(rawGrant))
	expiresAt := now.Add(time.Duration(s.cfg.GrantTTLSeconds) * time.Second)
	grant := &store.TerminalGrant{
		ID:           uuid.NewString(),
		TokenHash:    hex.EncodeToString(digest[:]),
		Kind:         request.Kind,
		UserID:       principal.UserID,
		SandboxID:    target.SandboxID,
		ContainerID:  target.ContainerID,
		SessionID:    sessionID,
		Cols:         request.Cols,
		Rows:         request.Rows,
		ResumeOffset: int64(request.LastOffset),
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
	}
	if err := s.store.CreateTerminalGrant(ctx, grant, terminalPendingLimit); err != nil {
		if errors.Is(err, store.ErrTerminalGrantLimit) {
			return nil, newTerminalError(http.StatusTooManyRequests, "LIMIT_EXCEEDED", err)
		}
		return nil, newTerminalError(http.StatusInternalServerError, "INTERNAL", err)
	}

	return &TerminalGrantResponse{
		Token:       rawGrant,
		WSURL:       terminalPublicWSPath,
		SessionID:   sessionID,
		SandboxID:   target.SandboxID,
		ContainerID: target.ContainerID,
		ExpiresAt:   expiresAt,
		Containers:  target.Containers,
	}, nil
}

func (s *TerminalService) ConsumeTerminalGrant(ctx context.Context, rawGrant string) (*ConsumedTerminalGrant, *TerminalError) {
	decoded, err := base64.RawURLEncoding.DecodeString(rawGrant)
	if err != nil || len(decoded) != terminalGrantBytes {
		return nil, newTerminalError(http.StatusUnauthorized, "GRANT_INVALID", nil)
	}
	digest := sha256.Sum256([]byte(rawGrant))
	persisted, err := s.store.ConsumeTerminalGrant(ctx, hex.EncodeToString(digest[:]))
	if err != nil {
		if errors.Is(err, store.ErrTerminalGrantInvalid) {
			return nil, newTerminalError(http.StatusUnauthorized, "GRANT_INVALID", nil)
		}
		return nil, newTerminalError(http.StatusInternalServerError, "INTERNAL", err)
	}
	if persisted.Kind != terminalGrantKindOpen && persisted.Kind != terminalGrantKindResume {
		return nil, newTerminalError(http.StatusUnauthorized, "GRANT_INVALID", errors.New("persisted grant kind is invalid"))
	}
	if _, err := uuid.Parse(persisted.SessionID); err != nil || persisted.ResumeOffset < 0 {
		return nil, newTerminalError(http.StatusUnauthorized, "GRANT_INVALID", errors.New("persisted grant binding is invalid"))
	}
	return &ConsumedTerminalGrant{
		ID:           persisted.ID,
		Kind:         persisted.Kind,
		UserID:       persisted.UserID,
		SandboxID:    persisted.SandboxID,
		ContainerID:  persisted.ContainerID,
		SessionID:    persisted.SessionID,
		Cols:         persisted.Cols,
		Rows:         persisted.Rows,
		ResumeOffset: uint64(persisted.ResumeOffset),
	}, nil
}

// PrepareTerminalSession performs a second authoritative target check at grant
// consumption time, then writes the session audit row before the Master relay
// is allowed to open a PTY.
func (s *TerminalService) PrepareTerminalSession(ctx context.Context, grant *ConsumedTerminalGrant) *TerminalError {
	if grant == nil {
		return newTerminalError(http.StatusUnauthorized, "GRANT_INVALID", nil)
	}
	target, terminalErr := s.resolveTerminalTarget(ctx, grant.SandboxID, grant.ContainerID)
	if terminalErr != nil {
		return terminalErr
	}
	if target.SandboxID != grant.SandboxID || target.ContainerID != grant.ContainerID {
		return newTerminalError(http.StatusUnauthorized, "GRANT_INVALID", errors.New("terminal target binding changed"))
	}

	now := s.now()
	if grant.Kind == terminalGrantKindOpen {
		session := &store.TerminalSession{
			ID:          grant.SessionID,
			UserID:      grant.UserID,
			SandboxID:   grant.SandboxID,
			ContainerID: grant.ContainerID,
			CubeletHost: target.CubeletHost,
			OpenedAt:    now,
			LastSeenAt:  now,
		}
		if err := s.store.CreateTerminalSession(ctx, session, s.cfg.MaxSessionsPerUser, now.Add(-terminalOrphanTimeout)); err != nil {
			if errors.Is(err, store.ErrTerminalSessionLimit) {
				return newTerminalError(http.StatusTooManyRequests, "LIMIT_EXCEEDED", err)
			}
			return newTerminalError(http.StatusInternalServerError, "INTERNAL", err)
		}
		return nil
	}

	activeSince := now.Add(-time.Duration(s.cfg.ReconnectGraceSeconds) * time.Second)
	if err := s.store.ResumeTerminalSession(ctx, grant.SessionID, grant.UserID, grant.SandboxID, grant.ContainerID, now, activeSince); err != nil {
		if errors.Is(err, store.ErrTerminalSessionLost) {
			return newTerminalError(http.StatusConflict, "SESSION_LOST", err)
		}
		return newTerminalError(http.StatusInternalServerError, "INTERNAL", err)
	}
	return nil
}

func (s *TerminalService) TouchTerminalSession(ctx context.Context, sessionID string, bytesIn, bytesOut int64) error {
	return s.store.TouchTerminalSession(ctx, sessionID, s.now(), bytesIn, bytesOut)
}

func (s *TerminalService) CloseTerminalSession(ctx context.Context, sessionID, reason string, exitCode *int32, bytesIn, bytesOut int64) error {
	if reason == "" || len(reason) > 32 {
		reason = "INTERNAL"
	}
	return s.store.CloseTerminalSession(ctx, sessionID, s.now(), reason, exitCode, bytesIn, bytesOut)
}

func (s *TerminalService) RunTerminalMaintenance(ctx context.Context) {
	ticker := time.NewTicker(terminalMaintenanceTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maintainTerminalState(ctx)
		}
	}
}

func (s *TerminalService) maintainTerminalState(ctx context.Context) {
	now := s.now()
	if count, err := s.store.CleanupTerminalGrants(ctx, now.Add(-terminalGrantRetention)); err != nil {
		slog.Warn("terminal grant cleanup failed", "error", err)
	} else if count > 0 {
		slog.Info("terminal grant cleanup completed", "rows", count)
	}
	if count, err := s.store.ReconcileOrphanTerminalSessions(ctx, now.Add(-terminalOrphanTimeout), now); err != nil {
		slog.Warn("terminal session reconciliation failed", "error", err)
	} else if count > 0 {
		slog.Info("terminal session reconciliation completed", "rows", count)
	}
}

type terminalTarget struct {
	SandboxID   string
	ContainerID string
	CubeletHost string
	Containers  []TerminalContainer
}

type terminalCMSandboxEnvelope struct {
	Ret *struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret"`
	Data []struct {
		SandboxID  string `json:"sandbox_id"`
		Status     int32  `json:"status"`
		HostID     string `json:"host_id"`
		Containers []struct {
			Name        string `json:"name"`
			ContainerID string `json:"container_id"`
			Status      int32  `json:"status"`
			Type        string `json:"type"`
		} `json:"containers"`
	} `json:"data"`
}

func (s *TerminalService) resolveTerminalTarget(ctx context.Context, sandboxID, requestedContainerID string) (*terminalTarget, *TerminalError) {
	raw, err := s.cm.GetSandbox(ctx, sandboxID, terminalInstanceType)
	if err != nil {
		var cmErr *cubemaster.CMError
		if errors.As(err, &cmErr) {
			switch {
			case cmErr.IsNotFound():
				return nil, newTerminalError(http.StatusNotFound, "TARGET_NOT_FOUND", err)
			case cmErr.IsPausing() || cmErr.IsConflict():
				return nil, newTerminalError(http.StatusConflict, "TARGET_NOT_RUNNING", err)
			}
		}
		return nil, newTerminalError(http.StatusBadGateway, "INTERNAL", err)
	}

	var response terminalCMSandboxEnvelope
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, newTerminalError(http.StatusBadGateway, "INTERNAL", err)
	}
	if response.Ret != nil && response.Ret.RetCode != 0 && response.Ret.RetCode != 200 {
		if response.Ret.RetCode == 130404 || response.Ret.RetCode == 404 {
			return nil, newTerminalError(http.StatusNotFound, "TARGET_NOT_FOUND", errors.New("cubemaster target not found"))
		}
		if response.Ret.RetCode == 130490 || response.Ret.RetCode == 130409 || response.Ret.RetCode == 409 {
			return nil, newTerminalError(http.StatusConflict, "TARGET_NOT_RUNNING", errors.New("cubemaster target is not running"))
		}
		return nil, newTerminalError(http.StatusBadGateway, "INTERNAL", errors.New("cubemaster returned a terminal target error"))
	}
	if len(response.Data) != 1 {
		return nil, newTerminalError(http.StatusNotFound, "TARGET_NOT_FOUND", nil)
	}
	sandbox := response.Data[0]
	if sandbox.SandboxID == "" || len(sandbox.SandboxID) > 64 || sandbox.Status != 1 {
		if sandbox.Status != 1 && sandbox.SandboxID != "" {
			return nil, newTerminalError(http.StatusConflict, "TARGET_NOT_RUNNING", nil)
		}
		return nil, newTerminalError(http.StatusNotFound, "TARGET_NOT_FOUND", nil)
	}

	containers := make([]TerminalContainer, 0, len(sandbox.Containers))
	selected := -1
	for i, container := range sandbox.Containers {
		containers = append(containers, TerminalContainer{
			ContainerID: container.ContainerID,
			Name:        container.Name,
			Type:        container.Type,
			Status:      container.Status,
		})
		if requestedContainerID != "" && container.ContainerID == requestedContainerID {
			selected = i
		} else if requestedContainerID == "" && selected == -1 &&
			(container.Type == "sandbox" || container.ContainerID == sandbox.SandboxID) {
			selected = i
		}
	}
	if requestedContainerID == "" && selected == -1 && len(sandbox.Containers) > 0 {
		selected = 0
	}
	if selected < 0 {
		return nil, newTerminalError(http.StatusNotFound, "TARGET_NOT_FOUND", nil)
	}
	container := sandbox.Containers[selected]
	if container.ContainerID == "" || len(container.ContainerID) > 128 {
		return nil, newTerminalError(http.StatusNotFound, "TARGET_NOT_FOUND", nil)
	}
	if container.Status != 1 {
		return nil, newTerminalError(http.StatusConflict, "TARGET_NOT_RUNNING", nil)
	}
	hostID := strings.TrimSpace(sandbox.HostID)
	if len(hostID) > 64 {
		hostID = ""
	}
	return &terminalTarget{
		SandboxID:   sandbox.SandboxID,
		ContainerID: container.ContainerID,
		CubeletHost: hostID,
		Containers:  containers,
	}, nil
}

func mintTerminalGrant() (string, error) {
	raw := make([]byte, terminalGrantBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate terminal grant: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
