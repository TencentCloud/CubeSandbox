// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"gorm.io/gorm"
)

const (
	ArtifactStatusPending  = "PENDING"
	ArtifactStatusBuilding = "BUILDING"
	ArtifactStatusReady    = "READY"
	ArtifactStatusFailed   = "FAILED"

	JobStatusPending = "PENDING"
	JobStatusRunning = "RUNNING"
	JobStatusReady   = "READY"
	JobStatusFailed  = "FAILED"

	JobOperationCreate = "CREATE"
	JobOperationRedo   = "REDO"

	RedoModeAll         = "ALL"
	RedoModeNodes       = "NODES"
	RedoModeFailedOnly  = "FAILED_ONLY"
	RedoModeFailedNodes = "FAILED_NODES"

	JobPhasePulling          = "PULLING"
	JobPhaseUnpacking        = "UNPACKING"
	JobPhaseBuildingExt4     = "BUILDING_EXT4"
	JobPhaseGeneratingJSON   = "GENERATING_JSON"
	JobPhaseDistributing     = "DISTRIBUTING"
	JobPhaseCreatingTemplate = "CREATING_TEMPLATE"
	JobPhaseReady            = "READY"

	defaultTemplateCPU         = "2000m"
	defaultTemplateMemory      = "2000Mi"
	defaultTemplateArtifactTTL = 7 * 24 * time.Hour
	defaultArtifactStoreDir    = "/data/CubeMaster/storage"
	fallbackArtifactStoreDir   = "cubemaster-rootfs-artifacts-store"
	rootfsWritableVolumeName   = "cube_rootfs_rw"
	defaultDistributionWorkers = 4
)

var getTemplateImageConfig = config.GetConfig
var deleteRootfsArtifactRecord = func(ctx context.Context, artifactID string) error {
	return store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).Delete(&models.RootfsArtifact{}).Error
}
var ErrNoFailedTemplateReplicas = errors.New("no failed template replicas matched redo request")

type dockerInspectImage struct {
	ID          string            `json:"Id"`
	RepoDigests []string          `json:"RepoDigests"`
	Config      dockerImageConfig `json:"Config"`
}

type dockerImageConfig struct {
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	WorkingDir string   `json:"WorkingDir"`
	User       string   `json:"User"`
}

type resolvedSourceImage struct {
	localRef     string
	digest       string
	config       dockerImageConfig
	configJSON   string
	masterNodeIP string
	cleanup      func(context.Context)
}

func initRootfsArtifactTable(client *gorm.DB) error {
	if client.Migrator().HasTable(&models.RootfsArtifact{}) {
		return nil
	}
	stmt := &gorm.Statement{DB: client}
	stmt.Parse(&models.RootfsArtifact{})
	return client.Exec(`CREATE TABLE IF NOT EXISTS ` + stmt.Schema.Table + ` (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		artifact_id varchar(128) NOT NULL COMMENT 'artifact id',
		template_spec_fingerprint varchar(128) NOT NULL DEFAULT '' COMMENT 'immutable template fingerprint',
		source_image_ref varchar(1024) NOT NULL DEFAULT '' COMMENT 'source image ref',
		source_image_digest varchar(256) NOT NULL DEFAULT '' COMMENT 'source image digest',
		master_node_id varchar(128) NOT NULL DEFAULT '' COMMENT 'master node id',
		master_node_ip varchar(256) NOT NULL DEFAULT '' COMMENT 'master node ip or host',
		ext4_path varchar(2048) NOT NULL DEFAULT '' COMMENT 'artifact ext4 path',
		ext4_sha256 varchar(128) NOT NULL DEFAULT '' COMMENT 'artifact sha256',
		ext4_size_bytes bigint NOT NULL DEFAULT 0 COMMENT 'artifact size',
		image_config_json mediumtext COMMENT 'docker image config json',
		generated_request_json mediumtext COMMENT 'generated create request json',
		writable_layer_size varchar(64) NOT NULL DEFAULT '' COMMENT 'writable layer size',
		download_token varchar(256) NOT NULL DEFAULT '' COMMENT 'download token',
		status varchar(32) NOT NULL DEFAULT '' COMMENT 'artifact status',
		last_error text COMMENT 'last error',
		gc_deadline bigint NOT NULL DEFAULT 0 COMMENT 'gc deadline unix seconds',
		created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at datetime DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY idx_artifact_id (artifact_id),
		UNIQUE KEY idx_artifact_fingerprint (template_spec_fingerprint),
		KEY idx_artifact_status (status)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`).Error
}

func initTemplateImageJobTable(client *gorm.DB) error {
	stmt := &gorm.Statement{DB: client}
	stmt.Parse(&models.TemplateImageJob{})
	if !client.Migrator().HasTable(&models.TemplateImageJob{}) {
		if err := client.Exec(`CREATE TABLE IF NOT EXISTS ` + stmt.Schema.Table + ` (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			job_id varchar(128) NOT NULL COMMENT 'job id',
			template_id varchar(128) NOT NULL DEFAULT '' COMMENT 'template id',
			attempt_no int NOT NULL DEFAULT 1 COMMENT 'attempt number',
			retry_of_job_id varchar(128) NOT NULL DEFAULT '' COMMENT 'previous job id',
			operation varchar(32) NOT NULL DEFAULT '' COMMENT 'job operation',
			redo_mode varchar(32) NOT NULL DEFAULT '' COMMENT 'redo mode',
			redo_scope_json mediumtext COMMENT 'redo scope json',
			resume_phase varchar(64) NOT NULL DEFAULT '' COMMENT 'resume phase',
			node_id varchar(128) NOT NULL DEFAULT '' COMMENT 'target node id for cleanup',
			node_ip varchar(256) NOT NULL DEFAULT '' COMMENT 'target node ip for cleanup',
			snapshot_path varchar(1024) NOT NULL DEFAULT '' COMMENT 'template snapshot path for cleanup',
			artifact_id varchar(128) NOT NULL DEFAULT '' COMMENT 'artifact id',
			template_spec_fingerprint varchar(128) NOT NULL DEFAULT '' COMMENT 'immutable template fingerprint',
			source_image_ref varchar(1024) NOT NULL DEFAULT '' COMMENT 'source image ref',
			source_image_digest varchar(256) NOT NULL DEFAULT '' COMMENT 'source image digest',
			writable_layer_size varchar(64) NOT NULL DEFAULT '' COMMENT 'writable layer size',
			instance_type varchar(64) NOT NULL DEFAULT '' COMMENT 'instance type',
			network_type varchar(64) NOT NULL DEFAULT '' COMMENT 'network type',
			status varchar(32) NOT NULL DEFAULT '' COMMENT 'job status',
			phase varchar(64) NOT NULL DEFAULT '' COMMENT 'current phase',
			progress int NOT NULL DEFAULT 0 COMMENT 'progress percentage',
			error_message text COMMENT 'error message',
			expected_node_count int NOT NULL DEFAULT 0 COMMENT 'expected node count',
			ready_node_count int NOT NULL DEFAULT 0 COMMENT 'ready node count',
			failed_node_count int NOT NULL DEFAULT 0 COMMENT 'failed node count',
			template_status varchar(32) NOT NULL DEFAULT '' COMMENT 'template status',
			artifact_status varchar(32) NOT NULL DEFAULT '' COMMENT 'artifact status',
			request_json mediumtext COMMENT 'sanitized request json',
			result_json mediumtext COMMENT 'result json',
			created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at datetime DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY idx_template_image_job_id (job_id),
			UNIQUE KEY idx_template_image_template_attempt (template_id,attempt_no),
			KEY idx_template_image_status (status),
			KEY idx_template_image_template_status (template_id,status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`).Error; err != nil {
			return err
		}
	}
	return migrateTemplateImageJobTable(client, stmt.Schema.Table)
}

func migrateTemplateImageJobTable(client *gorm.DB, tableName string) error {
	jobModel := &models.TemplateImageJob{}
	if !client.Migrator().HasColumn(jobModel, "attempt_no") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN attempt_no int NOT NULL DEFAULT 1 AFTER template_id`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "retry_of_job_id") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN retry_of_job_id varchar(128) NOT NULL DEFAULT '' AFTER attempt_no`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "operation") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN operation varchar(32) NOT NULL DEFAULT '' AFTER retry_of_job_id`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "redo_mode") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN redo_mode varchar(32) NOT NULL DEFAULT '' AFTER operation`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "redo_scope_json") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN redo_scope_json mediumtext COMMENT 'redo scope json' AFTER redo_mode`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "resume_phase") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN resume_phase varchar(64) NOT NULL DEFAULT '' AFTER redo_scope_json`).Error; err != nil {
			return err
		}
	}
	if err := client.Exec(`UPDATE ` + tableName + ` SET attempt_no = 1 WHERE attempt_no = 0`).Error; err != nil {
		return err
	}
	if client.Migrator().HasIndex(jobModel, "idx_template_image_template_id") {
		if err := client.Migrator().DropIndex(jobModel, "idx_template_image_template_id"); err != nil {
			return err
		}
	}
	if !client.Migrator().HasIndex(jobModel, "idx_template_image_template_attempt") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD UNIQUE KEY idx_template_image_template_attempt (template_id,attempt_no)`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasIndex(jobModel, "idx_template_image_template_status") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD KEY idx_template_image_template_status (template_id,status)`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "node_id") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN node_id varchar(128) NOT NULL DEFAULT '' AFTER retry_of_job_id`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "node_ip") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN node_ip varchar(256) NOT NULL DEFAULT '' AFTER node_id`).Error; err != nil {
			return err
		}
	}
	if !client.Migrator().HasColumn(jobModel, "snapshot_path") {
		if err := client.Exec(`ALTER TABLE ` + tableName + ` ADD COLUMN snapshot_path varchar(1024) NOT NULL DEFAULT '' AFTER node_ip`).Error; err != nil {
			return err
		}
	}
	return nil
}

func SubmitTemplateFromImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	normalized, err := normalizeTemplateImageRequest(req)
	if err != nil {
		return nil, err
	}
	log.G(ctx).Infof(
		"SubmitTemplateFromImage: template_id=%s image=%s network_type=%s cubevs_context=%s",
		normalized.TemplateID,
		normalized.SourceImageRef,
		normalized.NetworkType,
		formatTemplateImageCubeVSContext(normalized.CubeVSContext),
	)
	requestSnapshot, err := marshalTemplateImageJobRequest(normalized)
	if err != nil {
		return nil, err
	}
	jobID := uuid.New().String()
	attemptNo := int32(1)
	retryOfJobID := ""
	reusedExistingJob := false
	if err := withTemplateWriteLock(normalized.TemplateID, func() error {
		definitionFailed := false
		if def, err := GetDefinition(ctx, normalized.TemplateID); err == nil {
			if strings.EqualFold(def.Status, StatusFailed) {
				definitionFailed = true
			} else {
				return fmt.Errorf("template %s already exists; rootfs template specs are immutable, use a new template id to change writable layer size or rootfs settings", normalized.TemplateID)
			}
		} else if !errors.Is(err, ErrTemplateNotFound) {
			return err
		}

		if job, err := getActiveTemplateImageJobByTemplateID(ctx, normalized.TemplateID); err == nil {
			if job.RequestJSON == requestSnapshot {
				jobID = job.JobID
				reusedExistingJob = true
				return nil
			}
			return fmt.Errorf("%w: template %s is currently %s (job_id=%s)", ErrTemplateAttemptInProgress, normalized.TemplateID, strings.ToLower(job.Status), job.JobID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var latestJob *models.TemplateImageJob
		if job, err := getLatestTemplateImageJobByTemplateID(ctx, normalized.TemplateID); err == nil {
			latestJob = job
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if definitionFailed {
			if err := cleanupTemplateReplicas(ctx, normalized.TemplateID); err != nil {
				return err
			}
			if err := cleanupTemplateMetadata(ctx, normalized.TemplateID); err != nil {
				return err
			}
		}

		if latestJob != nil {
			attemptNo = latestJob.AttemptNo + 1
			if attemptNo <= 1 {
				attemptNo = 2
			}
			retryOfJobID = latestJob.JobID
		}
		record := &models.TemplateImageJob{
			JobID:             jobID,
			TemplateID:        normalized.TemplateID,
			AttemptNo:         attemptNo,
			RetryOfJobID:      retryOfJobID,
			Operation:         JobOperationCreate,
			SourceImageRef:    normalized.SourceImageRef,
			WritableLayerSize: normalized.WritableLayerSize,
			InstanceType:      normalized.InstanceType,
			NetworkType:       normalized.NetworkType,
			Status:            JobStatusPending,
			Phase:             JobPhasePulling,
			Progress:          0,
			RequestJSON:       requestSnapshot,
		}
		return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error
	}); err != nil {
		return nil, err
	}
	if reusedExistingJob {
		return GetTemplateImageJobInfo(ctx, jobID)
	}
	go runTemplateImageJob(detachTemplateImageJobContext(ctx, map[string]any{
		"job_id":          jobID,
		"template_id":     normalized.TemplateID,
		"attempt_no":      attemptNo,
		"retry_of_job_id": retryOfJobID,
		"image":           normalized.SourceImageRef,
	}), jobID, normalized, downloadBaseURL)
	return GetTemplateImageJobInfo(ctx, jobID)
}

func SubmitRedoTemplateFromImage(ctx context.Context, req *types.RedoTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	normalized, err := normalizeRedoTemplateImageRequest(req)
	if err != nil {
		return nil, err
	}
	jobID := uuid.NewString()
	var redoJob *models.TemplateImageJob
	if err := withTemplateWriteLock(normalized.TemplateID, func() error {
		if _, err := getActiveTemplateImageJobByTemplateID(ctx, normalized.TemplateID); err == nil {
			return fmt.Errorf("%w: template %s is currently running", ErrTemplateAttemptInProgress, normalized.TemplateID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		latestJob, err := getLatestTemplateImageJobByTemplateID(ctx, normalized.TemplateID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTemplateNotFound
			}
			return err
		}
		if err := allowRedoResumePhase(latestJob); err != nil {
			return err
		}
		sourceReq, err := unmarshalTemplateImageJobRequest(latestJob.RequestJSON)
		if err != nil {
			return fmt.Errorf("decode latest template image request fail: %w", err)
		}
		replicas, err := ListReplicas(ctx, normalized.TemplateID)
		if err != nil {
			return err
		}
		targetNodes, err := resolveRedoTargets(sourceReq.InstanceType, normalized, replicas)
		if err != nil {
			return err
		}
		targetScope := make([]string, 0, len(targetNodes))
		for _, target := range targetNodes {
			if target == nil {
				continue
			}
			targetScope = append(targetScope, target.ID())
		}
		attemptNo := latestJob.AttemptNo + 1
		if attemptNo <= 1 {
			attemptNo = 2
		}
		requestSnapshot, err := marshalTemplateImageJobRequest(sourceReq)
		if err != nil {
			return err
		}
		redoJob = &models.TemplateImageJob{
			JobID:             jobID,
			TemplateID:        normalized.TemplateID,
			AttemptNo:         attemptNo,
			RetryOfJobID:      latestJob.JobID,
			Operation:         JobOperationRedo,
			RedoMode:          determineRedoMode(normalized),
			RedoScopeJSON:     marshalRedoScope(targetScope),
			ResumePhase:       determineRedoResumePhase(latestJob, replicas),
			ArtifactID:        latestJob.ArtifactID,
			SourceImageRef:    sourceReq.SourceImageRef,
			WritableLayerSize: sourceReq.WritableLayerSize,
			InstanceType:      sourceReq.InstanceType,
			NetworkType:       sourceReq.NetworkType,
			Status:            JobStatusPending,
			Phase:             determineRedoResumePhase(latestJob, replicas),
			Progress:          0,
			RequestJSON:       requestSnapshot,
		}
		return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(redoJob).Error
	}); err != nil {
		return nil, err
	}
	go runRedoTemplateImageJob(detachTemplateImageJobContext(ctx, map[string]any{
		"job_id":      jobID,
		"template_id": normalized.TemplateID,
	}), jobID, normalized, downloadBaseURL)
	return GetTemplateImageJobInfo(ctx, jobID)
}

func GetTemplateImageJobInfo(ctx context.Context, jobID string) (*types.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	record := &models.TemplateImageJob{}
	if err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).First(record).Error; err != nil {
		return nil, err
	}
	return jobModelToInfo(ctx, record)
}

func GetRootfsArtifactInfo(ctx context.Context, artifactID string) (*types.RootfsArtifactInfo, error) {
	record, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	return artifactModelToInfo(record), nil
}

func normalizeRedoTemplateImageRequest(req *types.RedoTemplateFromImageReq) (*types.RedoTemplateFromImageReq, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if req.Request == nil || strings.TrimSpace(req.RequestID) == "" {
		return nil, errors.New("requestID is required")
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		return nil, errors.New("template_id is required")
	}
	cloned := *req
	if len(req.DistributionScope) > 0 {
		cloned.DistributionScope = append([]string(nil), req.DistributionScope...)
	}
	return &cloned, nil
}

func allowRedoResumePhase(job *models.TemplateImageJob) error {
	if job == nil {
		return ErrTemplateNotFound
	}
	switch strings.ToUpper(strings.TrimSpace(job.Phase)) {
	case "", JobPhasePulling:
		return errors.New("template redo is not allowed before source image has been pulled successfully")
	default:
		return nil
	}
}

func getTemplateImageJobRecordByID(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	if err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).First(record).Error; err != nil {
		return nil, err
	}
	return record, nil
}

func unmarshalTemplateImageJobRequest(payload string) (*types.CreateTemplateFromImageReq, error) {
	req := &types.CreateTemplateFromImageReq{}
	if err := json.Unmarshal([]byte(payload), req); err != nil {
		return nil, err
	}
	req.Request = &types.Request{RequestID: uuid.NewString()}
	return normalizeTemplateImageRequest(req)
}

func determineRedoMode(req *types.RedoTemplateFromImageReq) string {
	switch {
	case req == nil:
		return RedoModeAll
	case req.FailedOnly && len(req.DistributionScope) > 0:
		return RedoModeFailedNodes
	case req.FailedOnly:
		return RedoModeFailedOnly
	case len(req.DistributionScope) > 0:
		return RedoModeNodes
	default:
		return RedoModeAll
	}
}

func replicaNeedsRedo(replica models.TemplateReplica) bool {
	return replica.Status != ReplicaStatusReady || replica.CleanupRequired
}

func failedRedoScope(replicas []models.TemplateReplica) []string {
	failedScope := make([]string, 0, len(replicas))
	for _, replica := range replicas {
		if !replicaNeedsRedo(replica) {
			continue
		}
		if replica.NodeID != "" {
			failedScope = append(failedScope, replica.NodeID)
			continue
		}
		if replica.NodeIP != "" {
			failedScope = append(failedScope, replica.NodeIP)
		}
	}
	return failedScope
}

func marshalRedoScope(scope []string) string {
	if len(scope) == 0 {
		return ""
	}
	payload, err := json.Marshal(scope)
	if err != nil {
		return ""
	}
	return string(payload)
}

func unmarshalRedoScope(scopeJSON string) []string {
	if strings.TrimSpace(scopeJSON) == "" {
		return nil
	}
	var scope []string
	if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
		return nil
	}
	return scope
}

