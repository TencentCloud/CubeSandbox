// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"gorm.io/gorm"
)

const (
	DefaultTemplateVersion              = "v2"
	snapshotRuntimeRefReleasedByDestroy = "sandbox destroyed"

	StatusPending        = "PENDING"
	StatusReady          = "READY"
	StatusPartiallyReady = "PARTIALLY_READY"
	StatusFailed         = "FAILED"
	StatusCreating       = "CREATING"
	StatusDeleting       = "DELETING"

	TemplateKindTemplate = "template"
	TemplateKindSnapshot = "snapshot"

	StorageBackendCow = "cubecow"

	ReplicaStatusReady  = "READY"
	ReplicaStatusFailed = "FAILED"

	ReplicaPhasePending      = "PENDING"
	ReplicaPhaseDistributing = "DISTRIBUTING"
	ReplicaPhaseDistributed  = "DISTRIBUTED"
	ReplicaPhaseSnapshotting = "SNAPSHOTTING"
	ReplicaPhaseReady        = "READY"
	ReplicaPhaseFailed       = "FAILED"
	ReplicaPhaseCleaning     = "CLEANING"
)

var (
	ErrTemplateStoreNotInitialized = errors.New("template store is not initialized")
	ErrTemplateNotFound            = errors.New("template not found")
	ErrTemplateIDRequired          = errors.New("template id is required")
	ErrTemplateHasNoReadyReplica   = errors.New("template has no ready replica")
	ErrNoTemplateNodes             = errors.New("no healthy nodes available for template creation")
	ErrDuplicateTemplate           = errors.New("template already exists")
	ErrTemplateAttemptInProgress   = errors.New("template attempt is already in progress")
)

type localStore struct {
	db     *gorm.DB
	dbAddr string
}

var (
	store     = &localStore{}
	storeOnce sync.Once
)

