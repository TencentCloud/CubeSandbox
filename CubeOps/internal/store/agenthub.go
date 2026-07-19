// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
)

// AgentInstance matches the old Rust AgentInstanceResponse exactly.
type AgentInstance struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Status            string            `json:"status"`
	Engine            string            `json:"engine"`
	Env               string            `json:"env"`
	Model             string            `json:"model"`
	Version           string            `json:"version"`
	Bots              []string          `json:"bots"`
	BotsAvailable     []string          `json:"botsAvailable"`
	Avatar            string            `json:"avatar"`
	AvatarTone        string            `json:"avatarTone"`
	SandboxID         string            `json:"sandboxId"`
	TemplateID        string            `json:"templateId"`
	GatewayURL        string            `json:"gatewayUrl"`
	EnvURL            string            `json:"envUrl"`
	PersistenceMode   *string           `json:"persistenceMode,omitempty"`
	RootfsSourceType  *string           `json:"rootfsSourceType,omitempty"`
	RootfsSourceID    *string           `json:"rootfsSourceId,omitempty"`
	OpenclawPersistID *string           `json:"openclawPersistId,omitempty"`
	OpenclawStatePath *string           `json:"openclawStatePath,omitempty"`
	WecomConfig       *AgentWecomConfig `json:"wecomConfig,omitempty"`
	Setup             *AgentSetupResult `json:"setup,omitempty"`

	// Internal fields (not serialized to JSON)
	BaseSnapshotID string `json:"-"`
	Domain         string `json:"-"`
	GatewayToken   string `json:"-"`
}

type AgentWecomConfig struct {
	BotID     string `json:"botId"`
	BotSecret string `json:"botSecret"`
}

type AgentSetupResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

const instanceColumns = `agent_id, sandbox_id, template_id, name, engine, env, model, version, status,
       bots, avatar, avatar_tone, domain, gateway_port, env_port, gateway_token,
       persistence_mode, rootfs_source_type, rootfs_source_id,
       openclaw_persist_id, openclaw_state_path,
       wecom_bot_id, wecom_bot_secret,
       last_error, setup_exit_code, base_snapshot_id`

// Pagination defaults for list endpoints. These exist to prevent unbounded
// full-table scans as t_agenthub_instance and t_agenthub_template grow
// (review-bot flag: "No LIMIT clause — unbounded full-table scan").
//
// DefaultListLimit is used when the caller passes 0 (e.g. legacy callers
// or omitted query params). MaxListLimit is a hard cap to prevent an
// adversarial client from pulling millions of rows in a single request.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// UpsertInstance inserts or updates an agent instance record.
// Matches the old Rust upsert_instance SQL exactly.
func (s *Store) UpsertInstance(ctx context.Context, inst *AgentInstance) error {
	botsJSON := "[]"
	if len(inst.Bots) > 0 {
		b, _ := json.Marshal(inst.Bots)
		botsJSON = string(b)
	}

	var wecomBotID, wecomBotSecret *string
	if inst.WecomConfig != nil {
		wecomBotID = &inst.WecomConfig.BotID
		enc, err := crypto.EncryptSecret(inst.WecomConfig.BotSecret)
		if err == nil {
			wecomBotSecret = &enc
		}
	}

	var gatewayToken *string
	if inst.GatewayToken != "" {
		gatewayToken = &inst.GatewayToken
	}

	var setupExitCode *int
	var lastError *string
	if inst.Setup != nil {
		setupExitCode = &inst.Setup.ExitCode
		if inst.Setup.Stderr != "" {
			lastError = &inst.Setup.Stderr
		}
	}

	// Parse envPort from inst.EnvURL (format: http://{port}-{sandboxId}.{domain}).
	// Falls back to 8080 when parsing fails, matching the default for all-in-one images.
	envPort := 8080
	if inst.EnvURL != "" {
		if u, err := url.Parse(inst.EnvURL); err == nil {
			if match := regexp.MustCompile(`^(\d+)-`).FindStringSubmatch(u.Hostname()); match != nil {
				if p, err := strconv.Atoi(match[1]); err == nil {
					envPort = p
				}
			}
		}
	}

	return s.db.WithContext(ctx).Exec(
		upsertInstanceSQL(),
		inst.ID, inst.SandboxID, inst.TemplateID, inst.Name, inst.Engine, inst.Env,
		inst.Model, inst.Version, inst.Status,
		botsJSON, inst.Avatar, inst.AvatarTone, inst.Domain,
		18789, envPort, gatewayToken,
		inst.PersistenceMode, inst.RootfsSourceType, inst.RootfsSourceID,
		inst.OpenclawPersistID, inst.OpenclawStatePath,
		wecomBotID, wecomBotSecret,
		lastError, setupExitCode,
	).Error
}