func determineRedoResumePhase(job *models.TemplateImageJob, replicas []models.TemplateReplica) string {
	if job != nil {
		switch strings.ToUpper(job.Phase) {
		case JobPhasePulling, JobPhaseUnpacking, JobPhaseBuildingExt4, JobPhaseGeneratingJSON:
			return JobPhaseBuildingExt4
		case JobPhaseDistributing:
			return JobPhaseDistributing
		case JobPhaseCreatingTemplate, JobPhaseSnapshotting, JobPhaseRegistering:
			return JobPhaseSnapshotting
		}
	}
	for _, replica := range replicas {
		if replica.Status == ReplicaStatusReady {
			continue
		}
		switch strings.ToUpper(replica.LastErrorPhase) {
		case ReplicaPhaseDistributing:
			return JobPhaseDistributing
		case ReplicaPhaseSnapshotting, ReplicaPhaseFailed:
			return JobPhaseSnapshotting
		}
	}
	return JobPhaseSnapshotting
}

func resolveTemplateNodes(instanceType string, scope []string) ([]*node.Node, error) {
	nodes := healthyTemplateNodes(instanceType)
	if len(nodes) == 0 {
		return nil, ErrNoTemplateNodes
	}
	if len(scope) == 0 {
		return nodes, nil
	}
	allowed := make(map[string]struct{}, len(scope))
	for _, item := range scope {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		allowed[item] = struct{}{}
	}
	selected := make([]*node.Node, 0, len(nodes))
	matched := make(map[string]struct{})
	for _, item := range nodes {
		if item == nil {
			continue
		}
		if _, ok := allowed[item.ID()]; ok {
			selected = append(selected, item)
			matched[item.ID()] = struct{}{}
			continue
		}
		if _, ok := allowed[item.HostIP()]; ok {
			selected = append(selected, item)
			matched[item.HostIP()] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, item := range scope {
		if _, ok := matched[item]; ok {
			continue
		}
		missing = append(missing, item)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("target nodes are not healthy or not found: %s", strings.Join(missing, ","))
	}
	if len(selected) == 0 {
		return nil, ErrNoTemplateNodes
	}
	return selected, nil
}

func resolveRedoTargets(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
	if req == nil {
		return resolveTemplateNodes(instanceType, nil)
	}
	baseScope := req.DistributionScope
	if len(baseScope) == 0 {
		baseScope = nil
	}
	targets, err := resolveTemplateNodes(instanceType, baseScope)
	if err != nil {
		return nil, err
	}
	if !req.FailedOnly {
		return targets, nil
	}
	failedScope := failedRedoScope(replicas)
	if len(failedScope) == 0 {
		return nil, ErrNoFailedTemplateReplicas
	}
	failedSet := make(map[string]struct{}, len(failedScope))
	for _, item := range failedScope {
		if strings.TrimSpace(item) == "" {
			continue
		}
		failedSet[item] = struct{}{}
	}
	filtered := make([]*node.Node, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		if _, ok := failedSet[target.ID()]; ok {
			filtered = append(filtered, target)
			continue
		}
		if _, ok := failedSet[target.HostIP()]; ok {
			filtered = append(filtered, target)
		}
	}
	if len(filtered) == 0 {
		return nil, ErrNoFailedTemplateReplicas
	}
	return filtered, nil
}

func prepareLocalSourceImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*resolvedSourceImage, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	inspectOutput, err := dockerOutput(ctx, "", "image", "inspect", req.SourceImageRef)
	if err != nil {
		return nil, fmt.Errorf("redo requires source image %s to still exist locally: %w", req.SourceImageRef, err)
	}
	var inspectList []dockerInspectImage
	if err := json.Unmarshal(inspectOutput, &inspectList); err != nil {
		return nil, fmt.Errorf("unmarshal local docker inspect output: %w", err)
	}
	if len(inspectList) == 0 {
		return nil, fmt.Errorf("docker image inspect returned empty result for %s", req.SourceImageRef)
	}
	inspectInfo := inspectList[0]
	configJSON, _ := json.Marshal(inspectInfo.Config)
	return &resolvedSourceImage{
		localRef:     req.SourceImageRef,
		digest:       firstNonEmptyDigest(inspectInfo),
		config:       inspectInfo.Config,
		configJSON:   string(configJSON),
		masterNodeIP: normalizeBaseURL(downloadBaseURL),
	}, nil
}

func buildReplicaForDistribution(target *node.Node, req *types.CreateCubeSandboxReq, artifactID, jobID string) ReplicaStatus {
	spec := ""
	instanceType := ""
	if req != nil {
		spec = calculateRequestSpec(req)
		instanceType = req.InstanceType
	}
	return ReplicaStatus{
		NodeID:          target.ID(),
		NodeIP:          target.HostIP(),
		InstanceType:    instanceType,
		Spec:            spec,
		Status:          ReplicaStatusFailed,
		Phase:           ReplicaPhaseDistributing,
		ArtifactID:      artifactID,
		LastJobID:       jobID,
		LastErrorPhase:  ReplicaPhaseDistributing,
		CleanupRequired: true,
	}
}

func cleanupArtifactOnNodes(ctx context.Context, artifactID string, targets []*node.Node) error {
	if artifactID == "" {
		return nil
	}
	var cleanupErr error
	for _, target := range targets {
		if target == nil {
			continue
		}
		rsp, err := deleteImageOnCubelet(ctx, getCubeletAddrForDelete(target.HostIP()), &imagev1.DestroyImageRequest{
			RequestID: uuid.NewString(),
			Spec: &imagev1.ImageSpec{
				Image: artifactID,
			},
		})
		if err != nil {
			if isIgnorableArtifactDeleteError(err) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete artifact %s on node %s: %w", artifactID, target.ID(), err))
			continue
		}
		if rsp.GetRet() != nil && int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
			if isIgnorableArtifactDeleteMessage(rsp.GetRet().GetRetMsg()) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete artifact %s on node %s failed: %s", artifactID, target.ID(), rsp.GetRet().GetRetMsg()))
		}
	}
	return cleanupErr
}

func cleanupTemplateReplicasOnNodes(ctx context.Context, templateID string, replicas []models.TemplateReplica, targets []*node.Node) error {
	if len(replicas) == 0 || len(targets) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(targets)*2)
	for _, target := range targets {
		if target == nil {
			continue
		}
		allowed[target.ID()] = struct{}{}
		if target.HostIP() != "" {
			allowed[target.HostIP()] = struct{}{}
		}
	}
	locators := make([]templateCleanupLocator, 0, len(replicas))
	for _, replica := range replicas {
		if _, ok := allowed[replica.NodeID]; !ok {
			if _, ok := allowed[replica.NodeIP]; !ok {
				continue
			}
		}
		locators = append(locators, templateCleanupLocator{
			NodeID:       replica.NodeID,
			NodeIP:       replica.NodeIP,
			SnapshotPath: replica.SnapshotPath,
		})
	}
	if len(locators) == 0 {
		return nil
	}
	return cleanupTemplateReplicasWithLocators(ctx, templateID, locators)
}

func OpenRootfsArtifact(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, *os.File, error) {
	record, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, nil, err
	}
	if record.DownloadToken != "" && token != record.DownloadToken {
		return nil, nil, fmt.Errorf("invalid artifact token")
	}
	f, err := os.Open(record.Ext4Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("artifact source missing: %w", err)
		}
		return nil, nil, err
	}
	return record, f, nil
}

