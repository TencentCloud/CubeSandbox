// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/remotestatus"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

	CompatStatusOK      = "OK"
	CompatStatusStale   = "STALE"
	CompatStatusUnknown = "UNKNOWN"
	CompatStatusMissing = "MISSING"

	CompatPolicyStrict    = "STRICT"
	CompatPolicyGuestOnly = "GUEST_ONLY"
)

var (
	ErrTemplateStoreNotInitialized  = errors.New("template store is not initialized")
	ErrTemplateNotFound             = errors.New("template not found")
	ErrTemplateIDRequired           = errors.New("template id is required")
	ErrTemplateHasNoReadyReplica    = errors.New("template has no ready replica")
	ErrNoTemplateNodes              = errors.New("no healthy nodes available for template creation")
	ErrDuplicateTemplate            = errors.New("template already exists")
	ErrTemplateAttemptInProgress    = errors.New("template attempt is already in progress")
	ErrAliasNotApplicableToSnapshot = errors.New("alias is not applicable to snapshot")
	ErrInvalidAlias                 = errors.New("alias is invalid")
	ErrTemplateNotReady             = errors.New("template is not ready")
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
	NodeID            string `json:"node_id"`
	NodeIP            string `json:"node_ip"`
	InstanceType      string `json:"instance_type,omitempty"`
	Spec              string `json:"spec,omitempty"`
	Status            string `json:"status"`
	Phase             string `json:"phase,omitempty"`
	ArtifactID        string `json:"artifact_id,omitempty"`
	LastJobID         string `json:"last_job_id,omitempty"`
	LastErrorPhase    string `json:"last_error_phase,omitempty"`
	CleanupRequired   bool   `json:"cleanup_required,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	GuestImageVersion string `json:"guest_image_version,omitempty"`
	AgentVersion      string `json:"agent_version,omitempty"`
	KernelVersion     string `json:"kernel_version,omitempty"`
	ShimVersion       string `json:"shim_version,omitempty"`
	CompatStatus      string `json:"compat_status,omitempty"`
	CompatPolicy      string `json:"compat_policy,omitempty"`
	CompatCheckedUnix int64  `json:"compat_checked_unix,omitempty"`
}

type TemplateInfo struct {
	TemplateID                string          `json:"template_id"`
	InstanceType              string          `json:"instance_type,omitempty"`
	Version                   string          `json:"version,omitempty"`
	Status                    string          `json:"status"`
	Kind                      string          `json:"kind,omitempty"`
	OriginSandboxID           string          `json:"origin_sandbox_id,omitempty"`
	OriginNodeID              string          `json:"origin_node_id,omitempty"`
	OriginHostFactsJSON       string          `json:"origin_host_facts_json,omitempty"`
	DisplayName               string          `json:"display_name,omitempty"`
	StorageBackend            string          `json:"storage_backend,omitempty"`
	Backend                   string          `json:"backend,omitempty"`
	Retain                    bool            `json:"retain,omitempty"`
	RootfsSizeBytesAtSnapshot uint64          `json:"rootfs_size_bytes_at_snapshot,omitempty"`
	LastError                 string          `json:"last_error,omitempty"`
	CreatedAt                 string          `json:"created_at,omitempty"`
	ImageInfo                 string          `json:"image_info,omitempty"`
	JobID                     string          `json:"job_id,omitempty"`
	Replicas                  []ReplicaStatus `json:"replicas,omitempty"`

	// CubeEgress CA bake metadata, surfaced for ops triage. Populated
	// from the RootfsArtifact row pointed to by the first replica.
	// All replicas of one template share the same artifact, so a
	// single lookup covers them. Empty/zero on legacy templates that
	// were built before the CA feature existed.
	CubeEgressCABaked          bool   `json:"cube_egress_ca_baked,omitempty"`
	CubeEgressCAFingerprint    string `json:"cube_egress_ca_fingerprint,omitempty"`
	CubeEgressCATargetsWritten int    `json:"cube_egress_ca_targets_written,omitempty"`
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
		OriginHostFactsJSON:       def.OriginHostFactsJSON,
		DisplayName:               def.DisplayName,
		StorageBackend:            def.StorageBackend,
		Backend:                   def.StorageBackend,
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
	OriginHostFactsJSON       string
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
		// Pause-produced snaps are internal Resume artifacts (Kind=pause_snapshot).
		// Keep them out of template/snapshot list surfaces.
		if strings.EqualFold(strings.TrimSpace(def.Kind), pausesnap.KindPauseSnapshot) {
			continue
		}
		imageInfo := extractImageInfoFromRequestJSON(def.RequestJSON)
		if latestJob := latestJobByTemplateID[def.TemplateID]; latestJob != nil {
			imageInfo = composeImageInfo(latestJob.SourceImageRef, latestJob.SourceImageDigest)
		}
		info := templateInfoFromDefinition(def)
		info.CreatedAt = formatUTCRFC3339(def.CreatedAt)
		info.ImageInfo = imageInfo
		if latestJob := latestJobByTemplateID[def.TemplateID]; latestJob != nil {
			info.JobID = latestJobIDFromJob(latestJob)
		}
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
	if config.GetDbConfig() == nil {
		return ErrTemplateStoreNotInitialized
	}
	var initErr error
	storeOnce.Do(func() {
		// Schema is owned by pkg/base/dao/migrate and applied in main.go
		// before any business package Init runs; here we only attach to
		// the existing *gorm.DB.
		store.db = db.Init(config.GetDbConfig())
		store.dbAddr = config.GetDbConfig().Addr
		if initErr = sandboxspec.Init(store.db); initErr != nil {
			return
		}
		pausesnap.Init(store.db)
		configureSnapshotRuntimeRefHooks()
		configureSandboxSpecHooks()
		if warmErr := warmReadyTemplateLocality(ctx); warmErr != nil {
			log.G(ctx).Warnf("warm ready template locality fail:%v", warmErr)
		}
		startSnapshotReconciler(ctx)
		remotestatus.Start(ctx, store.db)
		startArtifactGC(ctx)
		scheduleInitialCompatScan(ctx)
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
	} else if !hasValidTemplateIDPrefix(templateID) {
		// Defensive guard: template IDs must start with "tpl-" (templates
		// created from images or imports) or "snap-" (snapshot-kind templates).
		// Reaching this branch means an external caller injected a non-standard
		// template ID via the annotation. Reject it explicitly rather than
		// silently accepting it, so the caller can be fixed.
		return nil, "", fmt.Errorf("invalid template ID %q from annotation %s: must start with 'tpl-' or 'snap-' and include a non-empty suffix",
			templateID, constants.CubeAnnotationAppSnapshotTemplateID)
	}
	cloned.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = templateID
	cloned.Annotations[constants.CubeAnnotationsAppSnapshotCreate] = "true"
	if cloned.InstanceType == "" {
		cloned.InstanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	if err := validateTemplateCubeNetworkConfig(cloned.CubeNetworkConfig); err != nil {
		return nil, "", err
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

func hasValidTemplateIDPrefix(templateID string) bool {
	for _, prefix := range []string{"tpl-", "snap-"} {
		if strings.HasPrefix(templateID, prefix) {
			return len(templateID) > len(prefix)
		}
	}
	return false
}

// GenerateTemplateID returns a new unique template ID with "tpl-" prefix.
// Exported for use by HTTP handlers (e.g. template commit) that need to
// generate a template ID before calling NormalizeRequest.
func GenerateTemplateID() string {
	return generateTemplateID()
}

func normalizeStoredTemplateRequest(req *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, error) {
	cloned, templateID, err := NormalizeRequest(req)
	if err != nil {
		return nil, err
	}
	delete(cloned.Annotations, constants.CubeAnnotationsAppSnapshotCreate)
	cloned.SnapshotDir = ""
	cloned.Timeout = nil
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
	// The synchronous CreateCubeSandboxReq path carries no template alias (that
	// is exclusive to CreateTemplateFromImageReq), so no alias is claimed here.
	info, _, finalizeErr := finalizeTemplateReplicas(ctx, templateID, "", createReq.InstanceType, constants.GetAppSnapshotVersion(createReq.Annotations), replicas)
	return info, finalizeErr
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
	envdVersions := make([]nodeEnvdVersion, 0, len(targets))
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

			replica, envdVersion := createReplicaOnNode(ctx, target, req, opts)
			lock.Lock()
			replicas = append(replicas, replica)
			envdVersions = append(envdVersions, nodeEnvdVersion{
				NodeID:  target.ID(),
				Version: envdVersion,
			})
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
	// Converge per-node envd versions into a single template value and persist it
	// once to the definition annotation (idempotent; covers create and redo).
	if envdVersion := convergeEnvdVersion(ctx, envdVersions); envdVersion != "" {
		if err := persistTemplateEnvdVersion(ctx, templateID, envdVersion); err != nil {
			log.G(ctx).Warnf("persist template envd version fail, template=%s err=%v", templateID, err)
		}
	}
	return replicas, persistErr
}

type nodeEnvdVersion struct {
	NodeID  string
	Version string
}

// convergeEnvdVersion picks a single template-level envd version from the
// per-node collection results: the first valid semver wins, and any divergence
// across nodes is logged but not treated as an error.
func convergeEnvdVersion(ctx context.Context, versions []nodeEnvdVersion) string {
	chosen := ""
	for _, item := range versions {
		v := sanitizeEnvdVersion(item.Version)
		if v == "" {
			continue
		}
		if chosen == "" {
			chosen = v
			continue
		}
		if v != chosen {
			log.G(ctx).Warnf("envd version mismatch across nodes: keeping=%s saw=%s node=%s", chosen, v, item.NodeID)
		}
	}
	return chosen
}

// persistTemplateEnvdVersion writes the converged envd version into the template
// definition's request_json annotation exactly once (read-modify-write), then
// invalidates the cached request so new sandboxes inherit the annotation. It is
// idempotent: a no-op when the annotation already holds the same value.
func persistTemplateEnvdVersion(ctx context.Context, templateID, version string) error {
	version = sanitizeEnvdVersion(version)
	if templateID == "" || version == "" {
		return nil
	}
	// Serialize the request_json read-modify-write against concurrent template
	// request reads/writes for the same template to avoid a lost update.
	return withTemplateWriteLock(templateID, func() error {
		if rec, err := getSnapshotRecord(ctx, templateID); err == nil && rec != nil {
			req, err := requestFromSnapshotJSON(rec.RequestJSON)
			if err != nil {
				return err
			}
			if req.Annotations == nil {
				req.Annotations = make(map[string]string)
			}
			if req.Annotations[constants.CubeAnnotationComponentEnvdVersion] == version {
				return nil
			}
			req.Annotations[constants.CubeAnnotationComponentEnvdVersion] = version
			payload, err := json.Marshal(req)
			if err != nil {
				return err
			}
			if err := updateSnapshotFields(ctx, templateID, map[string]any{"request_json": string(payload)}); err != nil {
				return err
			}
			templateDefinitionCache.Delete(templateID)
			return nil
		} else if err != nil && !errors.Is(err, ErrSnapshotNotFound) {
			return err
		}
		def, err := GetDefinition(ctx, templateID)
		if err != nil {
			return err
		}
		req := &sandboxtypes.CreateCubeSandboxReq{}
		if err := json.Unmarshal([]byte(def.RequestJSON), req); err != nil {
			return err
		}
		if req.Annotations == nil {
			req.Annotations = make(map[string]string)
		}
		if req.Annotations[constants.CubeAnnotationComponentEnvdVersion] == version {
			return nil
		}
		req.Annotations[constants.CubeAnnotationComponentEnvdVersion] = version
		payload, err := json.Marshal(req)
		if err != nil {
			return err
		}
		if err := updateDefinitionFields(ctx, templateID, map[string]any{"request_json": string(payload)}); err != nil {
			return err
		}
		// Drop the stale cache entry so the next GetTemplateRequest reloads the
		// definition (now carrying the envd annotation) from the database.
		templateDefinitionCache.Delete(templateID)
		return nil
	})
}

func createReplicaOnNode(ctx context.Context, target *node.Node, req *sandboxtypes.CreateCubeSandboxReq, opts replicaRunOptions) (ReplicaStatus, string) {
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
		return replica, ""
	}
	ensureRuntimeTemplateRequest(nodeReq)
	cubeletReq, err := sandbox.ConstructCubeletReq(ctx, nodeReq)
	if err != nil {
		replica.Phase = ReplicaPhaseFailed
		replica.ErrorMessage = err.Error()
		return replica, ""
	}
	rsp, err := cubelet.AppSnapshot(ctx, cubelet.GetCubeletAddr(target.HostIP()), &cubeboxv1.AppSnapshotRequest{
		CreateRequest: cubeletReq,
		SnapshotDir:   req.SnapshotDir,
		Backend:       storageBackendFromCreate(nodeReq),
	})
	if err != nil {
		replica.Phase = ReplicaPhaseFailed
		replica.ErrorMessage = err.Error()
		return replica, ""
	}
	if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		replica.Phase = ReplicaPhaseFailed
		if rsp.GetRet() != nil {
			replica.ErrorMessage = rsp.GetRet().GetRetMsg()
		} else {
			replica.ErrorMessage = "empty appsnapshot response"
		}
		return replica, ""
	}
	replica.Status = ReplicaStatusReady
	replica.Phase = ReplicaPhaseReady
	bindGuestVersionToReplica(&replica, rsp.GetGuestImageVersion(), rsp.GetAgentVersion(), rsp.GetKernelVersion(), rsp.GetShimVersion())
	envdVersion := sanitizeEnvdVersion(rsp.GetEnvdVersion())
	// v4: AppSnapshot replica is "thin" -- physical refs are owned by cubelet's
	// local catalog. Master only persists control-plane state (status / phase /
	// last job / error) so we deliberately ignore SnapshotPath/RootfsVol/
	// MemoryVol/RootfsKind/MemoryKind in the RPC response here.
	replica.LastErrorPhase = ""
	replica.CleanupRequired = false
	replica.ErrorMessage = ""
	return replica, envdVersion
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

// refreshTemplateReplicaSummary recomputes the template's aggregate status from
// its replicas and publishes it. When the template is not FAILED it claims the
// requested alias *before* publishing the status, so a client that observes the
// template as READY (e.g. by polling GetTemplateInfo) is guaranteed the alias
// already resolves — closing the create/claim publish-ordering race. The
// returned claimWarning is non-empty when the alias could not be claimed for a
// non-duplicate reason.
func refreshTemplateReplicaSummary(ctx context.Context, templateID, jobID string) (claimWarning string, err error) {
	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return "", err
	}
	current := make([]ReplicaStatus, 0, len(replicas))
	for _, replica := range replicas {
		current = append(current, replicaModelToStatus(replica))
	}
	status, lastError := summarizeStatus(current)
	_, claimWarning, err = publishTemplateStatusWithAlias(ctx, templateID, jobID, status, lastError)
	if err != nil {
		return "", err
	}
	localcache.InvalidateImageState(templateID)
	setTemplateLocalityCache(templateID, current)
	registerReadyTemplateReplicas(templateID, current)
	return claimWarning, nil
}

// createDefinitionWithOptions wraps createDefinitionTx in a real DB transaction
// so the INSERT is atomic. Alias claiming is NOT done here — it happens
// separately when the template reaches a non-FAILED status, as part of the
// transaction that publishes that status.
func createDefinitionWithOptions(ctx context.Context, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string, opts definitionCreateOptions) error {
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createDefinitionTx(ctx, tx, templateID, storedReq, instanceType, version, opts)
	})
}

// createDefinition creates a definition without alias metadata. Convenience
// wrapper around createDefinitionWithOptions for callers that don't set an
// alias.
func createDefinition(ctx context.Context, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string) error {
	return createDefinitionWithOptions(ctx, templateID, storedReq, instanceType, version, definitionCreateOptions{
		StorageBackend: storageBackendFromCreate(storedReq),
	})
}

// ensureTemplateDefinitionWithOptions checks whether a definition already exists
// and creates one if missing, threading alias metadata (DisplayName) via opts.
func ensureTemplateDefinitionWithOptions(ctx context.Context, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string, opts definitionCreateOptions) (bool, error) {
	if _, err := GetDefinition(ctx, templateID); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrTemplateNotFound) {
		return false, err
	}
	if err := createDefinitionWithOptions(ctx, templateID, storedReq, instanceType, version, opts); err != nil {
		return false, err
	}
	if cacheErr := setTemplateRequestCache(templateID, storedReq); cacheErr != nil {
		log.G(ctx).Warnf("set template request cache fail, template=%s err=%v", templateID, cacheErr)
	}
	return true, nil
}

// finalizeTemplateReplicas publishes the aggregate template status computed from
// its replicas. When the template reaches a non-FAILED status it claims the
// requested alias *before* publishing the status via UpdateDefinitionStatus, so
// a client that observes the template as READY is guaranteed the alias already
// resolves — closing the create/claim publish-ordering race.
//
// A side effect of that ordering is that the alias becomes resolvable while the
// status is still the pre-publish value (PENDING/PARTIALLY_READY): alias
// resolution no longer implies READY. This is intended and strictly safer than
// the old 404 race; callers must not assume "alias resolves" ⇒ "READY".
//
// The returned claim warning (non-empty only when the alias could not be
// claimed for a non-duplicate reason) and the claimed alias are separate,
// mutually-exclusive results: on success the alias is reflected in
// TemplateInfo.DisplayName and the warning is empty; on a non-duplicate claim
// failure the warning is set and DisplayName stays empty.
func finalizeTemplateReplicas(ctx context.Context, templateID, jobID, instanceType, version string, replicas []ReplicaStatus) (*TemplateInfo, string, error) {
	setTemplateLocalityCache(templateID, replicas)
	registerReadyTemplateReplicas(templateID, replicas)

	status, lastError := summarizeStatus(replicas)
	displayName, claimWarning, err := publishTemplateStatusWithAlias(ctx, templateID, jobID, status, lastError)
	if err != nil {
		return nil, "", err
	}
	info := &TemplateInfo{
		TemplateID:   templateID,
		InstanceType: instanceType,
		Version:      version,
		Status:       status,
		LastError:    lastError,
		DisplayName:  displayName,
		Replicas:     replicas,
	}
	if status == StatusFailed {
		if lastError == "" {
			lastError = "template creation failed on all nodes"
		}
		return info, claimWarning, fmt.Errorf("template %s creation failed: %s", templateID, lastError)
	}
	return info, claimWarning, nil
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
	// Pause snaps are not user-visible templates/snapshots.
	if strings.EqualFold(strings.TrimSpace(def.Kind), pausesnap.KindPauseSnapshot) {
		return nil, ErrTemplateNotFound
	}
	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return nil, err
	}
	info := templateInfoFromDefinition(*def)
	out := &info
	out.CreatedAt = formatUTCRFC3339(def.CreatedAt)
	out.ImageInfo = extractImageInfoFromRequestJSON(def.RequestJSON)
	if latestJob, jobErr := getLatestTemplateImageJobByTemplateID(ctx, templateID); jobErr == nil && latestJob != nil {
		out.ImageInfo = composeImageInfo(latestJob.SourceImageRef, latestJob.SourceImageDigest)
		out.JobID = latestJobIDFromJob(latestJob)
	}
	out.Replicas = make([]ReplicaStatus, 0, len(replicas))
	for _, replica := range replicas {
		out.Replicas = append(out.Replicas, replicaModelToStatus(replica))
	}
	// Populate CA bake metadata from the artifact referenced by the
	// first replica with a non-empty ArtifactID. Errors here are
	// non-fatal — the CA fields stay zero, the rest of the template
	// info is still useful. Worst case the operator can re-query.
	for _, r := range out.Replicas {
		if r.ArtifactID == "" {
			continue
		}
		artifact, err := getRootfsArtifactByID(ctx, r.ArtifactID)
		if err != nil {
			break
		}
		out.CubeEgressCABaked = artifact.CubeEgressCABaked
		out.CubeEgressCAFingerprint = artifact.CubeEgressCAFingerprint
		out.CubeEgressCATargetsWritten = artifact.CubeEgressCATargetsWritten
		break
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
		JobID:        latestJobIDFromJob(job),
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

// GetTemplateByAlias looks up a non-deleted TEMPLATE definition by its
// display_name (used as a stable alias). Returns ErrTemplateNotFound when no
// template has the given alias.
//
// The query uses alias_key (the STORED generated column, non-NULL only for
// kind='template' rows with a non-empty display_name — see migration
// 20260704120000) so it is inherently scoped to template-kind rows: a snapshot
// carries its own informational display_name (NULL alias_key) and can never
// match. The write path that owns template aliases is claimTemplateAlias
// (invoked once the template reaches a non-FAILED status, before that status is
// published); without the alias_key filter, a snapshot sharing the alias could
// shadow the owning template and resolve the alias to a snap-* id.
func GetTemplateByAlias(ctx context.Context, alias string) (*models.TemplateDefinition, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	if strings.TrimSpace(alias) == "" {
		return nil, ErrTemplateNotFound
	}
	return getTemplateByAliasTx(store.db.WithContext(ctx), alias)
}

// ResolveTemplateIdentifier resolves an identifier that may be either a
// template ID (tpl-.../snap-...) or a human-readable alias. If the identifier
// already has a valid template ID prefix it is returned unchanged. Otherwise
// it is treated as an alias and resolved to the underlying template ID.
// Returns ("", nil) for an empty identifier.
func ResolveTemplateIdentifier(ctx context.Context, identifier string) (string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", nil
	}
	if hasValidTemplateIDPrefix(identifier) {
		return identifier, nil
	}
	def, err := GetTemplateByAlias(ctx, identifier)
	if err != nil {
		return "", err
	}
	return def.TemplateID, nil
}

func claimTemplateAliasTx(tx *gorm.DB, templateID, alias string) error {
	if err := tx.Table(constants.TemplateDefinitionTableName).
		Where("alias_key = ? AND template_id <> ?", alias, templateID).
		Update("display_name", "").Error; err != nil {
		return fmt.Errorf("release stale alias %q fail: %w", alias, err)
	}
	if err := tx.Table(constants.TemplateDefinitionTableName).
		Where("template_id = ? AND status = ?", templateID, StatusReady).
		Update("display_name", alias).Error; err != nil {
		return fmt.Errorf("claim alias %q for template %s fail: %w", alias, templateID, err)
	}
	var claimed int64
	if err := tx.Table(constants.TemplateDefinitionTableName).
		Where("template_id = ? AND alias_key = ? AND status = ?", templateID, alias, StatusReady).
		Count(&claimed).Error; err != nil {
		return fmt.Errorf("confirm alias claim for template %s fail: %w", templateID, err)
	}
	if claimed > 0 {
		return nil
	}
	var exists int64
	if err := tx.Table(constants.TemplateDefinitionTableName).
		Where("template_id = ?", templateID).Count(&exists).Error; err != nil {
		return fmt.Errorf("confirm existence for template %s fail: %w", templateID, err)
	}
	if exists > 0 {
		return ErrTemplateNotReady
	}
	return ErrTemplateNotFound
}

func publishTemplateStatusWithAlias(ctx context.Context, templateID, jobID, status, lastError string) (displayName, claimWarning string, err error) {
	if !isReady() {
		return "", "", ErrTemplateStoreNotInitialized
	}
	alias := ""
	claimantJobRowID := uint(0)
	expectedStatus := ""
	claimed := false
	displacedTemplateID := ""
	claimWarning = ""
	var claimErr error
	run := func() error {
		return retryOnceOnDeadlock(func() error {
			alias = ""
			claimantJobRowID = 0
			expectedStatus = ""
			claimed = false
			displacedTemplateID = ""
			claimWarning = ""
			claimErr = nil
			return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				target, err := lockTemplateDefinitionTx(tx, templateID)
				if err != nil {
					return err
				}
				if target.Status == StatusDeleting {
					return ErrTemplateNotFound
				}
				expectedStatus = target.Status
				if jobID != "" {
					job, err := getCreateRedoImageJobByIDTx(tx, templateID, jobID)
					if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
						claimErr = err
						return err
					}
					if err == nil {
						claimantJobRowID = job.ID
						alias = strings.TrimSpace(aliasFromRequestJSON(job.RequestJSON))
						if strings.TrimSpace(target.DisplayName) != "" {
							alias = strings.TrimSpace(target.DisplayName)
						}
					}
				}
				if status != StatusFailed && alias != "" {
					claimResult, err := claimTemplateAliasByJobOrderTx(tx, templateID, claimantJobRowID, alias)
					if err != nil {
						claimErr = err
						return err
					}
					claimed = claimResult.Claimed
					displacedTemplateID = claimResult.DisplacedTemplateID
					claimWarning = claimResult.Warning
				}
				return tx.Table(constants.TemplateDefinitionTableName).
					Where("template_id = ?", templateID).
					Updates(map[string]any{
						"status":     status,
						"last_error": lastError,
						"updated_at": time.Now(),
					}).Error
			})
		})
	}
	err = run()
	if err != nil && isDuplicateAliasError(err) {
		err = run()
	}
	if err == nil {
		if claimed {
			if displacedTemplateID != "" {
				log.G(ctx).Warnf("alias %q transferred from template %s to newer template build %s", alias, displacedTemplateID, templateID)
			}
			return alias, claimWarning, nil
		}
		if claimWarning != "" {
			log.G(ctx).Warnf("template %s is %s without alias %q: %s", templateID, status, alias, claimWarning)
			return "", claimWarning, nil
		}
		if alias != "" && status != StatusFailed {
			log.G(ctx).Infof("alias %q belongs to a newer template build; template %s is %s without alias", alias, templateID, status)
		}
		return "", "", nil
	}
	if claimErr == nil {
		return "", "", err
	}
	if statusErr := publishTemplateStatusWithoutAlias(ctx, templateID, expectedStatus, status, lastError); statusErr != nil {
		return "", "", statusErr
	}
	if isDuplicateAliasError(claimErr) {
		return "", "", nil
	}
	return "", fmt.Sprintf("template is ready but alias %q could not be claimed: %v", alias, claimErr), nil
}

func publishTemplateStatusWithoutAlias(ctx context.Context, templateID, expectedStatus, status, lastError string) error {
	return retryOnceOnDeadlock(func() error {
		return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			target, err := lockTemplateDefinitionTx(tx, templateID)
			if err != nil {
				return err
			}
			if target.Status == StatusDeleting {
				return ErrTemplateNotFound
			}
			if target.Status != expectedStatus {
				return fmt.Errorf("template %s status changed from %s to %s while publishing without alias", templateID, expectedStatus, target.Status)
			}
			result := tx.Table(constants.TemplateDefinitionTableName).
				Where("template_id = ? AND status = ?", templateID, expectedStatus).
				Updates(map[string]any{
					"status":     status,
					"last_error": lastError,
					"updated_at": time.Now(),
				})
			if result.Error != nil {
				return result.Error
			}
			var published int64
			if err := tx.Table(constants.TemplateDefinitionTableName).
				Where("template_id = ? AND status <> ?", templateID, StatusDeleting).
				Count(&published).Error; err != nil {
				return err
			}
			if published == 0 {
				return ErrTemplateNotFound
			}
			return nil
		})
	})
}

type orderedAliasClaimResult struct {
	Claimed             bool
	DisplacedTemplateID string
	Warning             string
}

func claimTemplateAliasByJobOrderTx(tx *gorm.DB, templateID string, claimantJobRowID uint, alias string) (orderedAliasClaimResult, error) {
	var result orderedAliasClaimResult
	holder := &models.TemplateDefinition{}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Table(constants.TemplateDefinitionTableName).
		Where("alias_key = ?", alias).
		First(holder).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		update := tx.Table(constants.TemplateDefinitionTableName).
			Where("template_id = ? AND status <> ?", templateID, StatusDeleting).
			Update("display_name", alias)
		if update.Error != nil {
			return result, fmt.Errorf("claim alias %q for template %s fail: %w", alias, templateID, update.Error)
		}
		result.Claimed = update.RowsAffected > 0
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if holder.TemplateID == templateID {
		result.Claimed = true
		return result, nil
	}
	if holder.Status == StatusDeleting {
		if err := tx.Table(constants.TemplateDefinitionTableName).
			Where("template_id = ? AND alias_key = ? AND status = ?", holder.TemplateID, alias, StatusDeleting).
			Update("display_name", "").Error; err != nil {
			return result, fmt.Errorf("release alias %q from deleting template %s fail: %w", alias, holder.TemplateID, err)
		}
		if err := syncCreateRedoImageJobAliasTx(tx, holder.TemplateID, ""); err != nil {
			return result, err
		}
		update := tx.Table(constants.TemplateDefinitionTableName).
			Where("template_id = ? AND status <> ?", templateID, StatusDeleting).
			Update("display_name", alias)
		if update.Error != nil {
			return result, fmt.Errorf("claim alias %q for template %s fail: %w", alias, templateID, update.Error)
		}
		result.Claimed = update.RowsAffected > 0
		return result, nil
	}

	// Order template generations globally by persisted row ID. The holder's
	// newest generation is intentionally conservative even if it requested a
	// different alias; alias-scoped or operator ordering needs persisted claim
	// provenance rather than the per-template attempt number.
	holderJob, err := getNewestCreateRedoImageJobByTemplateIDTx(tx, holder.TemplateID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		result.Warning = fmt.Sprintf("alias %q could not be ordered because holder template %s has no CREATE/REDO job metadata", alias, holder.TemplateID)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if claimantJobRowID <= holderJob.ID {
		return result, nil
	}

	if err := tx.Table(constants.TemplateDefinitionTableName).
		Where("template_id = ? AND alias_key = ?", holder.TemplateID, alias).
		Update("display_name", "").Error; err != nil {
		return result, fmt.Errorf("release alias %q from template %s fail: %w", alias, holder.TemplateID, err)
	}
	if err := syncCreateRedoImageJobAliasTx(tx, holder.TemplateID, ""); err != nil {
		return result, err
	}
	if err := tx.Table(constants.TemplateDefinitionTableName).
		Where("template_id = ? AND status <> ?", templateID, StatusDeleting).
		Update("display_name", alias).Error; err != nil {
		return result, fmt.Errorf("claim alias %q for template %s fail: %w", alias, templateID, err)
	}
	result.Claimed = true
	result.DisplacedTemplateID = holder.TemplateID
	return result, nil
}

func lockTemplateDefinitionTx(tx *gorm.DB, templateID string) (*models.TemplateDefinition, error) {
	def := &models.TemplateDefinition{}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Table(constants.TemplateDefinitionTableName).
		Where("template_id = ?", templateID).
		First(def).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTemplateNotFound
		}
		return nil, err
	}
	return def, nil
}

func getTemplateByAliasTx(tx *gorm.DB, alias string) (*models.TemplateDefinition, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, ErrTemplateNotFound
	}
	def := &models.TemplateDefinition{}
	err := tx.Table(constants.TemplateDefinitionTableName).
		Where("alias_key = ? AND status <> ?", alias, StatusDeleting).
		First(def).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTemplateNotFound
		}
		return nil, err
	}
	return def, nil
}

// isDuplicateAliasError returns true for MySQL (1062) and PostgreSQL (23505)
// unique-violation errors, using structured type assertions instead of string
// matching to avoid false positives from unrelated error text.
func isDuplicateAliasError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return true
	}
	// pgconn.PgError has Code == "23505" for unique_violation. We check via
	// string matching on the error message as a fallback because the pgx
	// driver may not be imported in all build configurations.
	s := err.Error()
	return strings.Contains(s, "23505") || strings.Contains(s, "unique_constraint")
}

// isDeadlockError reports transient lock errors worth one retry in the alias
// release+claim transaction: InnoDB deadlock (1213) / lock-wait-timeout (1205),
// and the PostgreSQL equivalents 40P01 (deadlock_detected) / 55P03
// (lock_not_available). PG codes are string-matched like isDuplicateAliasError
// because pgx may not be imported in all build configurations.
func isDeadlockError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1205 || mysqlErr.Number == 1213) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "40P01") || strings.Contains(s, "55P03")
}

func retryOnceOnDeadlock(run func() error) error {
	if err := run(); err != nil {
		if isDeadlockError(err) {
			return run()
		}
		return err
	}
	return nil
}

// IsDuplicateAliasError is the exported wrapper around isDuplicateAliasError
// so the HTTP layer (separate package) can map concurrent-claim failures to
// HTTP 409 / RetCode=ErrorCode_Conflict without duplicating the driver
// detection logic. See design §3.3.
func IsDuplicateAliasError(err error) bool { return isDuplicateAliasError(err) }

// SetTemplateAlias atomically sets, transfers, or clears the alias (display_name)
// of an existing template-kind definition.
//
//	alias == ""  → clear: UPDATE display_name = '' WHERE template_id = ?
//	alias != ""  → claim: operator transfer (release + claim + confirm) inside
//	                    one transaction with CREATE/REDO job JSON sync.
//
// Claim requires the target to be READY (non-READY → ErrTemplateNotReady), so
// an alias never points at a building/failed template. Clear is allowed for any
// non-DELETING template, so an alias stuck on a FAILED template can be released
// without deleting the template. Snapshots are rejected
// (ErrAliasNotApplicableToSnapshot) because their display_name is an
// informational label, not a unique alias (alias_key is always NULL per the
// STORED generated column). DELETING templates return ErrTemplateNotFound,
// matching GetTemplateByAlias' behavior (store.go:921).
//
// display_name and the template's CREATE/REDO RequestJSON are updated in the
// same DB transaction (design §3.6 I1). COMMIT / SNAPSHOT_* / LEGACY jobs are
// never rewritten. A failure rolls back; the API does not return success with
// a stale redo blob.
func SetTemplateAlias(ctx context.Context, templateID, alias string) error {
	if strings.TrimSpace(templateID) == "" {
		return ErrTemplateIDRequired
	}
	alias = strings.TrimSpace(alias)
	if err := validateTemplateAlias(alias); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAlias, err)
	}
	def, err := GetDefinition(ctx, templateID)
	if err != nil {
		return err
	}
	if isSnapshotDefinition(def) {
		return ErrAliasNotApplicableToSnapshot
	}
	if def.Status == StatusDeleting {
		return ErrTemplateNotFound
	}
	// Claim requires READY; clear is allowed for any non-DELETING template so a
	// stuck alias (e.g. on a FAILED template) can always be released.
	if alias != "" && def.Status != StatusReady {
		return ErrTemplateNotReady
	}
	// Idempotent clear of an already-empty alias is a no-op: do not rewrite
	// in-flight first-create job JSON (design §3.6 — PENDING DisplayName is
	// empty until finalize claims).
	if alias == "" && strings.TrimSpace(def.DisplayName) == "" {
		return nil
	}
	return setTemplateAliasLocked(ctx, templateID, alias)
}

// setTemplateAliasLocked holds the definition row, mutates display_name, and
// syncs CREATE/REDO job JSON in one transaction.
func setTemplateAliasLocked(ctx context.Context, templateID, alias string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	return retryOnceOnDeadlock(func() error {
		return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			def, err := lockTemplateDefinitionTx(tx, templateID)
			if err != nil {
				return err
			}
			if isSnapshotDefinition(def) {
				return ErrAliasNotApplicableToSnapshot
			}
			if def.Status == StatusDeleting {
				return ErrTemplateNotFound
			}
			if alias == "" {
				if strings.TrimSpace(def.DisplayName) == "" {
					return nil
				}
				if err := tx.Table(constants.TemplateDefinitionTableName).
					Where("template_id = ?", templateID).
					Updates(map[string]any{
						"display_name": "",
						"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
					}).Error; err != nil {
					return err
				}
				return syncCreateRedoImageJobAliasTx(tx, templateID, "")
			}
			if def.Status != StatusReady {
				return ErrTemplateNotReady
			}
			oldHolder := ""
			if cur, err := getTemplateByAliasTx(tx, alias); err == nil && cur != nil && cur.TemplateID != templateID {
				oldHolder = cur.TemplateID
			} else if err != nil && !errors.Is(err, ErrTemplateNotFound) {
				return err
			}
			if err := claimTemplateAliasTx(tx, templateID, alias); err != nil {
				return err
			}
			if err := syncCreateRedoImageJobAliasTx(tx, templateID, alias); err != nil {
				return err
			}
			if oldHolder != "" {
				if err := syncCreateRedoImageJobAliasTx(tx, oldHolder, ""); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// applyAliasToRequestJSON returns payload with its "alias" field set to alias
// (omitted when alias is ""). Unrelated fields are preserved semantically via
// a generic JSON edit (UseNumber keeps numeric precision; RegistryPassword is
// not stripped — marshalTemplateImageJobRequest would). The result is not
// byte-identical: json.Marshal may reorder keys. changed is false when the
// alias already matched.
func applyAliasToRequestJSON(payload, alias string) (string, bool, error) {
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return "", false, err
	}
	if cur, _ := m["alias"].(string); cur == alias {
		return payload, false, nil
	}
	if alias == "" {
		delete(m, "alias")
	} else {
		m["alias"] = alias
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

// aliasFromRequestJSON reads the "alias" field from a job's RequestJSON (""
// if absent/unparseable).
func aliasFromRequestJSON(payload string) string {
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return ""
	}
	a, _ := m["alias"].(string)
	return a
}

// syncCreateRedoImageJobAliasTx writes the operator alias into every CREATE
// and REDO job's RequestJSON for the template (design §3.6 I1). COMMIT /
// SNAPSHOT_* / LEGACY jobs are never touched — their payloads are compared
// byte-wise for idempotency. Failures abort the caller's transaction.
func syncCreateRedoImageJobAliasTx(tx *gorm.DB, templateID, alias string) error {
	jobs, err := listCreateRedoImageJobsByTemplateIDTx(tx, templateID)
	if err != nil {
		return fmt.Errorf("list CREATE/REDO image jobs for template %s: %w", templateID, err)
	}
	for i := range jobs {
		if jobs[i].RequestJSON == "" {
			continue
		}
		newPayload, changed, err := applyAliasToRequestJSON(jobs[i].RequestJSON, alias)
		if err != nil {
			return fmt.Errorf("edit request_json alias for job %s: %w", jobs[i].JobID, err)
		}
		if !changed {
			continue
		}
		if err := updateTemplateImageJobTx(tx, jobs[i].JobID, map[string]any{"request_json": newPayload}); err != nil {
			return fmt.Errorf("sync image job alias: update job %s for template %s failed: %w", jobs[i].JobID, templateID, err)
		}
	}
	return nil
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
			if rec, snapErr := getSnapshotRecord(ctx, templateID); snapErr == nil && rec != nil {
				reportTemplateMetric(ctx, constants.MySQL, store.dbAddr, constants.ActionTemplateGetDefinition, time.Since(dbStart), 0)
				parsed, err := requestFromSnapshotJSON(rec.RequestJSON)
				if err != nil {
					return err
				}
				req = parsed
				if err = applyStoredCreateBackend(req, rec.Backend); err != nil {
					return err
				}
				if err = setTemplateRequestCache(templateID, req); err != nil {
					log.G(ctx).Warnf("set snapshot request cache fail, snapshot=%s err=%v", templateID, err)
				}
				return nil
			} else if snapErr != nil && !errors.Is(snapErr, ErrSnapshotNotFound) {
				return snapErr
			}
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
			if err = applyStoredCreateBackend(req, def.StorageBackend); err != nil {
				return err
			}
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

func normalizeComponentVersion(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "unknown") {
		return ""
	}
	return value
}

// envdSemverRe matches a major.minor.patch semantic version anywhere in the
// input; the captured group is the version we keep.
var envdSemverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// sanitizeEnvdVersion validates an envd version reported by the collection side
// and returns the extracted semver, or "" when absent/malformed. This is a
// defense-in-depth check on top of the cubelet-side validation, run before the
// value is persisted into a template annotation.
func sanitizeEnvdVersion(value string) string {
	return envdSemverRe.FindString(strings.TrimSpace(value))
}

func normalizeCompatStatus(status string) string {
	status = strings.ToUpper(strings.TrimSpace(status))
	switch status {
	case CompatStatusOK, CompatStatusStale, CompatStatusUnknown:
		return status
	case "":
		return CompatStatusUnknown
	default:
		return status
	}
}

func normalizeCompatPolicy(policy string) string {
	policy = strings.ToUpper(strings.TrimSpace(policy))
	switch policy {
	case CompatPolicyStrict, CompatPolicyGuestOnly:
		return policy
	case "":
		return CompatPolicyStrict
	default:
		return policy
	}
}

func compareCompatDimension(bound, current string) (stale bool, unknown bool) {
	bound = normalizeComponentVersion(bound)
	current = normalizeComponentVersion(current)
	if bound == "" || current == "" {
		return false, true
	}
	return bound != current, false
}

func evaluateCompat(replica ReplicaStatus, currentGuestImage, currentAgent, _ string) string {
	policy := normalizeCompatPolicy(replica.CompatPolicy)
	dimensions := []struct {
		bound   string
		current string
		active  bool
	}{
		{replica.GuestImageVersion, currentGuestImage, true},
		{replica.AgentVersion, currentAgent, policy != CompatPolicyGuestOnly},
	}
	seenUnknown := false
	for _, dim := range dimensions {
		if !dim.active {
			continue
		}
		stale, unknown := compareCompatDimension(dim.bound, dim.current)
		if stale {
			return CompatStatusStale
		}
		if unknown {
			seenUnknown = true
		}
	}
	if seenUnknown {
		return CompatStatusUnknown
	}
	return CompatStatusOK
}

func isReplicaSchedulable(replica ReplicaStatus) bool {
	return replica.Status == ReplicaStatusReady
}

// bindGuestVersionToReplica records pin versions on the replica. CompatStatus
// still compares guest[+agent] only; kernel/shim are stored for create inject
// and do not participate in evaluateCompat.
func bindGuestVersionToReplica(replica *ReplicaStatus, guestImageVersion, agentVersion, kernelVersion, shimVersion string) {
	if replica == nil {
		return
	}
	replica.GuestImageVersion = normalizeComponentVersion(guestImageVersion)
	replica.AgentVersion = normalizeComponentVersion(agentVersion)
	replica.KernelVersion = normalizeComponentVersion(kernelVersion)
	replica.ShimVersion = normalizeComponentVersion(shimVersion)
	replica.CompatPolicy = CompatPolicyStrict
	replica.CompatStatus = evaluateCompat(*replica, replica.GuestImageVersion, replica.AgentVersion, replica.KernelVersion)
	replica.CompatCheckedUnix = time.Now().Unix()
}

func replicaModelToStatus(replica models.TemplateReplica) ReplicaStatus {
	return ReplicaStatus{
		NodeID:            replica.NodeID,
		NodeIP:            replica.NodeIP,
		InstanceType:      replica.InstanceType,
		Spec:              replica.Spec,
		Status:            replica.Status,
		Phase:             replica.Phase,
		ArtifactID:        replica.ArtifactID,
		LastJobID:         replica.LastJobID,
		LastErrorPhase:    replica.LastErrorPhase,
		CleanupRequired:   replica.CleanupRequired,
		ErrorMessage:      replica.ErrorMessage,
		GuestImageVersion: replica.GuestImageVersion,
		AgentVersion:      replica.AgentVersion,
		KernelVersion:     replica.KernelVersion,
		ShimVersion:       replica.ShimVersion,
		CompatStatus:      normalizeCompatStatus(replica.CompatStatus),
		CompatPolicy:      normalizeCompatPolicy(replica.CompatPolicy),
		CompatCheckedUnix: replica.CompatCheckedUnix,
	}
}

func replicaStatusToModel(templateID, instanceType string, replica ReplicaStatus) *models.TemplateReplica {
	return &models.TemplateReplica{
		TemplateID:        templateID,
		NodeID:            replica.NodeID,
		NodeIP:            replica.NodeIP,
		InstanceType:      instanceType,
		Spec:              replica.Spec,
		Status:            replica.Status,
		Phase:             replica.Phase,
		ArtifactID:        replica.ArtifactID,
		LastJobID:         replica.LastJobID,
		LastErrorPhase:    replica.LastErrorPhase,
		CleanupRequired:   replica.CleanupRequired,
		ErrorMessage:      replica.ErrorMessage,
		GuestImageVersion: replica.GuestImageVersion,
		AgentVersion:      replica.AgentVersion,
		KernelVersion:     replica.KernelVersion,
		ShimVersion:       replica.ShimVersion,
		CompatStatus:      normalizeCompatStatus(replica.CompatStatus),
		CompatPolicy:      normalizeCompatPolicy(replica.CompatPolicy),
		CompatCheckedUnix: replica.CompatCheckedUnix,
	}
}

func replicaStatusUpdateFields(instanceType string, replica ReplicaStatus) map[string]any {
	fields := map[string]any{
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
	if normalizeCompatStatus(replica.CompatStatus) != CompatStatusUnknown ||
		normalizeComponentVersion(replica.GuestImageVersion) != "" ||
		normalizeComponentVersion(replica.AgentVersion) != "" ||
		normalizeComponentVersion(replica.KernelVersion) != "" ||
		normalizeComponentVersion(replica.ShimVersion) != "" {
		fields["guest_image_version"] = normalizeComponentVersion(replica.GuestImageVersion)
		fields["agent_version"] = normalizeComponentVersion(replica.AgentVersion)
		fields["kernel_version"] = normalizeComponentVersion(replica.KernelVersion)
		fields["shim_version"] = normalizeComponentVersion(replica.ShimVersion)
		fields["compat_status"] = normalizeCompatStatus(replica.CompatStatus)
		fields["compat_policy"] = normalizeCompatPolicy(replica.CompatPolicy)
		fields["compat_checked_unix"] = replica.CompatCheckedUnix
	}
	return fields
}

func UpsertReplica(ctx context.Context, templateID, instanceType string, replica ReplicaStatus) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	record := &models.TemplateReplica{}
	// Do not reuse the *gorm.DB chain after First on PostgreSQL: GORM may emit
	// UPDATE ... FROM t_cube_template_replica (SQLSTATE 42712).
	err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", templateID, replica.NodeID).
		First(record).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
			Create(replicaStatusToModel(templateID, instanceType, replica)).Error
	}
	return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", templateID, replica.NodeID).
		Updates(replicaStatusUpdateFields(instanceType, replica)).Error
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
		if isReplicaSchedulableNow(ctx, replicaModelToStatus(replica)) {
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
		if !isReplicaSchedulableNow(ctx, replica) {
			continue
		}
		if preferredNodeID == "" || strings.TrimSpace(replica.NodeID) == preferredNodeID {
			return replica, nil
		}
	}
	return ReplicaStatus{}, ErrTemplateHasNoReadyReplica
}

func isTemplateReplicaSchedulable(ctx context.Context, templateID, nodeID string) bool {
	if !isReady() || strings.TrimSpace(templateID) == "" || strings.TrimSpace(nodeID) == "" {
		return false
	}
	replica := models.TemplateReplica{}
	err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", templateID, nodeID).
		First(&replica).Error
	if err != nil {
		return false
	}
	return isReplicaSchedulableNow(ctx, replicaModelToStatus(replica))
}

func isReplicaSchedulableNow(ctx context.Context, replica ReplicaStatus) bool {
	_ = ctx
	return strings.TrimSpace(replica.Status) == ReplicaStatusReady
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
			if isTemplateReplicaSchedulable(ctx, templateID, nodes[i].ID()) {
				reportTemplateCacheMetric(ctx, constants.ActionTemplateLocalityHit, 0)
				return nil
			}
			evictReplicaFromSchedulingCaches(templateID, nodes[i].ID())
		}
		healthyNodeIDs[nodes[i].ID()] = struct{}{}
		if hostIP := strings.TrimSpace(nodes[i].HostIP()); hostIP != "" {
			healthyNodeIPs[hostIP] = struct{}{}
		}
	}
	if replicas, ok := getCachedTemplateLocality(templateID); ok {
		for _, replica := range replicas {
			if !isReplicaSchedulableNow(ctx, replica) {
				continue
			}
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
				status := replicaModelToStatus(replica)
				if !isReplicaSchedulableNow(ctx, status) {
					continue
				}
				readyReplicas = append(readyReplicas, status)
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