// ListInstances returns a page of non-deleted agent instances, ordered by
// created_at DESC, id DESC. limit must be > 0; offset must be >= 0.
// Callers should cap limit to avoid OOM on large tables — see
// DefaultListLimit / MaxListLimit.
func (s *Store) ListInstances(ctx context.Context, limit, offset int) ([]AgentInstance, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT `+instanceColumns+` FROM t_agenthub_instance WHERE deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		limit, offset,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()
	return scanInstances(rows)
}

// GetInstance returns a single agent instance by ID.
func (s *Store) GetInstance(ctx context.Context, agentID string) (*AgentInstance, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT `+instanceColumns+` FROM t_agenthub_instance WHERE agent_id = ? AND deleted_at IS NULL LIMIT 1`,
		agentID,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	defer rows.Close()
	instances, err := scanInstances(rows)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, nil
	}
	return &instances[0], nil
}

// SoftDeleteInstance marks an instance as deleted.
func (s *Store) SoftDeleteInstance(ctx context.Context, agentID string) error {
	result := s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_instance SET deleted_at = NOW(), status = 'deleted' WHERE agent_id = ? AND deleted_at IS NULL`,
		agentID,
	)
	if result.Error != nil {
		return fmt.Errorf("soft delete instance: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errors.New("instance not found")
	}
	return nil
}

// UpdateInstanceStatus updates the status of an agent instance.
func (s *Store) UpdateInstanceStatus(ctx context.Context, agentID, status string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_instance SET status = ?, updated_at = NOW() WHERE agent_id = ? AND deleted_at IS NULL`,
		status, agentID,
	).Error
}

// UpdateInstanceModel updates the model of an agent instance.
func (s *Store) UpdateInstanceModel(ctx context.Context, agentID, model string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_instance SET model = ?, updated_at = NOW() WHERE agent_id = ? AND deleted_at IS NULL`,
		model, agentID,
	).Error
}

// GetAgentWecomConfig returns the WeCom bot config for an agent.
func (s *Store) GetAgentWecomConfig(ctx context.Context, agentID string) (botID, botSecret string, err error) {
	row := s.db.WithContext(ctx).Raw(
		`SELECT wecom_bot_id, wecom_bot_secret FROM t_agenthub_instance WHERE agent_id = ? AND deleted_at IS NULL LIMIT 1`,
		agentID,
	).Row()
	var botIDStr sql.NullString
	var botSecretStr sql.NullString
	if err = row.Scan(&botIDStr, &botSecretStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("get wecom config: %w", err)
	}
	botID = botIDStr.String
	if botSecretStr.Valid && botSecretStr.String != "" {
		botSecret = crypto.DecryptOrPassthrough(botSecretStr.String)
	}
	return botID, botSecret, nil
}