func runTemplateImageJob(ctx context.Context, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL string) {
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": req.TemplateID,
		"image":       req.SourceImageRef,
	})
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhasePulling,
		"progress": 5,
	}); err != nil {
		logger.Errorf("update job start fail: %v", err)
		return
	}
	if err := ensureArtifactBuildPreflight(ctx); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhasePulling,
			"progress":      100,
			"error_message": err.Error(),
		})
		return
	}
	source, err := prepareSourceImage(ctx, req, downloadBaseURL)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhasePulling,
			"progress":      100,
			"error_message": err.Error(),
		})
		return
	}
	if source.cleanup != nil {
		defer source.cleanup(ctx)
	}
	fingerprint := buildTemplateSpecFingerprint(req, source.digest)
	artifactID := buildArtifactID(fingerprint)
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"artifact_id":               artifactID,
		"template_spec_fingerprint": fingerprint,
		"source_image_digest":       source.digest,
		"phase":                     JobPhaseUnpacking,
		"progress":                  20,
	}); err != nil {
		logger.Errorf("update job source metadata fail: %v", err)
	}
	artifact, generatedReq, builtFreshArtifact, err := ensureRootfsArtifact(ctx, req, source, downloadBaseURL)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":                    JobStatusFailed,
			"phase":                     JobPhaseBuildingExt4,
			"artifact_id":               artifactID,
			"template_spec_fingerprint": fingerprint,
			"artifact_status":           ArtifactStatusFailed,
			"error_message":             err.Error(),
			"progress":                  100,
		})
		return
	}
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"artifact_id":               artifact.ArtifactID,
		"template_spec_fingerprint": artifact.TemplateSpecFingerprint,
		"source_image_digest":       artifact.SourceImageDigest,
		"artifact_status":           artifact.Status,
		"phase":                     JobPhaseDistributing,
		"progress":                  70,
	}); err != nil {
		logger.Errorf("update job artifact fail: %v", err)
	}
	readyTargets, expected, ready, failed, distErr := distributeRootfsArtifact(ctx, req, generatedReq, artifact, req.TemplateID, jobID)
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":               JobPhaseCreatingTemplate,
		"progress":            85,
		"expected_node_count": expected,
		"ready_node_count":    ready,
		"failed_node_count":   failed,
		"error_message":       errorString(distErr),
	}); err != nil {
		logger.Errorf("update distribution status fail: %v", err)
	}
	if expected > 0 && ready == 0 {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after distribution failure fail: %v", cleanupErr)
			}
		}
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseDistributing,
			"progress":      100,
			"error_message": fmt.Sprintf("artifact distribution failed on all %d nodes: %v", expected, distErr),
		})
		return
	}
	var info *TemplateInfo
	storedReq, err := normalizeStoredTemplateRequest(generatedReq)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	if _, err := ensureTemplateDefinition(ctx, req.TemplateID, storedReq, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations)); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	replicas, persistErr := createTemplateReplicasOnNodes(ctx, req.TemplateID, generatedReq, readyTargets, replicaRunOptions{
		ArtifactID: artifact.ArtifactID,
		JobID:      jobID,
	})
	if persistErr != nil {
		err = persistErr
	} else {
		info, err = finalizeTemplateReplicas(ctx, req.TemplateID, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations), replicas)
	}
	if err != nil {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after create template error fail: %v", cleanupErr)
			}
		}
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	resultPayload, _ := json.Marshal(info)
	jobStatus := JobStatusReady
	jobPhase := JobPhaseReady
	if info.Status == StatusFailed {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after failed template status fail: %v", cleanupErr)
			}
		}
		jobStatus = JobStatusFailed
		jobPhase = JobPhaseCreatingTemplate
	}
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":          jobStatus,
		"phase":           jobPhase,
		"progress":        100,
		"template_status": info.Status,
		"result_json":     string(resultPayload),
		"error_message":   info.LastError,
	})
}

func failRedoTemplateImageJob(ctx context.Context, jobID, phase, message string) {
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":        JobStatusFailed,
		"phase":         phase,
		"progress":      100,
		"error_message": message,
	})
}

func runRedoTemplateImageJob(ctx context.Context, jobID string, req *types.RedoTemplateFromImageReq, downloadBaseURL string) {
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": req.TemplateID,
	})
	jobRecord, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil {
		logger.Errorf("lookup redo job fail: %v", err)
		return
	}
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    jobRecord.ResumePhase,
		"progress": 5,
	}); err != nil {
		logger.Errorf("update redo job start fail: %v", err)
		return
	}
	sourceReq, err := unmarshalTemplateImageJobRequest(jobRecord.RequestJSON)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, jobRecord.ResumePhase, err.Error())
		return
	}
	existingReplicas, err := ListReplicas(ctx, req.TemplateID)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, jobRecord.ResumePhase, err.Error())
		return
	}
	targets, err := resolveRedoTargets(sourceReq.InstanceType, req, existingReplicas)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, jobRecord.ResumePhase, err.Error())
		return
	}
	workingReq := *sourceReq
	workingReq.Request = &types.Request{RequestID: uuid.NewString()}
	workingReq.DistributionScope = make([]string, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		workingReq.DistributionScope = append(workingReq.DistributionScope, target.ID())
	}

	var artifact *models.RootfsArtifact
	resumePhase := jobRecord.ResumePhase
	if resumePhase == "" {
		resumePhase = JobPhaseSnapshotting
	}
	if resumePhase == JobPhaseBuildingExt4 {
		if err := ensureArtifactBuildPreflight(ctx); err != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4, err.Error())
			return
		}
		if jobRecord.ArtifactID != "" {
			if previousArtifact, lookupErr := getRootfsArtifactByID(ctx, jobRecord.ArtifactID); lookupErr == nil {
				if previousArtifact.Ext4Path != "" {
					_ = cleanupLocalRootfsArtifact(previousArtifact.ArtifactID, previousArtifact.Ext4Path)
				}
				_ = updateRootfsArtifact(ctx, previousArtifact.ArtifactID, map[string]any{
					"status":     ArtifactStatusFailed,
					"last_error": "redo requested after artifact build failure",
				})
			}
		}
		source, prepErr := prepareLocalSourceImage(ctx, &workingReq, downloadBaseURL)
		if prepErr != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4, prepErr.Error())
			return
		}
		var generatedReq *types.CreateCubeSandboxReq
		var builtFresh bool
		artifact, generatedReq, builtFresh, err = ensureRootfsArtifact(ctx, &workingReq, source, downloadBaseURL)
		if err != nil {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":          JobStatusFailed,
				"phase":           JobPhaseBuildingExt4,
				"progress":        100,
				"artifact_status": ArtifactStatusFailed,
				"error_message":   err.Error(),
			})
			return
		}
		workingReq = *sourceReq
		workingReq.Request = &types.Request{RequestID: uuid.NewString()}
		workingReq.DistributionScope = make([]string, 0, len(targets))
		for _, target := range targets {
			if target == nil {
				continue
			}
			workingReq.DistributionScope = append(workingReq.DistributionScope, target.ID())
		}
		_ = generatedReq
		_ = builtFresh
		jobRecord.ArtifactID = artifact.ArtifactID
		if err := updateTemplateImageJob(ctx, jobID, map[string]any{
			"artifact_id":               artifact.ArtifactID,
			"template_spec_fingerprint": artifact.TemplateSpecFingerprint,
			"source_image_digest":       artifact.SourceImageDigest,
			"artifact_status":           artifact.Status,
			"phase":                     JobPhaseDistributing,
			"progress":                  60,
		}); err != nil {
			logger.Errorf("update redo rebuilt artifact fail: %v", err)
		}
		resumePhase = JobPhaseDistributing
	} else {
		artifact, err = getRootfsArtifactByID(ctx, jobRecord.ArtifactID)
		if err != nil {
			failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
			return
		}
	}

	var generatedReq *types.CreateCubeSandboxReq
	if strings.TrimSpace(artifact.GeneratedRequestJSON) != "" {
		generatedReq = &types.CreateCubeSandboxReq{}
		if err := json.Unmarshal([]byte(artifact.GeneratedRequestJSON), generatedReq); err != nil {
			generatedReq = nil
		}
	}
	if generatedReq == nil {
		generatedReq, err = generateTemplateCreateRequest(&workingReq, artifact, dockerImageConfig{}, downloadBaseURL)
		if artifact.ImageConfigJSON != "" {
			var imageCfg dockerImageConfig
			if json.Unmarshal([]byte(artifact.ImageConfigJSON), &imageCfg) == nil {
				generatedReq, err = generateTemplateCreateRequest(&workingReq, artifact, imageCfg, downloadBaseURL)
			}
		}
	}
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
		return
	}

	readyTargets := targets
	if resumePhase == JobPhaseDistributing {
		if err := cleanupArtifactOnNodes(ctx, artifact.ArtifactID, targets); err != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseDistributing, fmt.Sprintf("cleanup artifact before redistribute failed: %v", err))
			return
		}
		distributedTargets, expected, ready, failed, distErr := distributeRootfsArtifact(ctx, &workingReq, generatedReq, artifact, req.TemplateID, jobID)
		if err := updateTemplateImageJob(ctx, jobID, map[string]any{
			"phase":               JobPhaseSnapshotting,
			"progress":            80,
			"expected_node_count": expected,
			"ready_node_count":    ready,
			"failed_node_count":   failed,
			"artifact_status":     artifact.Status,
			"error_message":       errorString(distErr),
		}); err != nil {
			logger.Errorf("update redo distribution status fail: %v", err)
		}
		if expected > 0 && ready == 0 {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":        JobStatusFailed,
				"phase":         JobPhaseDistributing,
				"progress":      100,
				"error_message": fmt.Sprintf("artifact redistribution failed on all %d nodes: %v", expected, distErr),
			})
			return
		}
		readyTargets = distributedTargets
		resumePhase = JobPhaseSnapshotting
	}

	if err := cleanupTemplateReplicasOnNodes(ctx, req.TemplateID, existingReplicas, readyTargets); err != nil {
		failRedoTemplateImageJob(ctx, jobID, JobPhaseSnapshotting, fmt.Sprintf("cleanup template replicas before redo snapshot failed: %v", err))
		return
	}
	storedReq, err := normalizeStoredTemplateRequest(generatedReq)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
		return
	}
	if _, err := ensureTemplateDefinition(ctx, req.TemplateID, storedReq, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations)); err != nil {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
		return
	}
	if _, err := createTemplateReplicasOnNodes(ctx, req.TemplateID, generatedReq, readyTargets, replicaRunOptions{
		ArtifactID: artifact.ArtifactID,
		JobID:      jobID,
	}); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseSnapshotting,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	if err := refreshTemplateReplicaSummary(ctx, req.TemplateID); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseSnapshotting,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	info, err := GetTemplateInfo(ctx, req.TemplateID)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseSnapshotting,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	resultPayload, _ := json.Marshal(info)
	finalStatus := JobStatusReady
	finalPhase := JobPhaseReady
	if info.Status == StatusFailed {
		finalStatus = JobStatusFailed
		finalPhase = JobPhaseSnapshotting
	}
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":          finalStatus,
		"phase":           finalPhase,
		"progress":        100,
		"artifact_id":     artifact.ArtifactID,
		"artifact_status": artifact.Status,
		"template_status": info.Status,
		"result_json":     string(resultPayload),
		"error_message":   info.LastError,
	})
}

func ensureRootfsArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, source *resolvedSourceImage, downloadBaseURL string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
	var generatedReq *types.CreateCubeSandboxReq
	fingerprint := buildTemplateSpecFingerprint(req, source.digest)
	artifactID := buildArtifactID(fingerprint)
	record, wasDeleted, err := findReusableRootfsArtifact(ctx, fingerprint, artifactID)
	if err == nil && wasDeleted {
		if restoreErr := restoreRootfsArtifact(ctx, artifactID); restoreErr != nil {
			return nil, nil, false, restoreErr
		}
		record.DeletedAt = gorm.DeletedAt{}
	}
	if err == nil && record.Status == ArtifactStatusReady && record.GeneratedRequestJSON != "" {
		generatedReq, err = generateTemplateCreateRequest(req, record, source.config, downloadBaseURL)
		if err == nil {
			return record, generatedReq, false, nil
		}
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, false, err
	}
	if record == nil {
		record = &models.RootfsArtifact{
			ArtifactID:              artifactID,
			TemplateSpecFingerprint: fingerprint,
			SourceImageRef:          req.SourceImageRef,
			SourceImageDigest:       source.digest,
			WritableLayerSize:       req.WritableLayerSize,
			Status:                  ArtifactStatusPending,
		}
		if createErr := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).Create(record).Error; createErr != nil {
			if !errors.Is(createErr, gorm.ErrDuplicatedKey) &&
				!strings.Contains(createErr.Error(), "1062") &&
				!strings.Contains(createErr.Error(), "Duplicate entry") {
				return nil, nil, false, createErr
			}
			record, wasDeleted, err = findReusableRootfsArtifact(ctx, fingerprint, artifactID)
			if err != nil {
				return nil, nil, false, createErr
			}
			if wasDeleted {
				if restoreErr := restoreRootfsArtifact(ctx, artifactID); restoreErr != nil {
					return nil, nil, false, restoreErr
				}
				record.DeletedAt = gorm.DeletedAt{}
			}
			if record.Status == ArtifactStatusReady && record.GeneratedRequestJSON != "" {
				generatedReq, err = generateTemplateCreateRequest(req, record, source.config, downloadBaseURL)
				if err == nil {
					return record, generatedReq, false, nil
				}
			}
		}
	}
	_ = updateRootfsArtifact(ctx, artifactID, map[string]any{
		"template_spec_fingerprint": fingerprint,
		"source_image_ref":          req.SourceImageRef,
		"source_image_digest":       source.digest,
		"writable_layer_size":       req.WritableLayerSize,
		"status":                    ArtifactStatusBuilding,
		"last_error":                "",
	})
	record, generatedReq, err = buildRootfsArtifact(ctx, record, req, source, downloadBaseURL)
	if err != nil {
		_ = updateRootfsArtifact(ctx, artifactID, map[string]any{
			"status":     ArtifactStatusFailed,
			"last_error": err.Error(),
		})
		return nil, nil, false, err
	}
	return record, generatedReq, true, nil
}

func findReusableRootfsArtifact(ctx context.Context, fingerprint, artifactID string) (*models.RootfsArtifact, bool, error) {
	record, err := getRootfsArtifactByFingerprint(ctx, fingerprint)
	if err == nil {
		record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
		return record, rootfsArtifactSoftDeleted(record), err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	record, err = getRootfsArtifactByFingerprintUnscoped(ctx, fingerprint)
	if err == nil {
		record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
		return record, rootfsArtifactSoftDeleted(record), err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	record, err = getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		record, err = getRootfsArtifactByIDUnscoped(ctx, artifactID)
		if err != nil {
			return nil, false, err
		}
	}
	record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
	return record, rootfsArtifactSoftDeleted(record), err
}

func validateReusableRootfsArtifact(record *models.RootfsArtifact, fingerprint, artifactID string) (*models.RootfsArtifact, error) {
	if record == nil {
		return nil, gorm.ErrRecordNotFound
	}
	if record.ArtifactID != artifactID {
		return nil, fmt.Errorf("rootfs artifact id mismatch: want %s got %s", artifactID, record.ArtifactID)
	}
	if record.TemplateSpecFingerprint != "" && record.TemplateSpecFingerprint != fingerprint {
		return nil, fmt.Errorf("rootfs artifact %s fingerprint mismatch: want %s got %s", artifactID, fingerprint, record.TemplateSpecFingerprint)
	}
	return record, nil
}

func rootfsArtifactSoftDeleted(record *models.RootfsArtifact) bool {
	return record != nil && record.DeletedAt.Valid
}

func restoreRootfsArtifact(ctx context.Context, artifactID string) error {
	tx := store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).
		Updates(map[string]any{
			"deleted_at": nil,
			"updated_at": time.Now(),
		})
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func buildRootfsArtifact(ctx context.Context, record *models.RootfsArtifact, req *types.CreateTemplateFromImageReq, source *resolvedSourceImage, downloadBaseURL string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	workDir := filepath.Join(artifactWorkRootDir(), record.ArtifactID)
	rootfsDir := filepath.Join(workDir, "rootfs")
	storeDir, err := resolveArtifactStoreDir(ctx, record.ArtifactID)
	if err != nil {
		return nil, nil, err
	}
	storeRootfsDir := filepath.Join(storeDir, "rootfs")
	ext4Path := filepath.Join(storeDir, record.ArtifactID+".ext4")
	keepStoreDir := false
	defer func() {
		cleanupIntermediateArtifacts(workDir, storeRootfsDir, storeDir, keepStoreDir)
	}()
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return nil, nil, err
	}
	if _, err = exportImageRootfs(ctx, source, workDir, rootfsDir); err != nil {
		return nil, nil, err
	}
	if err := relocateRootfsToArtifactStore(ctx, rootfsDir, storeRootfsDir); err != nil {
		return nil, nil, err
	}
	if err := createExt4Image(ctx, storeRootfsDir, ext4Path); err != nil {
		return nil, nil, err
	}
	shaValue, sizeBytes, err := computeFileSHA256(ext4Path)
	if err != nil {
		return nil, nil, err
	}
	downloadToken := uuid.New().String()
	record.SourceImageDigest = source.digest
	record.MasterNodeIP = source.masterNodeIP
	record.Ext4Path = ext4Path
	record.Ext4SHA256 = shaValue
	record.Ext4SizeBytes = sizeBytes
	record.ImageConfigJSON = source.configJSON
	record.DownloadToken = downloadToken
	record.Status = ArtifactStatusReady
	record.GCDeadline = time.Now().Add(defaultTemplateArtifactTTL).Unix()

	generatedReq, err := generateTemplateCreateRequest(req, record, source.config, downloadBaseURL)
	if err != nil {
		return nil, nil, err
	}
	reqPayload, err := json.Marshal(generatedReq)
	if err != nil {
		return nil, nil, err
	}
	record.GeneratedRequestJSON = string(reqPayload)
	if err := updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
		"source_image_digest":    record.SourceImageDigest,
		"master_node_ip":         record.MasterNodeIP,
		"ext4_path":              record.Ext4Path,
		"ext4_sha256":            record.Ext4SHA256,
		"ext4_size_bytes":        record.Ext4SizeBytes,
		"image_config_json":      record.ImageConfigJSON,
		"generated_request_json": record.GeneratedRequestJSON,
		"download_token":         record.DownloadToken,
		"status":                 record.Status,
		"gc_deadline":            record.GCDeadline,
		"last_error":             "",
	}); err != nil {
		return nil, nil, err
	}
	latest, err := getRootfsArtifactByID(ctx, record.ArtifactID)
	if err != nil {
		return nil, nil, err
	}
	keepStoreDir = true
	return latest, generatedReq, nil
}

func prepareSourceImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*resolvedSourceImage, error) {
	var (
		dockerConfigDir    string
		imageExistsLocally bool
		inspectOutput      []byte
		err                error
	)
	inspectOutput, err = dockerOutput(ctx, "", "image", "inspect", req.SourceImageRef)
	if err == nil {
		imageExistsLocally = true
	}
	if !imageExistsLocally {
		if req.RegistryUsername != "" || req.RegistryPassword != "" {
			tmpDir, err := os.MkdirTemp("", "cubemaster-docker-config-*")
			if err != nil {
				return nil, err
			}
			dockerConfigDir = tmpDir
			defer os.RemoveAll(tmpDir)
			if err := dockerLogin(ctx, dockerConfigDir, req.SourceImageRef, req.RegistryUsername, req.RegistryPassword); err != nil {
				return nil, err
			}
		}
		if err := dockerRun(ctx, dockerConfigDir, "pull", req.SourceImageRef); err != nil {
			return nil, fmt.Errorf("docker pull %s failed: %w", req.SourceImageRef, err)
		}
		inspectOutput, err = dockerOutput(ctx, dockerConfigDir, "image", "inspect", req.SourceImageRef)
		if err != nil {
			return nil, fmt.Errorf("docker image inspect %s failed: %w", req.SourceImageRef, err)
		}
	}
	var inspectList []dockerInspectImage
	if err := json.Unmarshal(inspectOutput, &inspectList); err != nil {
		return nil, fmt.Errorf("unmarshal docker inspect output: %w", err)
	}
	if len(inspectList) == 0 {
		return nil, fmt.Errorf("docker image inspect returned empty result for %s", req.SourceImageRef)
	}
	inspectInfo := inspectList[0]
	configJSON, _ := json.Marshal(inspectInfo.Config)
	return &resolvedSourceImage{
		localRef:     req.SourceImageRef,
		digest:       firstNonEmptyDigest(inspectInfo),
		config:       inspectInfo.Config,
		configJSON:   string(configJSON),
		masterNodeIP: normalizeBaseURL(downloadBaseURL),
		cleanup: func(cleanupCtx context.Context) {
			if dockerConfigDir != "" {
				_ = os.RemoveAll(dockerConfigDir)
			}
			if !imageExistsLocally {
				_ = dockerRun(cleanupCtx, "", "image", "rm", "-f", req.SourceImageRef)
			}
		},
	}, nil
}