// ReplicaStatus is the master-side, control-plane view of a template replica
// on a given node. v5: physical fields (rootfs_vol, memory_vol, snapshot_path,
// meta_dir, build_rootfs_vol, rootfs_kind, memory_kind, rootfs_dev,
// memory_dev) were removed because Cubelet's local snapshot catalog is the
// single source of truth, queried by templateID/snapshotID at restore/cleanup
// time.
type ReplicaStatus struct {
	NodeID          string `json:"node_id"`
	NodeIP          string `json:"node_ip"`
	InstanceType    string `json:"instance_type,omitempty"`
	Spec            string `json:"spec,omitempty"`
	Status          string `json:"status"`
	Phase           string `json:"phase,omitempty"`
	ArtifactID      string `json:"artifact_id,omitempty"`
	LastJobID       string `json:"last_job_id,omitempty"`
	LastErrorPhase  string `json:"last_error_phase,omitempty"`
	CleanupRequired bool   `json:"cleanup_required,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
}

type TemplateInfo struct {
	TemplateID                string          `json:"template_id"`
	InstanceType              string          `json:"instance_type,omitempty"`
	Version                   string          `json:"version,omitempty"`
	Status                    string          `json:"status"`
	Kind                      string          `json:"kind,omitempty"`
	OriginSandboxID           string          `json:"origin_sandbox_id,omitempty"`
	OriginNodeID              string          `json:"origin_node_id,omitempty"`
	DisplayName               string          `json:"display_name,omitempty"`
	StorageBackend            string          `json:"storage_backend,omitempty"`
	Retain                    bool            `json:"retain,omitempty"`
	RootfsSizeBytesAtSnapshot uint64          `json:"rootfs_size_bytes_at_snapshot,omitempty"`
	LastError                 string          `json:"last_error,omitempty"`
	CreatedAt                 string          `json:"created_at,omitempty"`
	ImageInfo                 string          `json:"image_info,omitempty"`
	Replicas                  []ReplicaStatus `json:"replicas,omitempty"`
}

func templateInfoFromDefinition(def models.TemplateDefinition) TemplateInfo {
	return TemplateInfo{
		TemplateID:                def.TemplateID,
		InstanceType:              def.InstanceType,
		Version:                   def.Version,
		Status:                    def.Status,
		Kind:                      def.Kind,
		OriginSandboxID:           def.OriginSandboxID,
		OriginNodeID:              def.OriginNodeID,
		DisplayName:               def.DisplayName,
		StorageBackend:            def.StorageBackend,
		Retain:                    def.Retain,
		RootfsSizeBytesAtSnapshot: def.RootfsSizeBytesAtSnapshot,
		LastError:                 def.LastError,
	}
}

type replicaRunOptions struct {
	ArtifactID string
	JobID      string
}

type definitionCreateOptions struct {
	Kind                      string
	OriginSandboxID           string
	OriginNodeID              string
	DisplayName               string
	StorageBackend            string
	Retain                    bool
	RootfsSizeBytesAtSnapshot uint64
}

func ListTemplates(ctx context.Context) ([]TemplateInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	var defs []models.TemplateDefinition
	if err := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Order("updated_at desc").Find(&defs).Error; err != nil {
		return nil, err
	}
	var jobs []models.TemplateImageJob
	if err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Order("template_id asc, attempt_no desc, id desc").Find(&jobs).Error; err != nil {
		return nil, err
	}
	latestJobByTemplateID := make(map[string]*models.TemplateImageJob, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		if _, exists := latestJobByTemplateID[job.TemplateID]; exists {
			continue
		}
		latestJobByTemplateID[job.TemplateID] = job
	}

	out := make([]TemplateInfo, 0, len(defs))
	for _, def := range defs {
		imageInfo := extractImageInfoFromRequestJSON(def.RequestJSON)
		if latestJob := latestJobByTemplateID[def.TemplateID]; latestJob != nil {
			imageInfo = composeImageInfo(latestJob.SourceImageRef, latestJob.SourceImageDigest)
		}
		info := templateInfoFromDefinition(def)
		info.CreatedAt = formatUTCRFC3339(def.CreatedAt)
		info.ImageInfo = imageInfo
		out = append(out, info)
	}
	seen := make(map[string]struct{}, len(out))
	for _, item := range out {
		seen[item.TemplateID] = struct{}{}
	}
	for _, job := range jobs {
		if _, ok := seen[job.TemplateID]; ok {
			continue
		}
		out = append(out, templateInfoFromJob(&job))
		seen[job.TemplateID] = struct{}{}
	}
	return out, nil
}

func Init(ctx context.Context) error {
	_ = ctx
	if config.GetInstanceConfig() == nil {
		return ErrTemplateStoreNotInitialized
	}
	var initErr error
	storeOnce.Do(func() {
		// Schema is owned by pkg/base/dao/migrate and applied in main.go
		// before any business package Init runs; here we only attach to
		// the existing *gorm.DB.
		store.db = db.Init(config.GetInstanceConfig())
		store.dbAddr = config.GetInstanceConfig().Addr
		if initErr = sandboxspec.Init(store.db); initErr != nil {
			return
		}
		configureSnapshotRuntimeRefHooks()
		configureSandboxSpecHooks()
		if warmErr := warmReadyTemplateLocality(ctx); warmErr != nil {
			log.G(ctx).Warnf("warm ready template locality fail:%v", warmErr)
		}
		startSnapshotReconciler(ctx)
	})
	return initErr
}

func configureSnapshotRuntimeRefHooks() {
	releaseBySandboxID := func(ctx context.Context, sandboxID string) error {
		errReleasingRefs := ReleaseSnapshotRuntimeRefsBySandbox(ctx, sandboxID, snapshotRuntimeRefReleasedByDestroy)
		errDeletingSpec := sandboxspec.Delete(ctx, sandboxID)
		if errReleasingRefs != nil && errDeletingSpec != nil {
			return errors.Join(errReleasingRefs, errDeletingSpec)
		}
		if errReleasingRefs != nil {
			return errReleasingRefs
		}
		if errDeletingSpec != nil && !errors.Is(errDeletingSpec, sandboxspec.ErrSandboxSpecNotFound) {
			return errDeletingSpec
		}
		return nil
	}
	sandbox.SetAfterDestroySandboxSuccessHook(releaseBySandboxID)
	task.SetAfterDestroyTaskSuccessHook(releaseBySandboxID)
}

// configureSandboxSpecHooks wires sandbox create success to the canonical
// spec store. Failures are swallowed by the sandbox layer (logged only); we
// still surface them here so future callers of the hook can react.
func configureSandboxSpecHooks() {
	sandbox.SetAfterCreateSandboxSuccessHook(func(ctx context.Context, sandboxID, hostID, hostIP string, req *sandboxtypes.CreateCubeSandboxReq) error {
		return sandboxspec.Put(ctx, sandboxID, req, sandboxspec.PutOptions{
			HostID: hostID,
			HostIP: hostIP,
		})
	})
}

func isReady() bool {
	return store.db != nil
}

func NormalizeRequest(req *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, string, error) {
	if req == nil {
		return nil, "", errors.New("request is nil")
	}
	cloned, err := cloneCreateRequest(req)
	if err != nil {
		return nil, "", err
	}
	if cloned.Annotations == nil {
		cloned.Annotations = make(map[string]string)
	}
	if cloned.Labels == nil {
		cloned.Labels = make(map[string]string)
	}
	templateID := strings.TrimSpace(cloned.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	if templateID == "" {
		templateID = generateTemplateID()
	}
	cloned.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = templateID
	cloned.Annotations[constants.CubeAnnotationsAppSnapshotCreate] = "true"
	if cloned.InstanceType == "" {
		cloned.InstanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	version := constants.GetAppSnapshotVersion(cloned.Annotations)
	if version == "" {
		version = DefaultTemplateVersion
	}
	constants.SetAppSnapshotVersion(cloned.Annotations, version)
	return cloned, templateID, nil
}

func generateTemplateID() string {
	return "tpl-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
}

func normalizeStoredTemplateRequest(req *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, error) {
	cloned, templateID, err := NormalizeRequest(req)
	if err != nil {
		return nil, err
	}
	delete(cloned.Annotations, constants.CubeAnnotationsAppSnapshotCreate)
	cloned.SnapshotDir = ""
	cloned.Timeout = 0
	cloned.InsId = ""
	cloned.InsIp = ""
	cloned.Request = nil
	// v4+: runtime-binding annotations are per-invocation and owned by
	// cubelet's local catalog. Strip them from the stored template request
	// so future restores/snapshots cannot drag a stale logical id or
	// attachment timestamp into the new sandbox's request envelope. Physical
	// memory-vol annotations were removed entirely in v5 (the constants no
	// longer exist).
	delete(cloned.Annotations, constants.CubeAnnotationRuntimeSnapshotID)
	delete(cloned.Annotations, constants.CubeAnnotationRuntimeSnapshotAttachedAt)
	cloned.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = templateID
	return cloned, nil
}

func CreateTemplate(ctx context.Context, req *sandboxtypes.CreateCubeSandboxReq) (info *TemplateInfo, err error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	createReq, templateID, err := NormalizeRequest(req)
	if err != nil {
		return nil, err
	}
	storedReq, err := normalizeStoredTemplateRequest(req)
	if err != nil {
		return nil, err
	}
	definitionCreated := false
	defer func() {
		if err == nil || !definitionCreated {
			return
		}
		if cleanupErr := cleanupTemplateReplicas(ctx, templateID); cleanupErr != nil {
			log.G(ctx).Errorf("cleanup failed template replicas fail, template=%s err=%v", templateID, cleanupErr)
			err = errors.Join(err, cleanupErr)
		}
		if cleanupErr := cleanupTemplateMetadata(ctx, templateID); cleanupErr != nil {
			log.G(ctx).Errorf("cleanup failed template metadata fail, template=%s err=%v", templateID, cleanupErr)
			err = errors.Join(err, cleanupErr)
		}
		invalidateTemplateCaches(templateID)
	}()
	if err = withTemplateWriteLock(templateID, func() error {
		if err := createDefinition(ctx, templateID, storedReq, createReq.InstanceType,
			constants.GetAppSnapshotVersion(createReq.Annotations)); err != nil {
			return err
		}
		definitionCreated = true
		if cacheErr := setTemplateRequestCache(templateID, storedReq); cacheErr != nil {
			log.G(ctx).Warnf("set template request cache fail, template=%s err=%v", templateID, cacheErr)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	nodes, err := resolveTemplateNodes(createReq.InstanceType, createReq.DistributionScope)
	if err != nil {
		return nil, err
	}

	replicas, persistErr := createTemplateReplicasOnNodes(ctx, templateID, createReq, nodes, replicaRunOptions{})
	if persistErr != nil {
		return nil, persistErr
	}
	return finalizeTemplateReplicas(ctx, templateID, createReq.InstanceType, constants.GetAppSnapshotVersion(createReq.Annotations), replicas)
}

func healthyTemplateNodes(instanceType string) []*node.Node {
	nodes := localcache.GetHealthyNodesByInstanceType(-1, instanceType)
	out := make([]*node.Node, 0, nodes.Len())
	for i := range nodes {
		out = append(out, nodes[i])
	}
	return out
}

func createTemplateReplicasOnNodes(ctx context.Context, templateID string, req *sandboxtypes.CreateCubeSandboxReq, targets []*node.Node, opts replicaRunOptions) ([]ReplicaStatus, error) {
	replicas := make([]ReplicaStatus, 0, len(targets))
	var lock sync.Mutex
	var persistErr error
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for _, target := range targets {
		target := target
		if target == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			replica := createReplicaOnNode(ctx, target, req, opts)
			lock.Lock()
			replicas = append(replicas, replica)
			lock.Unlock()

			if upsertErr := UpsertReplica(ctx, templateID, req.InstanceType, replica); upsertErr != nil {
				lock.Lock()
				persistErr = errors.Join(persistErr, fmt.Errorf("upsert template replica fail, template=%s node=%s: %w", templateID, target.ID(), upsertErr))
				lock.Unlock()
				log.G(ctx).Errorf("upsert template replica fail, template=%s node=%s err=%v", templateID, target.ID(), upsertErr)
			}
		}()
	}
	wg.Wait()
	return replicas, persistErr
}

func createReplicaOnNode(ctx context.Context, target *node.Node, req *sandboxtypes.CreateCubeSandboxReq, opts replicaRunOptions) ReplicaStatus {
	replica := ReplicaStatus{
		NodeID:          target.ID(),
		NodeIP:          target.HostIP(),
		InstanceType:    req.InstanceType,
		Spec:            calculateRequestSpec(req),
		Status:          ReplicaStatusFailed,
		Phase:           ReplicaPhaseSnapshotting,
		ArtifactID:      opts.ArtifactID,
		LastJobID:       opts.JobID,
		LastErrorPhase:  ReplicaPhaseSnapshotting,
		CleanupRequired: true,
	}
	nodeReq, err := cloneCreateRequest(req)
	if err != nil {
		replica.Phase = ReplicaPhaseFailed
		replica.ErrorMessage = err.Error()
		return replica
	}
	ensureRuntimeTemplateRequest(nodeReq)
	cubeletReq, err := sandbox.ConstructCubeletReq(ctx, nodeReq)
	if err != nil {
		replica.Phase = ReplicaPhaseFailed
		replica.ErrorMessage = err.Error()
		return replica
	}
	rsp, err := cubelet.AppSnapshot(ctx, cubelet.GetCubeletAddr(target.HostIP()), &cubeboxv1.AppSnapshotRequest{
		CreateRequest: cubeletReq,
		SnapshotDir:   req.SnapshotDir,
	})
	if err != nil {
		replica.Phase = ReplicaPhaseFailed
		replica.ErrorMessage = err.Error()
		return replica
	}
	if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		replica.Phase = ReplicaPhaseFailed
		if rsp.GetRet() != nil {
			replica.ErrorMessage = rsp.GetRet().GetRetMsg()
		} else {
			replica.ErrorMessage = "empty appsnapshot response"
		}
		return replica
	}
	replica.Status = ReplicaStatusReady
	replica.Phase = ReplicaPhaseReady
	// v4: AppSnapshot replica is "thin" -- physical refs are owned by cubelet's
	// local catalog. Master only persists control-plane state (status / phase /
	// last job / error) so we deliberately ignore SnapshotPath/RootfsVol/
	// MemoryVol/RootfsKind/MemoryKind in the RPC response here.
	replica.LastErrorPhase = ""
	replica.CleanupRequired = false
	replica.ErrorMessage = ""
	return replica
}

func summarizeStatus(replicas []ReplicaStatus) (status string, lastError string) {
	successes := 0
	failures := 0
	for _, replica := range replicas {
		if replica.Status == ReplicaStatusReady {
			successes++
			continue
		}
		failures++
		if lastError == "" {
			lastError = replica.ErrorMessage
		}
	}
	switch {
	case successes == 0:
		return StatusFailed, lastError
	case failures == 0:
		return StatusReady, ""
	default:
		return StatusPartiallyReady, lastError
	}
}

func ensureRuntimeTemplateRequest(req *sandboxtypes.CreateCubeSandboxReq) {
	if req == nil {
		return
	}
	if req.Request == nil {
		req.Request = &sandboxtypes.Request{}
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = uuid.NewString()
	}
}

func refreshTemplateReplicaSummary(ctx context.Context, templateID string) error {
	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return err
	}
	current := make([]ReplicaStatus, 0, len(replicas))
	for _, replica := range replicas {
		current = append(current, replicaModelToStatus(replica))
	}
	status, lastError := summarizeStatus(current)
	if err := UpdateDefinitionStatus(ctx, templateID, status, lastError); err != nil {
		return err
	}
	localcache.InvalidateImageState(templateID)
	setTemplateLocalityCache(templateID, current)
	registerReadyTemplateReplicas(templateID, current)
	return nil
}

func createDefinition(ctx context.Context, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string) error {
	payload, err := json.Marshal(storedReq)
	if err != nil {
		return err
	}
	model := &models.TemplateDefinition{
		TemplateID:   templateID,
		InstanceType: instanceType,
		Version:      version,
		Status:       StatusPending,
		Kind:         TemplateKindTemplate,
		RequestJSON:  string(payload),
	}
	if err = store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).Create(model).Error; err != nil {
		if strings.Contains(err.Error(), "1062") || strings.Contains(err.Error(), "Duplicate entry") {
			return ErrDuplicateTemplate
		}
		return err
	}
	return nil
}

func createDefinitionWithOptions(ctx context.Context, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string, opts definitionCreateOptions) error {
	return createDefinitionTx(ctx, store.db.WithContext(ctx), templateID, storedReq, instanceType, version, opts)
}

func ensureTemplateDefinition(ctx context.Context, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string) (bool, error) {
	if _, err := GetDefinition(ctx, templateID); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrTemplateNotFound) {
		return false, err
	}
	if err := createDefinition(ctx, templateID, storedReq, instanceType, version); err != nil {
		return false, err
	}
	if cacheErr := setTemplateRequestCache(templateID, storedReq); cacheErr != nil {
		log.G(ctx).Warnf("set template request cache fail, template=%s err=%v", templateID, cacheErr)
	}
	return true, nil
}

func finalizeTemplateReplicas(ctx context.Context, templateID, instanceType, version string, replicas []ReplicaStatus) (*TemplateInfo, error) {
	setTemplateLocalityCache(templateID, replicas)
	registerReadyTemplateReplicas(templateID, replicas)

	status, lastError := summarizeStatus(replicas)
	if err := UpdateDefinitionStatus(ctx, templateID, status, lastError); err != nil {
		return nil, err
	}
	info := &TemplateInfo{
		TemplateID:   templateID,
		InstanceType: instanceType,
		Version:      version,
		Status:       status,
		LastError:    lastError,
		Replicas:     replicas,
	}
	if status == StatusFailed {
		if lastError == "" {
			lastError = "template creation failed on all nodes"
		}
		return info, fmt.Errorf("template %s creation failed: %s", templateID, lastError)
	}
	return info, nil
}

func UpdateDefinitionStatus(ctx context.Context, templateID, status, lastError string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	return store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id = ?", templateID).
		Updates(map[string]any{
			"status":     status,
			"last_error": lastError,
			"updated_at": time.Now(),
		}).Error
}

func GetTemplateInfo(ctx context.Context, templateID string) (*TemplateInfo, error) {
	def, err := GetDefinition(ctx, templateID)
	if err != nil {
		if !errors.Is(err, ErrTemplateNotFound) {
			return nil, err
		}
		job, jobErr := getLatestTemplateImageJobByTemplateID(ctx, templateID)
		if jobErr != nil {
			return nil, err
		}
		info := templateInfoFromJob(job)
		return &info, nil
	}
	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return nil, err
	}
	info := templateInfoFromDefinition(*def)
	out := &info
	out.Replicas = make([]ReplicaStatus, 0, len(replicas))
	for _, replica := range replicas {
		out.Replicas = append(out.Replicas, replicaModelToStatus(replica))
	}
	return out, nil
}

func templateInfoFromJob(job *models.TemplateImageJob) TemplateInfo {
	if job == nil {
		return TemplateInfo{}
	}
	status := strings.ToUpper(job.Status)
	if job.TemplateStatus != "" {
		status = job.TemplateStatus
	}
	if status == "" {
		status = JobStatusPending
	}
	return TemplateInfo{
		TemplateID:   job.TemplateID,
		InstanceType: job.InstanceType,
		Version:      DefaultTemplateVersion,
		Status:       status,
		LastError:    job.ErrorMessage,
		CreatedAt:    formatUTCRFC3339(job.CreatedAt),
		ImageInfo:    composeImageInfo(job.SourceImageRef, job.SourceImageDigest),
	}
}

func formatUTCRFC3339(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func composeImageInfo(ref, digest string) string {
	imageRef := strings.TrimSpace(ref)
	imageDigest := strings.TrimSpace(digest)
	if imageRef == "" {
		return ""
	}
	if imageDigest == "" {
		return imageRef
	}
	// Tolerate historical rows where SourceImageDigest was stored as a
	// full canonical reference ("name@sha256:..."). Strip the "name@"
	// prefix so we never produce "name:tag@name@sha256:...".
	if at := strings.Index(imageDigest, "@"); at >= 0 && at+1 < len(imageDigest) {
		imageDigest = imageDigest[at+1:]
	}
	if strings.Contains(imageRef, "@") {
		return imageRef
	}
	return imageRef + "@" + imageDigest
}

func extractImageInfoFromRequestJSON(payload string) string {
	if strings.TrimSpace(payload) == "" {
		return ""
	}
	req := &sandboxtypes.CreateCubeSandboxReq{}
	if err := json.Unmarshal([]byte(payload), req); err != nil {
		return ""
	}
	for _, ctr := range req.Containers {
		if ctr == nil || ctr.Image == nil {
			continue
		}
		ref := strings.TrimSpace(ctr.Image.Image)
		if ref == "" {
			continue
		}
		digest := ""
		if at := strings.LastIndex(ref, "@"); at >= 0 && at+1 < len(ref) {
			digest = strings.TrimSpace(ref[at+1:])
		}
		return composeImageInfo(ref, digest)
	}
	return ""
}

func GetDefinition(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	def := &models.TemplateDefinition{}
	err := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id = ?", templateID).First(def).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTemplateNotFound
		}
		return nil, err
	}
	return def, nil
}

func GetTemplateRequest(ctx context.Context, templateID string) (*sandboxtypes.CreateCubeSandboxReq, error) {
	cacheStart := time.Now()
	if req, hit, err := getCachedTemplateRequest(templateID); err != nil {
		return nil, err
	} else if hit {
		reportTemplateCacheMetric(ctx, constants.ActionTemplateCacheHit, time.Since(cacheStart))
		ensureRuntimeTemplateRequest(req)
		return req, nil
	}
	reportTemplateCacheMetric(ctx, constants.ActionTemplateCacheMiss, time.Since(cacheStart))

	v, err := templateRequestFetchGroup.Do(templateID, func() (interface{}, error) {
		var req *sandboxtypes.CreateCubeSandboxReq
		err := withTemplateReadLock(templateID, func() error {
			dbStart := time.Now()
			def, err := GetDefinition(ctx, templateID)
			reportTemplateMetric(ctx, constants.MySQL, store.dbAddr, constants.ActionTemplateGetDefinition, time.Since(dbStart), 0)
			if err != nil {
				return err
			}
			req = &sandboxtypes.CreateCubeSandboxReq{}
			if err = json.Unmarshal([]byte(def.RequestJSON), req); err != nil {
				return err
			}
			if req.Annotations == nil {
				req.Annotations = make(map[string]string)
			}
			constants.NormalizeAppSnapshotAnnotations(req.Annotations)
			if err = setTemplateRequestCache(templateID, req); err != nil {
				log.G(ctx).Warnf("set template request cache fail, template=%s err=%v", templateID, err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	req, ok := v.(*sandboxtypes.CreateCubeSandboxReq)
	if !ok || req == nil {
		return nil, errors.New("invalid template request cache entry")
	}
	cloned, err := cloneCreateRequest(req)
	if err != nil {
		return nil, err
	}
	ensureRuntimeTemplateRequest(cloned)
	return cloned, nil
}

func ListReplicas(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	var replicas []models.TemplateReplica
	err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ?", templateID).
		Order("node_id asc").Find(&replicas).Error
	return replicas, err
}

func replicaModelToStatus(replica models.TemplateReplica) ReplicaStatus {
	return ReplicaStatus{
		NodeID:          replica.NodeID,
		NodeIP:          replica.NodeIP,
		InstanceType:    replica.InstanceType,
		Spec:            replica.Spec,
		Status:          replica.Status,
		Phase:           replica.Phase,
		ArtifactID:      replica.ArtifactID,
		LastJobID:       replica.LastJobID,
		LastErrorPhase:  replica.LastErrorPhase,
		CleanupRequired: replica.CleanupRequired,
		ErrorMessage:    replica.ErrorMessage,
	}
}

func replicaStatusToModel(templateID, instanceType string, replica ReplicaStatus) *models.TemplateReplica {
	return &models.TemplateReplica{
		TemplateID:      templateID,
		NodeID:          replica.NodeID,
		NodeIP:          replica.NodeIP,
		InstanceType:    instanceType,
		Spec:            replica.Spec,
		Status:          replica.Status,
		Phase:           replica.Phase,
		ArtifactID:      replica.ArtifactID,
		LastJobID:       replica.LastJobID,
		LastErrorPhase:  replica.LastErrorPhase,
		CleanupRequired: replica.CleanupRequired,
		ErrorMessage:    replica.ErrorMessage,
	}
}

func replicaStatusUpdateFields(instanceType string, replica ReplicaStatus) map[string]any {
	return map[string]any{
		"node_ip":          replica.NodeIP,
		"instance_type":    instanceType,
		"spec":             replica.Spec,
		"status":           replica.Status,
		"phase":            replica.Phase,
		"artifact_id":      replica.ArtifactID,
		"last_job_id":      replica.LastJobID,
		"last_error_phase": replica.LastErrorPhase,
		"cleanup_required": replica.CleanupRequired,
		"error_message":    replica.ErrorMessage,
		"updated_at":       time.Now(),
	}
}

func UpsertReplica(ctx context.Context, templateID, instanceType string, replica ReplicaStatus) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	record := &models.TemplateReplica{}
	dbq := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", templateID, replica.NodeID)
	err := dbq.First(record).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
			Create(replicaStatusToModel(templateID, instanceType, replica)).Error
	}
	return dbq.Updates(replicaStatusUpdateFields(instanceType, replica)).Error
}

func EnsureReadyReplica(ctx context.Context, templateID string) error {
	if _, err := GetDefinition(ctx, templateID); err != nil {
		return err
	}
	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return err
	}
	for _, replica := range replicas {
		if replica.Status == ReplicaStatusReady {
			return nil
		}
	}
	return ErrTemplateHasNoReadyReplica
}

func ResolveTemplateReadyReplica(ctx context.Context, templateID, preferredNodeID string) (ReplicaStatus, error) {
	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return ReplicaStatus{}, err
	}
	preferredNodeID = strings.TrimSpace(preferredNodeID)
	for _, item := range replicas {
		replica := replicaModelToStatus(item)
		if replica.Status != ReplicaStatusReady {
			continue
		}
		if preferredNodeID == "" || strings.TrimSpace(replica.NodeID) == preferredNodeID {
			return replica, nil
		}
	}
	return ReplicaStatus{}, ErrTemplateHasNoReadyReplica
}

func EnsureTemplateLocalityReady(ctx context.Context, templateID, instanceType string) error {
	start := time.Now()
	defer func() {
		reportTemplateMetric(ctx, constants.CubeMasterTemplateID, constants.CubeMasterTemplateID, constants.ActionTemplateLocality, time.Since(start), 0)
	}()
	nodes := localcache.GetHealthyNodesByInstanceType(-1, instanceType)
	healthyNodeIDs := make(map[string]struct{}, len(nodes))
	healthyNodeIPs := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		if localcache.GetImageStateByNode(templateID, nodes[i].ID()) != nil {
			reportTemplateCacheMetric(ctx, constants.ActionTemplateLocalityHit, 0)
			return nil
		}
		healthyNodeIDs[nodes[i].ID()] = struct{}{}
		if hostIP := strings.TrimSpace(nodes[i].HostIP()); hostIP != "" {
			healthyNodeIPs[hostIP] = struct{}{}
		}
	}
	if replicas, ok := getCachedTemplateLocality(templateID); ok {
		for _, replica := range replicas {
			if _, matchNodeID := healthyNodeIDs[replica.NodeID]; matchNodeID {
				registerReadyTemplateReplicas(templateID, replicas)
				reportTemplateCacheMetric(ctx, constants.ActionTemplateLocalityHit, 0)
				return nil
			}
			if _, matchNodeIP := healthyNodeIPs[replica.NodeIP]; matchNodeIP {
				registerReadyTemplateReplicas(templateID, replicas)
				reportTemplateCacheMetric(ctx, constants.ActionTemplateLocalityHit, 0)
				return nil
			}
		}
	}
	reportTemplateCacheMetric(ctx, constants.ActionTemplateLocalityMiss, 0)
	if isReady() {
		matched := false
		err := withTemplateReadLock(templateID, func() error {
			dbStart := time.Now()
			replicas, err := ListReplicas(ctx, templateID)
			reportTemplateMetric(ctx, constants.MySQL, store.dbAddr, constants.ActionTemplateReplicaFallback, time.Since(dbStart), 0)
			if err != nil {
				return err
			}
			readyReplicas := make([]ReplicaStatus, 0, len(replicas))
			for _, replica := range replicas {
				if replica.Status != ReplicaStatusReady {
					continue
				}
				readyReplicas = append(readyReplicas, replicaModelToStatus(replica))
				if _, ok := healthyNodeIDs[replica.NodeID]; ok {
					matched = true
				}
				if _, ok := healthyNodeIPs[replica.NodeIP]; ok {
					matched = true
				}
			}
			setTemplateLocalityCache(templateID, readyReplicas)
			registerReadyTemplateReplicas(templateID, readyReplicas)
			return nil
		})
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
	}
	return ErrTemplateHasNoReadyReplica
}

func warmReadyTemplateLocality(ctx context.Context) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	var replicas []models.TemplateReplica
	if err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("status = ?", ReplicaStatusReady).
		Find(&replicas).Error; err != nil {
		return err
	}
	replicasByTemplate := make(map[string][]ReplicaStatus)
	for _, replica := range replicas {
		replicasByTemplate[replica.TemplateID] = append(replicasByTemplate[replica.TemplateID], replicaModelToStatus(replica))
	}
	for templateID, readyReplicas := range replicasByTemplate {
		setTemplateLocalityCache(templateID, readyReplicas)
		registerReadyTemplateReplicas(templateID, readyReplicas)
	}
	return nil
}

func cloneCreateRequest(req *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	out := &sandboxtypes.CreateCubeSandboxReq{}
	if err = json.Unmarshal(payload, out); err != nil {
		return nil, err
	}
	return out, nil
}

func calculateRequestSpec(req *sandboxtypes.CreateCubeSandboxReq) string {
	if req == nil || len(req.Containers) == 0 {
		return ""
	}
	var cpuParts []string
	var memParts []string
	for _, ctr := range req.Containers {
		if ctr == nil || ctr.Resources == nil {
			continue
		}
		if ctr.Resources.Cpu != "" {
			cpuParts = append(cpuParts, ctr.Resources.Cpu)
		}
		if ctr.Resources.Mem != "" {
			memParts = append(memParts, ctr.Resources.Mem)
		}
	}
	return fmt.Sprintf("cpu=%s,mem=%s", strings.Join(cpuParts, "+"), strings.Join(memParts, "+"))
}

func ResolveTemplate(ctx context.Context, reqInOut *sandboxtypes.CreateCubeSandboxReq) error {
	if reqInOut == nil || reqInOut.Annotations == nil {
		return nil
	}
	templateID := strings.TrimSpace(reqInOut.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	if templateID == "" {
		return nil
	}
	if constants.GetAppSnapshotVersion(reqInOut.Annotations) == "" {
		return nil
	}
	templateReq, err := GetTemplateRequest(ctx, templateID)
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			return ret.Err(errorcode.ErrorCode_NotFound, err.Error())
		}
		return err
	}
	if err = EnsureReadyReplica(ctx, templateID); err != nil {
		if errors.Is(err, ErrTemplateHasNoReadyReplica) {
			return ret.Err(errorcode.ErrorCode_NotFound, err.Error())
		}
		return err
	}
	return applyTemplateRequest(templateReq, reqInOut)
}

func applyTemplateRequest(templateReq, reqInOut *sandboxtypes.CreateCubeSandboxReq) error {

	if reqInOut.Annotations == nil {
		reqInOut.Annotations = make(map[string]string)
	}
	if reqInOut.Labels == nil {
		reqInOut.Labels = make(map[string]string)
	}
	for k, v := range templateReq.Annotations {
		if _, exists := reqInOut.Annotations[k]; !exists {
			reqInOut.Annotations[k] = v
		}
	}
	for k, v := range templateReq.Labels {
		if _, exists := reqInOut.Labels[k]; !exists {
			reqInOut.Labels[k] = v
		}
	}
	reqInOut.Volumes = append(reqInOut.Volumes, templateReq.Volumes...)
	for i, templateCtr := range templateReq.Containers {
		if len(reqInOut.Containers) <= i {
			reqInOut.Containers = append(reqInOut.Containers, templateCtr)
			continue
		}
		if reqInOut.Containers[i] == nil {
			reqInOut.Containers[i] = templateCtr
		}
	}
	if reqInOut.NetworkType == "" {
		reqInOut.NetworkType = templateReq.NetworkType
	}
	if reqInOut.RuntimeHandler == "" {
		reqInOut.RuntimeHandler = templateReq.RuntimeHandler
	}
	if reqInOut.Namespace == "" {
		reqInOut.Namespace = templateReq.Namespace
	}
	constants.NormalizeAppSnapshotAnnotations(reqInOut.Annotations)
	return nil
}