// UpdateAgentWecomConfig updates the WeCom bot config for an agent.
func (s *Store) UpdateAgentWecomConfig(ctx context.Context, agentID, botID, botSecret string) error {
	var encryptedSecret *string
	if botSecret != "" {
		enc, err := crypto.EncryptSecret(botSecret)
		if err != nil {
			return fmt.Errorf("encrypt bot secret: %w", err)
		}
		encryptedSecret = &enc
	}
	return s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_instance SET wecom_bot_id = ?, wecom_bot_secret = ?, updated_at = NOW() WHERE agent_id = ? AND deleted_at IS NULL`,
		botID, encryptedSecret, agentID,
	).Error
}

// AgentSnapshot matches the old Rust AgentSnapshotResponse.
type AgentSnapshot struct {
	SnapshotID                string   `json:"snapshotID"`
	Names                     []string `json:"names"`
	Status                    string   `json:"status"`
	SnapshotKind              *string  `json:"snapshotKind,omitempty"`
	OriginSandboxID           *string  `json:"originSandboxID,omitempty"`
	PublishedTemplate         *string  `json:"publishedTemplateId,omitempty"`
	RootfsSourceType          *string  `json:"rootfsSourceType,omitempty"`
	RootfsSourceID            *string  `json:"rootfsSourceId,omitempty"`
	RootfsSnapshotID          *string  `json:"rootfsSnapshotId,omitempty"`
	OpenclawStateSnapshotPath *string  `json:"openclawStateSnapshotPath,omitempty"`
	TemplateRef               bool     `json:"templateReferenced"`
	IsHealthy                 bool     `json:"isHealthy"`
	ParentSnapshotID          *string  `json:"parentSnapshotID,omitempty"`
	CreatedAt                 *string  `json:"createdAt,omitempty"`
	UpdatedAt                 *string  `json:"updatedAt,omitempty"`
}

// GetAgentSnapshot returns a single snapshot by agent_id and snapshot_id.
// Matches old Rust get_snapshot.
func (s *Store) GetAgentSnapshot(ctx context.Context, agentID, snapshotID string) (*AgentSnapshot, error) {
	row := s.db.WithContext(ctx).Raw(
		`SELECT s.snapshot_id, s.name, s.status, s.snapshot_kind, s.origin_sandbox_id,
		        s.published_template_id, s.rootfs_source_type, s.rootfs_source_id,
		        s.rootfs_snapshot_id, s.openclaw_state_snapshot_path,
		        s.parent_snapshot_id, s.is_healthy,
		        t.template_id IS NOT NULL AS template_referenced,
		        `+formatTimestamp("s.created_at")+` AS created_at,
		        `+formatTimestamp("s.updated_at")+` AS updated_at
		 FROM t_agenthub_snapshot s
		 LEFT JOIN t_agenthub_template t ON t.source_snapshot_id = s.snapshot_id AND t.deleted_at IS NULL
		 WHERE s.agent_id = ? AND s.snapshot_id = ? AND s.deleted_at IS NULL
		 LIMIT 1`,
		agentID, snapshotID,
	).Row()
	var snap AgentSnapshot
	var name, kind, originSB, pubTmpl, rfsType, rfsID, rfsSnapID, ocStatePath, parentSnap, created, updated sql.NullString
	var tmplRef sql.NullBool
	var isHealthy sql.NullBool
	if err := row.Scan(&snap.SnapshotID, &name, &snap.Status, &kind, &originSB,
		&pubTmpl, &rfsType, &rfsID, &rfsSnapID, &ocStatePath,
		&parentSnap, &isHealthy, &tmplRef, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get snapshot: %w", err)
	}
	snap.Names = []string{}
	if name.Valid && name.String != "" {
		snap.Names = []string{name.String}
	}
	snap.SnapshotKind = nullStringPtr(kind)
	snap.OriginSandboxID = nullStringPtr(originSB)
	snap.PublishedTemplate = nullStringPtr(pubTmpl)
	snap.RootfsSourceType = nullStringPtr(rfsType)
	snap.RootfsSourceID = nullStringPtr(rfsID)
	snap.RootfsSnapshotID = nullStringPtr(rfsSnapID)
	snap.OpenclawStateSnapshotPath = nullStringPtr(ocStatePath)
	snap.ParentSnapshotID = nullStringPtr(parentSnap)
	snap.CreatedAt = nullStringPtr(created)
	snap.UpdatedAt = nullStringPtr(updated)
	snap.TemplateRef = tmplRef.Valid && tmplRef.Bool
	snap.IsHealthy = isHealthy.Valid && isHealthy.Bool
	return &snap, nil
}

// ListAgentSnapshots returns snapshots for an agent instance.
func (s *Store) ListAgentSnapshots(ctx context.Context, agentID string) ([]AgentSnapshot, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT s.snapshot_id, s.name, s.status, s.snapshot_kind, s.origin_sandbox_id,
		        s.published_template_id, s.rootfs_source_type, s.rootfs_source_id,
		        s.rootfs_snapshot_id, s.openclaw_state_snapshot_path,
		        s.parent_snapshot_id, s.is_healthy,
		        t.template_id IS NOT NULL AS template_referenced,
		        `+formatTimestamp("s.created_at")+` AS created_at,
		        `+formatTimestamp("s.updated_at")+` AS updated_at
		 FROM t_agenthub_snapshot s
		 LEFT JOIN t_agenthub_template t ON t.source_snapshot_id = s.snapshot_id AND t.deleted_at IS NULL
		 WHERE s.agent_id = ? AND s.deleted_at IS NULL
		 ORDER BY s.created_at DESC, s.id DESC`,
		agentID,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list agent snapshots: %w", err)
	}
	defer rows.Close()

	snapshots := []AgentSnapshot{}
	for rows.Next() {
		var snap AgentSnapshot
		var name, kind, originSB, pubTmpl, rfsType, rfsID, rfsSnapID, ocStatePath, parentSnap, created, updated sql.NullString
		var tmplRef sql.NullBool
		var isHealthy sql.NullBool
		if err := rows.Scan(&snap.SnapshotID, &name, &snap.Status, &kind, &originSB,
			&pubTmpl, &rfsType, &rfsID, &rfsSnapID, &ocStatePath,
			&parentSnap, &isHealthy, &tmplRef,
			&created, &updated); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		// Convert single name to names array (matching old Rust mapping)
		snap.Names = []string{}
		if name.Valid && name.String != "" {
			snap.Names = []string{name.String}
		}
		snap.SnapshotKind = nullStringPtr(kind)
		snap.OriginSandboxID = nullStringPtr(originSB)
		snap.PublishedTemplate = nullStringPtr(pubTmpl)
		snap.RootfsSourceType = nullStringPtr(rfsType)
		snap.RootfsSourceID = nullStringPtr(rfsID)
		snap.RootfsSnapshotID = nullStringPtr(rfsSnapID)
		snap.OpenclawStateSnapshotPath = nullStringPtr(ocStatePath)
		snap.ParentSnapshotID = nullStringPtr(parentSnap)
		snap.CreatedAt = nullStringPtr(created)
		snap.UpdatedAt = nullStringPtr(updated)
		snap.TemplateRef = tmplRef.Valid && tmplRef.Bool
		snap.IsHealthy = isHealthy.Valid && isHealthy.Bool
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

// DeleteAgentSnapshot soft-deletes a snapshot.
func (s *Store) DeleteAgentSnapshot(ctx context.Context, agentID, snapshotID string) error {
	result := s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_snapshot SET deleted_at = NOW() WHERE snapshot_id = ? AND agent_id = ? AND deleted_at IS NULL`,
		snapshotID, agentID,
	)
	if result.Error != nil {
		return fmt.Errorf("delete snapshot: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errors.New("snapshot not found")
	}
	return nil
}

// AgentOperation matches the old Rust AgentOperationResponse.
type AgentOperation struct {
	OperationID   string  `json:"operationId"`
	AgentID       string  `json:"agentId"`
	OperationType string  `json:"operationType"`
	Status        string  `json:"status"`
	TargetID      *string `json:"targetId,omitempty"`
	ErrorMessage  *string `json:"errorMessage,omitempty"`
	CreatedAt     *string `json:"createdAt,omitempty"`
	UpdatedAt     *string `json:"updatedAt,omitempty"`
}

// ListAgentOperations returns operations for an agent instance.
func (s *Store) ListAgentOperations(ctx context.Context, agentID string) ([]AgentOperation, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT operation_id, agent_id, operation_type, status, target_id, error_message,
		        `+formatTimestamp("created_at")+` AS created_at,
		        `+formatTimestamp("updated_at")+` AS updated_at
		 FROM t_agenthub_operation
		 WHERE agent_id = ?
		 ORDER BY id DESC
		 LIMIT 100`,
		agentID,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	defer rows.Close()

	ops := []AgentOperation{}
	for rows.Next() {
		var op AgentOperation
		var targetID, errMsg, created, updated sql.NullString
		if err := rows.Scan(&op.OperationID, &op.AgentID, &op.OperationType, &op.Status, &targetID, &errMsg, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan operation: %w", err)
		}
		op.TargetID = nullStringPtr(targetID)
		op.ErrorMessage = nullStringPtr(errMsg)
		op.CreatedAt = nullStringPtr(created)
		op.UpdatedAt = nullStringPtr(updated)
		ops = append(ops, op)
	}
	return ops, nil
}

// AgentTemplate matches the old Rust AgentTemplateResponse exactly.
type AgentTemplate struct {
	TemplateID       string  `json:"templateId"`
	Name             string  `json:"name"`
	SourceAgentID    string  `json:"sourceAgentId"`
	SourceSnapshotID string  `json:"sourceSnapshotId"`
	SourceSandboxID  string  `json:"sourceSandboxId"`
	Model            string  `json:"model"`
	Version          string  `json:"version"`
	PersistenceMode  *string `json:"persistenceMode,omitempty"`
	Recommended      bool    `json:"recommended"`
	CreatedAt        *string `json:"createdAt"`
}

// ListAgentTemplates returns a page of non-deleted agent templates,
// ordered by created_at DESC, id DESC. limit must be > 0; offset must be
// >= 0. See DefaultListLimit / MaxListLimit for caller guidance.
func (s *Store) ListAgentTemplates(ctx context.Context, limit, offset int) ([]AgentTemplate, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT template_id, name, source_agent_id, source_snapshot_id,
		        source_sandbox_id, model, version, persistence_mode,
		        recommended, created_at
		 FROM t_agenthub_template
		 WHERE deleted_at IS NULL
		 ORDER BY created_at DESC, id DESC
		 LIMIT ? OFFSET ?`,
		limit, offset,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()

	templates := []AgentTemplate{}
	for rows.Next() {
		var tmpl AgentTemplate
		var persistenceMode, created sql.NullString
		if err := rows.Scan(&tmpl.TemplateID, &tmpl.Name, &tmpl.SourceAgentID,
			&tmpl.SourceSnapshotID, &tmpl.SourceSandboxID, &tmpl.Model, &tmpl.Version,
			&persistenceMode, &tmpl.Recommended, &created); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		tmpl.PersistenceMode = nullStringPtr(persistenceMode)
		tmpl.CreatedAt = nullStringPtr(created)
		templates = append(templates, tmpl)
	}
	return templates, nil
}

// GetAgentTemplate fetches a single agent template by ID.
func (s *Store) GetAgentTemplate(ctx context.Context, templateID string) (*AgentTemplate, error) {
	row := s.db.WithContext(ctx).Raw(
		`SELECT template_id, name, source_agent_id, source_snapshot_id,
		        source_sandbox_id, model, version, persistence_mode,
		        recommended, created_at
		 FROM t_agenthub_template
		 WHERE template_id = ? AND deleted_at IS NULL`,
		templateID,
	).Row()
	var tmpl AgentTemplate
	var persistenceMode, created sql.NullString
	if err := row.Scan(
		&tmpl.TemplateID, &tmpl.Name, &tmpl.SourceAgentID, &tmpl.SourceSnapshotID,
		&tmpl.SourceSandboxID, &tmpl.Model, &tmpl.Version, &persistenceMode,
		&tmpl.Recommended, &created,
	); err != nil {
		return nil, nil // not found
	}
	tmpl.PersistenceMode = nullStringPtr(persistenceMode)
	tmpl.CreatedAt = nullStringPtr(created)
	return &tmpl, nil
}

// DeleteAgentTemplate soft-deletes a template.
//
// This is the explicit AgentHub delete (DELETE /agenthub/templates/{id}).
// It soft-deletes the t_agenthub_template row AND nulls out
// published_template_id on any snapshots that referenced it — matching the
// old Rust soft_delete_template (db.rs:851). The snapshot cleanup was
// previously missing in CubeOps, leaving dangling published_template_id
// values after a template delete.
func (s *Store) DeleteAgentTemplate(ctx context.Context, templateID string) error {
	result := s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_template SET deleted_at = NOW() WHERE template_id = ? AND deleted_at IS NULL`,
		templateID,
	)
	if result.Error != nil {
		return fmt.Errorf("delete template: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errors.New("template not found")
	}
	// Clear published_template_id on snapshots referencing this template.
	// Best-effort: a failure here is logged but does not unwind the template
	// delete that already succeeded.
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_snapshot SET published_template_id = NULL WHERE published_template_id = ?`,
		templateID,
	).Error; err != nil {
		return fmt.Errorf("clear snapshot published_template_id: %w", err)
	}
	return nil
}

// FindTemplateIDsByInfraID returns the template_ids of all live (non-deleted)
// AgentHub template registrations whose template_id OR source_snapshot_id
// matches the given infra template/snapshot id.
//
// This is the Go equivalent of the old Rust
// find_template_ids_by_template_or_source_snapshot (db.rs:788). It backs the
// reverse-sync that runs after an infrastructure template/snapshot is
// deleted via the E2B / SDK path, so that AgentHub registrations pointing at
// the just-deleted infra resource are cleaned up rather than left dangling.
func (s *Store) FindTemplateIDsByInfraID(ctx context.Context, infraID string) ([]string, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT template_id FROM t_agenthub_template
		 WHERE deleted_at IS NULL
		   AND (template_id = ? OR source_snapshot_id = ?)`,
		infraID, infraID,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("find template ids by infra id: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan template_id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SoftDeleteAgentHubTemplate soft-deletes a single AgentHub template
// registration by template_id and clears published_template_id on any
// snapshots that referenced it. Used by the reverse-sync path; unlike
// DeleteAgentTemplate it is best-effort (RowsAffected==0 is not an error)
// because the reverse-sync iterates over possibly-stale query results.
//
// This is the Go equivalent of the old Rust soft_delete_template
// (db.rs:851), minus the "not found" error that DeleteAgentTemplate
// surfaces for the explicit-delete API.
func (s *Store) SoftDeleteAgentHubTemplate(ctx context.Context, templateID string) error {
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_template SET deleted_at = NOW() WHERE template_id = ? AND deleted_at IS NULL`,
		templateID,
	).Error; err != nil {
		return fmt.Errorf("soft-delete template: %w", err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_snapshot SET published_template_id = NULL WHERE published_template_id = ?`,
		templateID,
	).Error; err != nil {
		return fmt.Errorf("clear snapshot published_template_id: %w", err)
	}
	return nil
}

// RecordOperation inserts an operation record.
func (s *Store) RecordOperation(ctx context.Context, agentID, sandboxID, operationType, status, errMsg string) error {
	return s.db.WithContext(ctx).Exec(
		`INSERT INTO t_agenthub_operation (operation_id, agent_id, sandbox_id, operation_type, status, error_message)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		fmt.Sprintf("op-%s", uuid.New().String()), agentID, sandboxID, operationType, status, errMsg,
	).Error
}

// LatestHealthySnapshot returns the most recent snapshot marked as healthy
// for the given agent. Matches old Rust latest_healthy_snapshot.
func (s *Store) LatestHealthySnapshot(ctx context.Context, agentID string) (string, error) {
	var snapshotID string
	err := s.db.WithContext(ctx).Raw(
		`SELECT snapshot_id FROM t_agenthub_snapshot
		 WHERE agent_id = ? AND is_healthy = 1 AND deleted_at IS NULL
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		agentID,
	).Row().Scan(&snapshotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("latest healthy snapshot: %w", err)
	}
	return snapshotID, nil
}

// SetBaseSnapshotID updates the base_snapshot_id for an agent instance.
// Matches old Rust set_base_snapshot_id.
func (s *Store) SetBaseSnapshotID(ctx context.Context, agentID, snapshotID string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE t_agenthub_instance SET base_snapshot_id = ?, updated_at = NOW() WHERE agent_id = ? AND deleted_at IS NULL`,
		snapshotID, agentID,
	).Error
}

// --- helpers ---

func scanInstances(rows *sql.Rows) ([]AgentInstance, error) {
	instances := []AgentInstance{}
	for rows.Next() {
		var inst AgentInstance
		var gatewayToken, persistenceMode, rootfsSourceType, rootfsSourceID,
			openclawPersistID, openclawStatePath, wecomBotID, wecomBotSecret,
			lastError, baseSnapshotID sql.NullString
		var setupExitCode sql.NullInt64
		var botsVal sql.NullString
		var domain string
		var gatewayPort, envPort int
		if err := rows.Scan(
			&inst.ID, &inst.SandboxID, &inst.TemplateID, &inst.Name, &inst.Engine,
			&inst.Env, &inst.Model, &inst.Version, &inst.Status,
			&botsVal, &inst.Avatar, &inst.AvatarTone, &domain, &gatewayPort, &envPort, &gatewayToken,
			&persistenceMode, &rootfsSourceType, &rootfsSourceID,
			&openclawPersistID, &openclawStatePath,
			&wecomBotID, &wecomBotSecret,
			&lastError, &setupExitCode, &baseSnapshotID,
		); err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}

		// Parse bots JSON array
		inst.Bots = []string{}
		if botsVal.Valid && botsVal.String != "" {
			_ = json.Unmarshal([]byte(botsVal.String), &inst.Bots)
		}

		// Compute botsAvailable = ["wecom"] minus active bots
		inst.BotsAvailable = []string{}
		for _, b := range []string{"wecom"} {
			found := false
			for _, active := range inst.Bots {
				if active == b {
					found = true
					break
				}
			}
			if !found {
				inst.BotsAvailable = append(inst.BotsAvailable, b)
			}
		}

		// Compute gatewayUrl and envUrl (same logic as old Rust code)
		gatewayURL := fmt.Sprintf("https://%d-%s.%s", gatewayPort, inst.SandboxID, domain)
		if gatewayToken.Valid && gatewayToken.String != "" {
			gatewayURL = gatewayURL + "#token=" + gatewayToken.String
		}
		inst.GatewayURL = gatewayURL
		inst.EnvURL = fmt.Sprintf("http://%d-%s.%s", envPort, inst.SandboxID, domain)

		// Optional fields
		inst.PersistenceMode = nullStringPtr(persistenceMode)
		inst.RootfsSourceType = nullStringPtr(rootfsSourceType)
		inst.RootfsSourceID = nullStringPtr(rootfsSourceID)
		inst.OpenclawPersistID = nullStringPtr(openclawPersistID)
		inst.OpenclawStatePath = nullStringPtr(openclawStatePath)

		// WeCom config
		if wecomBotID.Valid && wecomBotID.String != "" {
			secret := ""
			if wecomBotSecret.Valid && wecomBotSecret.String != "" {
				secret = crypto.DecryptOrPassthrough(wecomBotSecret.String)
			}
			inst.WecomConfig = &AgentWecomConfig{
				BotID:     wecomBotID.String,
				BotSecret: secret,
			}
		}

		// Setup result
		if setupExitCode.Valid {
			stderr := ""
			if lastError.Valid {
				stderr = lastError.String
			}
			inst.Setup = &AgentSetupResult{
				ExitCode: int(setupExitCode.Int64),
				Stdout:   "",
				Stderr:   stderr,
			}
		}

		// Internal fields
		if baseSnapshotID.Valid {
			inst.BaseSnapshotID = baseSnapshotID.String
		}
		inst.Domain = domain

		instances = append(instances, inst)
	}
	return instances, nil
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	s := n.String
	return &s
}