func exportImageRootfs(ctx context.Context, source *resolvedSourceImage, workDir, rootfsDir string) (string, error) {
	containerIDBytes, err := dockerOutput(ctx, "", "create", source.localRef)
	if err != nil {
		return "", fmt.Errorf("docker create %s failed: %w", source.localRef, err)
	}
	containerID := strings.TrimSpace(string(containerIDBytes))
	defer func() {
		_ = dockerRun(ctx, "", "rm", "-f", containerID)
	}()
	tarPath := filepath.Join(workDir, "rootfs.tar")
	if err := dockerRun(ctx, "", "export", "-o", tarPath, containerID); err != nil {
		return "", fmt.Errorf("docker export %s failed: %w", containerID, err)
	}
	if err := os.RemoveAll(rootfsDir); err != nil { // NOCC:Path Traversal()
		return "", err
	}
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return "", err
	}
	if err := runCommand(ctx, "", "tar", "-xf", tarPath, "-C", rootfsDir); err != nil {
		return "", fmt.Errorf("extract rootfs tar failed: %w", err)
	}
	return tarPath, nil
}

func createExt4Image(ctx context.Context, rootfsDir, ext4Path string) error {
	sizeBytes, err := directorySize(rootfsDir)
	if err != nil {
		return err
	}

	raw := sizeBytes + 256*1024*1024
	const gib int64 = 1024 * 1024 * 1024
	if raw < gib {
		raw = gib
	}

	gibs := (raw + gib - 1) / gib
	pow := int64(1)
	for pow < gibs {
		pow <<= 1
	}
	imageSize := pow * gib
	if err := runCommand(ctx, "", "truncate", "-s", strconv.FormatInt(imageSize, 10), ext4Path); err != nil {
		return fmt.Errorf("truncate ext4 image failed: %w", err)
	}
	if err := runCommand(ctx, "", "mkfs.ext4", "-F", "-d", rootfsDir, ext4Path); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %w", err)
	}
	return nil
}

func generateTemplateCreateRequest(req *types.CreateTemplateFromImageReq, artifact *models.RootfsArtifact, imageCfg dockerImageConfig, downloadBaseURL string) (*types.CreateCubeSandboxReq, error) {
	annotations := map[string]string{
		constants.CubeAnnotationAppSnapshotTemplateID:      req.TemplateID,
		constants.CubeAnnotationsAppSnapshotCreate:         "true",
		constants.CubeAnnotationAppSnapshotVersion:         DefaultTemplateVersion,
		constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		constants.CubeAnnotationRootfsArtifactID:           artifact.ArtifactID,
		constants.CubeAnnotationWritableLayerSize:          req.WritableLayerSize,
		constants.CubeAnnotationTemplateSpecFingerprint:    artifact.TemplateSpecFingerprint,
	}
	sizeGi, err := quantityToGi(req.WritableLayerSize)
	if err == nil && sizeGi > 0 {
		annotations[constants.CubeAnnotationsSystemDiskSize] = strconv.FormatInt(sizeGi, 10)
	}
	if len(req.ExposedPorts) > 0 {
		annotations[constants.AnnotationsExposedPort] = formatExposedPortsAnnotation(req.ExposedPorts)
	}
	rootVolume := &types.Volume{
		Name: rootfsWritableVolumeName,
		VolumeSource: &types.VolumeSource{
			EmptyDir: &types.EmptyDirVolumeSource{
				SizeLimit: req.WritableLayerSize,
			},
		},
	}
	imageAnnotations := map[string]string{
		constants.CubeAnnotationRootfsArtifactID:        artifact.ArtifactID,
		constants.CubeAnnotationRootfsArtifactURL:       buildDownloadURL(downloadBaseURL, artifact.ArtifactID, artifact.DownloadToken),
		constants.CubeAnnotationRootfsArtifactToken:     artifact.DownloadToken,
		constants.CubeAnnotationRootfsArtifactSHA256:    artifact.Ext4SHA256,
		constants.CubeAnnotationRootfsArtifactSizeBytes: strconv.FormatInt(artifact.Ext4SizeBytes, 10),
		constants.CubeAnnotationWritableLayerSize:       req.WritableLayerSize,
		constants.CubeAnnotationTemplateSpecFingerprint: artifact.TemplateSpecFingerprint,
	}
	command := imageCfg.Entrypoint
	args := imageCfg.Cmd
	if req.ContainerOverrides != nil {
		if len(req.ContainerOverrides.Command) > 0 {
			command = req.ContainerOverrides.Command
		}
		if len(req.ContainerOverrides.Args) > 0 {
			args = req.ContainerOverrides.Args
		}
	}
	envs := envListToKeyValues(imageCfg.Env)
	if req.ContainerOverrides != nil && req.ContainerOverrides.Envs != nil {
		envs = req.ContainerOverrides.Envs
	}
	workingDir := imageCfg.WorkingDir
	if req.ContainerOverrides != nil && req.ContainerOverrides.WorkingDir != "" {
		workingDir = req.ContainerOverrides.WorkingDir
	}
	resources := &types.Resource{Cpu: defaultTemplateCPU, Mem: defaultTemplateMemory}
	if req.ContainerOverrides != nil && req.ContainerOverrides.Resources != nil {
		resources = req.ContainerOverrides.Resources
	}
	securityContext := &types.ContainerSecurityContext{Privileged: true, ReadonlyRootfs: false}
	if req.ContainerOverrides != nil && req.ContainerOverrides.SecurityContext != nil {
		securityContext = req.ContainerOverrides.SecurityContext
		securityContext.ReadonlyRootfs = false
	}
	if req.ContainerOverrides != nil && req.ContainerOverrides.VolumeMounts != nil {
		for _, mount := range req.ContainerOverrides.VolumeMounts {
			if mount != nil && mount.ContainerPath == "/" {
				return nil, fmt.Errorf("container_overrides.volume_mounts must not override / because writable rootfs is template-owned")
			}
		}
	}
	volumeMounts := []*cubeboxv1.VolumeMounts{{
		Name:          rootfsWritableVolumeName,
		ContainerPath: "/",
	}}
	if req.ContainerOverrides != nil && len(req.ContainerOverrides.VolumeMounts) > 0 {
		volumeMounts = append(volumeMounts, req.ContainerOverrides.VolumeMounts...)
	}
	containerAnnotations := map[string]string{}
	if req.ContainerOverrides != nil && req.ContainerOverrides.Annotations != nil {
		for k, v := range req.ContainerOverrides.Annotations {
			containerAnnotations[k] = v
		}
	}
	container := &types.Container{
		Name:            "cubebox-name-0",
		Image:           &types.ImageSpec{Image: artifact.ArtifactID, StorageMedia: imagev1.ImageStorageMediaType_ext4.String(), WritableLayerSize: req.WritableLayerSize, Annotations: imageAnnotations},
		Command:         command,
		Args:            args,
		WorkingDir:      workingDir,
		Envs:            envs,
		VolumeMounts:    volumeMounts,
		DnsConfig:       dnsConfigOrNil(req.ContainerOverrides),
		RLimit:          defaultRLimit(req.ContainerOverrides),
		Resources:       resources,
		SecurityContext: securityContext,
		Probe:           probeOrNil(req.ContainerOverrides),
		Annotations:     containerAnnotations,
	}
	return &types.CreateCubeSandboxReq{
		Request:       &types.Request{RequestID: req.RequestID},
		Volumes:       []*types.Volume{rootVolume},
		Containers:    []*types.Container{container},
		Annotations:   annotations,
		InstanceType:  req.InstanceType,
		NetworkType:   req.NetworkType,
		CubeVSContext: cloneCubeVSContext(req.CubeVSContext),
	}, nil
}

func cloneCubeVSContext(in *types.CubeVSContext) *types.CubeVSContext {
	if in == nil {
		return nil
	}
	out := &types.CubeVSContext{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := *in.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	return out
}

func formatTemplateImageCubeVSContext(in *types.CubeVSContext) string {
	if in == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[]"
	}
	allowInternetAccess := "default(true)"
	if in.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", *in.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v", allowInternetAccess, in.AllowOut, in.DenyOut)
}

func distributeRootfsArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
	targets, err := resolveTemplateNodes(req.InstanceType, req.DistributionScope)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	spec := &imagev1.ImageSpec{
		Image:        artifact.ArtifactID,
		StorageMedia: imagev1.ImageStorageMediaType_ext4.String(),
		Annotations: map[string]string{
			constants.CubeAnnotationRootfsArtifactID:        artifact.ArtifactID,
			constants.CubeAnnotationRootfsArtifactURL:       buildDownloadURL(artifact.MasterNodeIP, artifact.ArtifactID, artifact.DownloadToken),
			constants.CubeAnnotationRootfsArtifactToken:     artifact.DownloadToken,
			constants.CubeAnnotationRootfsArtifactSHA256:    artifact.Ext4SHA256,
			constants.CubeAnnotationRootfsArtifactSizeBytes: strconv.FormatInt(artifact.Ext4SizeBytes, 10),
			constants.CubeAnnotationWritableLayerSize:       req.WritableLayerSize,
			constants.CubeAnnotationTemplateSpecFingerprint: artifact.TemplateSpecFingerprint,
			constants.CubeAnnotationsInsType:                req.InstanceType,
		},
	}
	expected := int32(len(targets))
	ready := int32(0)
	failed := int32(0)
	var firstErr error
	var lock sync.Mutex
	sem := make(chan struct{}, defaultDistributionWorkers)
	var wg sync.WaitGroup
	readyTargets := make([]*node.Node, 0, len(targets))
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			replica := buildReplicaForDistribution(target, generatedReq, artifact.ArtifactID, jobID)
			rsp, err := cubelet.CreateImage(ctx, cubelet.GetCubeletAddr(target.HostIP()), &imagev1.CreateImageRequest{
				RequestID: uuid.New().String(),
				Spec:      spec,
			})
			lock.Lock()
			defer lock.Unlock()
			if err != nil {
				failed++
				replica.Phase = ReplicaPhaseFailed
				replica.ErrorMessage = err.Error()
				if firstErr == nil {
					firstErr = err
				}
				if templateID != "" && generatedReq != nil {
					_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				}
				return
			}
			if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
				failed++
				replica.Phase = ReplicaPhaseFailed
				if firstErr == nil {
					if rsp.GetRet() != nil {
						firstErr = fmt.Errorf("cubelet create image on %s failed: %s", target.HostIP(), rsp.GetRet().GetRetMsg())
					} else {
						firstErr = fmt.Errorf("cubelet create image on %s returned empty ret", target.HostIP())
					}
				}
				if rsp.GetRet() != nil {
					replica.ErrorMessage = rsp.GetRet().GetRetMsg()
				} else {
					replica.ErrorMessage = "empty create image response"
				}
				if templateID != "" && generatedReq != nil {
					_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				}
				return
			}
			replica.Phase = ReplicaPhaseDistributed
			replica.CleanupRequired = false
			replica.LastErrorPhase = ""
			replica.ErrorMessage = ""
			ready++
			readyTargets = append(readyTargets, target)
			if templateID != "" && generatedReq != nil {
				_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
			}
		}()
	}
	wg.Wait()
	return readyTargets, expected, ready, failed, firstErr
}

func normalizeTemplateImageRequest(req *types.CreateTemplateFromImageReq) (*types.CreateTemplateFromImageReq, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if req.Request == nil || strings.TrimSpace(req.RequestID) == "" {
		return nil, errors.New("requestID is required")
	}
	if strings.TrimSpace(req.SourceImageRef) == "" {
		return nil, errors.New("source_image_ref is required")
	}
	if strings.TrimSpace(req.WritableLayerSize) == "" {
		return nil, errors.New("writable_layer_size is required")
	}
	cloned := *req
	exposedPorts, err := normalizeTemplateExposedPorts(req.ExposedPorts)
	if err != nil {
		return nil, err
	}
	cloned.ExposedPorts = exposedPorts
	if strings.TrimSpace(cloned.TemplateID) == "" {
		cloned.TemplateID = generateTemplateID()
	}
	if cloned.InstanceType == "" {
		cloned.InstanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	if cloned.NetworkType == "" {
		cloned.NetworkType = cubeboxv1.NetworkType_tap.String()
	}
	return &cloned, nil
}

func buildTemplateSpecFingerprint(req *types.CreateTemplateFromImageReq, sourceImageDigest string) string {
	type fingerprintPayload struct {
		SourceImageDigest  string                    `json:"source_image_digest"`
		WritableLayerSize  string                    `json:"writable_layer_size"`
		ExposedPorts       []int32                   `json:"exposed_ports,omitempty"`
		InstanceType       string                    `json:"instance_type"`
		NetworkType        string                    `json:"network_type"`
		ContainerOverrides *types.ContainerOverrides `json:"container_overrides,omitempty"`
	}
	payload, _ := json.Marshal(fingerprintPayload{
		SourceImageDigest:  sourceImageDigest,
		WritableLayerSize:  req.WritableLayerSize,
		ExposedPorts:       req.ExposedPorts,
		InstanceType:       req.InstanceType,
		NetworkType:        req.NetworkType,
		ContainerOverrides: req.ContainerOverrides,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func dnsConfigOrNil(overrides *types.ContainerOverrides) *types.DNSConfig {
	if overrides == nil {
		return nil
	}
	return overrides.DnsConfig
}

func buildArtifactID(fingerprint string) string {
	return "rfs-" + fingerprint[:24]
}

func marshalTemplateImageJobRequest(req *types.CreateTemplateFromImageReq) (string, error) {
	if req == nil {
		return "", errors.New("request is nil")
	}
	cloned := *req
	cloned.RegistryPassword = ""
	cloned.Request = nil
	payload, err := json.Marshal(&cloned)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func marshalTemplateCommitJobRequest(req *types.CreateCubeSandboxReq) (string, error) {
	if req == nil {
		return "", errors.New("request is nil")
	}
	cloned, err := cloneCreateRequest(req)
	if err != nil {
		return "", err
	}
	cloned.Request = nil
	payload, err := json.Marshal(cloned)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func buildCommitTemplateSpecFingerprint(req *types.CreateCubeSandboxReq) string {
	payload, _ := marshalTemplateCommitJobRequest(req)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func getLatestTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ?", templateID).
		Order("attempt_no desc, id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	return getLatestTemplateImageJobByTemplateID(ctx, templateID)
}

func getActiveTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ? AND status IN ?", templateID, []string{JobStatusPending, JobStatusRunning}).
		Order("attempt_no desc, id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func listTemplateImageJobsByTemplateID(ctx context.Context, templateID string) ([]models.TemplateImageJob, error) {
	var records []models.TemplateImageJob
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ?", templateID).
		Order("attempt_no desc, id desc").Find(&records).Error
	return records, err
}

func getRootfsArtifactByID(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getRootfsArtifactByIDUnscoped(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getRootfsArtifactByFingerprint(ctx context.Context, fingerprint string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("template_spec_fingerprint = ?", fingerprint).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getRootfsArtifactByFingerprintUnscoped(ctx context.Context, fingerprint string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("template_spec_fingerprint = ?", fingerprint).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func updateTemplateImageJob(ctx context.Context, jobID string, values map[string]any) error {
	values["updated_at"] = time.Now()
	tx := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).Updates(values)
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func updateRootfsArtifact(ctx context.Context, artifactID string, values map[string]any) error {
	values["updated_at"] = time.Now()
	tx := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).Updates(values)
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func jobModelToInfo(ctx context.Context, record *models.TemplateImageJob) (*types.TemplateImageJobInfo, error) {
	info := &types.TemplateImageJobInfo{
		JobID:                   record.JobID,
		TemplateID:              record.TemplateID,
		AttemptNo:               record.AttemptNo,
		RetryOfJobID:            record.RetryOfJobID,
		Operation:               record.Operation,
		RedoMode:                record.RedoMode,
		RedoScope:               unmarshalRedoScope(record.RedoScopeJSON),
		ResumePhase:             record.ResumePhase,
		ArtifactID:              record.ArtifactID,
		TemplateSpecFingerprint: record.TemplateSpecFingerprint,
		Status:                  record.Status,
		Phase:                   record.Phase,
		Progress:                record.Progress,
		ErrorMessage:            record.ErrorMessage,
		ExpectedNodeCount:       record.ExpectedNodeCount,
		ReadyNodeCount:          record.ReadyNodeCount,
		FailedNodeCount:         record.FailedNodeCount,
		TemplateStatus:          record.TemplateStatus,
		ArtifactStatus:          record.ArtifactStatus,
	}
	if record.ArtifactID != "" {
		if artifact, err := getRootfsArtifactByID(ctx, record.ArtifactID); err == nil {
			info.Artifact = artifactModelToInfo(artifact)
		}
	}
	return info, nil
}

func artifactModelToInfo(record *models.RootfsArtifact) *types.RootfsArtifactInfo {
	return &types.RootfsArtifactInfo{
		ArtifactID:              record.ArtifactID,
		TemplateSpecFingerprint: record.TemplateSpecFingerprint,
		SourceImageRef:          record.SourceImageRef,
		SourceImageDigest:       record.SourceImageDigest,
		MasterNodeID:            record.MasterNodeID,
		MasterNodeIP:            record.MasterNodeIP,
		Ext4Path:                record.Ext4Path,
		Ext4SHA256:              record.Ext4SHA256,
		Ext4SizeBytes:           record.Ext4SizeBytes,
		WritableLayerSize:       record.WritableLayerSize,
		Status:                  record.Status,
		LastError:               record.LastError,
	}
}

func artifactWorkRootDir() string {
	if value := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR")); value != "" {
		return value
	}
	return filepath.Join(os.TempDir(), "cubemaster-rootfs-artifacts")
}

func artifactStoreRootDir() string {
	if value := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")); value != "" {
		return value
	}
	return defaultArtifactStoreDir
}

func artifactFallbackStoreRootDir() string {
	return filepath.Join(os.TempDir(), fallbackArtifactStoreDir)
}

func artifactStoreDir(artifactID string) string {
	return filepath.Join(artifactStoreRootDir(), artifactID)
}

func resolveArtifactStoreDir(ctx context.Context, artifactID string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")); configured != "" {
		dir := filepath.Join(configured, artifactID)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", fmt.Errorf("prepare configured artifact store root %s failed: %w", configured, err)
		}
		return dir, nil
	}
	primaryDir := artifactStoreDir(artifactID)
	if err := os.MkdirAll(filepath.Dir(primaryDir), 0o755); err == nil {
		return primaryDir, nil
	} else {
		fallbackDir := filepath.Join(artifactFallbackStoreRootDir(), artifactID)
		if fallbackErr := os.MkdirAll(filepath.Dir(fallbackDir), 0o755); fallbackErr == nil {
			log.G(ctx).Warnf("artifact store root %s is unavailable, fallback to %s: %v", artifactStoreRootDir(), artifactFallbackStoreRootDir(), err)
			return fallbackDir, nil
		} else {
			return "", fmt.Errorf("prepare artifact store root %s failed: %w; fallback %s failed: %v", artifactStoreRootDir(), err, artifactFallbackStoreRootDir(), fallbackErr)
		}
	}
}

func dockerLogin(ctx context.Context, configDir, imageRef, username, password string) error {
	registry := registryHostFromImageRef(imageRef)
	cmd := exec.CommandContext(ctx, "docker", "--config", configDir, "login", registry, "-u", username, "--password-stdin")
	cmd.Stdin = strings.NewReader(password)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func dockerRun(ctx context.Context, configDir string, args ...string) error {
	_, err := dockerOutput(ctx, configDir, args...)
	return err
}

func dockerOutput(ctx context.Context, configDir string, args ...string) ([]byte, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if configDir != "" {
		cmdArgs = append(cmdArgs, "--config", configDir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func computeFileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func firstNonEmptyDigest(info dockerInspectImage) string {
	if len(info.RepoDigests) > 0 && info.RepoDigests[0] != "" {
		return info.RepoDigests[0]
	}
	return info.ID
}

func envListToKeyValues(envs []string) []*types.KeyValue {
	if len(envs) == 0 {
		return nil
	}
	out := make([]*types.KeyValue, 0, len(envs))
	for _, env := range envs {
		parts := strings.SplitN(env, "=", 2)
		kv := &types.KeyValue{Key: parts[0]}
		if len(parts) == 2 {
			kv.Value = parts[1]
		}
		out = append(out, kv)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out
}

func defaultRLimit(overrides *types.ContainerOverrides) *types.RLimit {
	if overrides != nil && overrides.RLimit != nil {
		return overrides.RLimit
	}
	return &types.RLimit{NoFile: 1000000}
}

func probeOrNil(overrides *types.ContainerOverrides) *types.Probe {
	if overrides == nil {
		return nil
	}
	return overrides.Probe
}

func buildDownloadURL(baseURL, artifactID, token string) string {
	trimmed := strings.TrimRight(normalizeBaseURL(baseURL), "/")
	if trimmed == "" {
		trimmed = "http://" + artifactRootHostHint()
	}
	u, err := url.Parse(trimmed + "/cube/template/artifact/download")
	if err != nil {
		return trimmed
	}
	query := u.Query()
	query.Set("artifact_id", artifactID)
	query.Set("token", token)
	u.RawQuery = query.Encode()
	return u.String()
}

func normalizeBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed
	}
	return "http://" + trimmed
}

func artifactRootHostHint() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "127.0.0.1"
	}
	return host
}

func registryHostFromImageRef(imageRef string) string {
	parts := strings.Split(imageRef, "/")
	if len(parts) == 0 {
		return "docker.io"
	}
	first := parts[0]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first
	}
	return "docker.io"
}

func quantityToGi(value string) (int64, error) {
	v := strings.TrimSpace(strings.ToLower(value))
	switch {
	case strings.HasSuffix(v, "gi"):
		return strconv.ParseInt(strings.TrimSuffix(v, "gi"), 10, 64)
	case strings.HasSuffix(v, "g"):
		return strconv.ParseInt(strings.TrimSuffix(v, "g"), 10, 64)
	case strings.HasSuffix(v, "mi"):
		mi, err := strconv.ParseInt(strings.TrimSuffix(v, "mi"), 10, 64)
		if err != nil {
			return 0, err
		}
		if mi%1024 == 0 {
			return mi / 1024, nil
		}
		return mi/1024 + 1, nil
	default:
		return strconv.ParseInt(v, 10, 64)
	}
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func normalizeTemplateExposedPorts(ports []int32) ([]int32, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	uniq := make(map[int32]struct{}, len(ports))
	normalized := make([]int32, 0, len(ports))
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("invalid exposed port %d", port)
		}
		if _, exists := uniq[port]; exists {
			continue
		}
		uniq[port] = struct{}{}
		normalized = append(normalized, port)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i] < normalized[j]
	})
	if countCustomTemplateExposedPorts(normalized) > 3 {
		return nil, fmt.Errorf("at most 3 custom exposed ports are supported")
	}
	return normalized, nil
}

func countCustomTemplateExposedPorts(ports []int32) int {
	reserved := defaultTemplateExposedPorts()
	count := 0
	for _, port := range ports {
		if _, ok := reserved[port]; ok {
			continue
		}
		count++
	}
	return count
}

func defaultTemplateExposedPorts() map[int32]struct{} {
	return map[int32]struct{}{
		8080:  {},
		32000: {},
	}
}

func formatExposedPortsAnnotation(ports []int32) string {
	if len(ports) == 0 {
		return ""
	}
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, strconv.FormatInt(int64(port), 10))
	}
	return strings.Join(values, ":")
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func detachTemplateImageJobContext(ctx context.Context, fields map[string]any) context.Context {
	detached := context.Background()
	if rt := CubeLog.GetTraceInfo(ctx); rt != nil {
		detached = CubeLog.WithRequestTrace(detached, rt.DeepCopy())
	}
	return log.WithLogger(detached, log.G(ctx).WithFields(fields))
}

func ensureArtifactBuildPreflight(ctx context.Context) error {
	requiredCommands := []string{"docker", "mkfs.ext4", "tar", "truncate", "cp"}
	for _, cmd := range requiredCommands {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("required command %q is not available on cubemaster node", cmd)
		}
	}
	output, err := exec.CommandContext(ctx, "mkfs.ext4", "-h").CombinedOutput()
	helpText := string(output)
	if err != nil && helpText == "" {
		return fmt.Errorf("failed to probe mkfs.ext4 help output: %w", err)
	}
	if !strings.Contains(helpText, "-d") {
		return fmt.Errorf("mkfs.ext4 on cubemaster node does not appear to support the -d option required for rootfs image creation")
	}
	return nil
}

func cleanupIntermediateArtifacts(workDir, storeRootfsDir, storeDir string, keepStoreDir bool) {
	if workDir != "" {
		_ = os.RemoveAll(workDir) // NOCC:Path Traversal()
	}
	if !keepStoreDir {
		if storeDir != "" {
			_ = os.RemoveAll(storeDir) // NOCC:Path Traversal()
		}
		return
	}
	if storeRootfsDir != "" {
		_ = os.RemoveAll(storeRootfsDir) // NOCC:Path Traversal()
	}
}

func relocateRootfsToArtifactStore(ctx context.Context, srcRootfsDir, dstRootfsDir string) error {
	if err := os.RemoveAll(dstRootfsDir); err != nil { // NOCC:Path Traversal()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstRootfsDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcRootfsDir, dstRootfsDir); err == nil {
		return nil
	} else if !isCrossDeviceRenameErr(err) {
		return err
	}
	if err := runCommand(ctx, "", "cp", "-a", srcRootfsDir, dstRootfsDir); err != nil {
		return fmt.Errorf("copy rootfs to artifact store failed: %w", err)
	}
	return os.RemoveAll(srcRootfsDir) // NOCC:Path Traversal()
}

func isCrossDeviceRenameErr(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return errors.Is(err, syscall.EXDEV)
}

func cleanupFailedRootfsArtifact(ctx context.Context, artifact *models.RootfsArtifact, instanceType string) error {
	if artifact == nil {
		return nil
	}
	var cleanupErr error
	if err := cleanupDistributedArtifact(ctx, artifact.ArtifactID, instanceType); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := cleanupLocalRootfsArtifact(artifact.ArtifactID, artifact.Ext4Path); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr == nil {
		if err := deleteRootfsArtifactRecord(ctx, artifact.ArtifactID); err == nil {
			return nil
		} else {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	updateErr := updateRootfsArtifact(ctx, artifact.ArtifactID, map[string]any{
		"status":     ArtifactStatusFailed,
		"last_error": fmt.Sprintf("artifact cleanup incomplete: %v", cleanupErr),
	})
	return errors.Join(cleanupErr, updateErr)
}

func cleanupLocalRootfsArtifact(artifactID, ext4Path string) error {
	if ext4Path == "" {
		return nil
	}
	if dir, ok := managedArtifactDir(artifactID, ext4Path); ok {
		return os.RemoveAll(dir) // NOCC:Path Traversal()
	}
	if err := os.Remove(ext4Path); err != nil && !errors.Is(err, os.ErrNotExist) { // NOCC:Path Traversal()
		return err
	}
	return nil
}

func managedArtifactDir(artifactID, ext4Path string) (string, bool) {
	if strings.TrimSpace(artifactID) == "" || strings.TrimSpace(ext4Path) == "" {
		return "", false
	}
	dir := filepath.Clean(filepath.Dir(ext4Path))
	if filepath.Base(dir) != artifactID {
		return "", false
	}
	roots := []string{artifactWorkRootDir(), artifactStoreRootDir()}
	if strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")) == "" {
		roots = append(roots, artifactFallbackStoreRootDir())
	}
	for _, root := range roots {
		rel, err := filepath.Rel(filepath.Clean(root), dir)
		if err != nil {
			continue
		}
		if rel == artifactID {
			return dir, true
		}
	}
	return "", false
}
