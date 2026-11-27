// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	// runTemplateSnapshotDir / runTemplateConfigFile locate the cube-runtime
	// config inside a package metadata dir: <meta>/snapshot/config.json.
	runTemplateSnapshotDir = "snapshot"
	runTemplateConfigFile  = "config.json"

	// defaultInventoryRefreshTimeout bounds each asynchronous template
	// discovery pass so a stalled storage backend cannot wedge refreshes.
	defaultInventoryRefreshTimeout = 30 * time.Second
)

// Recovery lookups are replaceable so tests can verify backend isolation
// without accessing host mounts or live cubecow daemons.
var (
	recoverySnapshotKindRoot = storage.SnapshotKindRoot
	recoveryS3CatalogLookup  = storage.GetLocalSnapshotFor
)

type RunTemplateManager interface {
	SetInstanceType(instanceType string)

	EnsureCubeRunTemplate(ctx context.Context, templateID, backend string) (*templatetypes.LocalRunTemplate, error)

	ListLocalTemplates(context.Context) (map[string]*templatetypes.LocalRunTemplate, error)
}

var _ RunTemplateManager = &localCubeRunTemplateManager{}

type localCubeRunTemplateManager struct {
	clientSet              any
	templateLister         any
	nodeSnapshotLister     any
	nodedistributionLister any
	instanceType           string

	store *membolt.BoltCacheStore[*templatetypes.LocalRunTemplate]

	maxUnusedTemplateDuration time.Duration

	lock              sync.RWMutex
	unusedTemplateMap map[string]*unusedTemplate

	refreshMu      sync.Mutex
	refreshRunning bool
	refreshPending bool
	refreshDone    chan struct{}
	refreshTimeout time.Duration
	refreshFn      func(context.Context)
}

func NewCubeRunTemplateManager(db multimeta.MetadataDBAPI, _ any) (*localCubeRunTemplateManager, error) {
	store, err := membolt.NewBoltCacheStore(db, taskIDKeyFunc, indexer, &templatetypes.LocalRunTemplate{})
	if err != nil {
		return nil, err
	}
	manager := &localCubeRunTemplateManager{
		store:                     store,
		unusedTemplateMap:         make(map[string]*unusedTemplate),
		maxUnusedTemplateDuration: 2 * 24 * time.Hour,
		refreshTimeout:            defaultInventoryRefreshTimeout,
	}

	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeImage, &imageDeleteHook{manager})
	cdp.RegisterDeleteProtectionHook(cdp.ResourceDeleteProtectionTypeStorageBaseBlock, &baseBlockDeleteHook{manager})
	cdp.RegisterDeleteProtectionHook(cdp.ResourceTypeVmSnapshot, &snapshotDeleteHook{manager})
	return manager, nil
}

func (h *localCubeRunTemplateManager) IsReady() bool {
	return h.instanceType != ""
}

func (h *localCubeRunTemplateManager) ListLocalTemplates(ctx context.Context) (map[string]*templatetypes.LocalRunTemplate, error) {
	if !h.IsReady() {
		return nil, fmt.Errorf("local template manager is not ready")
	}
	// Never block node-liveness heartbeats on filesystem scans or storage RPCs:
	// serve from the in-memory store and reconcile asynchronously.
	h.triggerInventoryRefresh()
	templates, err := h.store.ListGeneric()
	if err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Error("failed to list local templates")
		return nil, err
	}
	templateMap := make(map[string]*templatetypes.LocalRunTemplate)
	for _, template := range templates {
		if template == nil {
			continue
		}
		if storage.S3PackageOperationInProgress(template.TemplateID) {
			continue
		}
		templateMap[template.TemplateID] = template.Clone()
	}
	return templateMap, nil
}

// triggerInventoryRefresh schedules a coalesced background template discovery
// pass without blocking the caller. At most one pass runs at a time.
func (h *localCubeRunTemplateManager) triggerInventoryRefresh() {
	h.refreshMu.Lock()
	if h.refreshRunning {
		h.refreshPending = true
		h.refreshMu.Unlock()
		return
	}
	h.refreshRunning = true
	h.refreshPending = false
	done := make(chan struct{})
	h.refreshDone = done
	fn := h.refreshFn
	if fn == nil {
		fn = h.refreshLocalTemplates
	}
	timeout := h.refreshTimeout
	if timeout <= 0 {
		timeout = defaultInventoryRefreshTimeout
	}
	h.refreshMu.Unlock()

	go func() {
		defer func() {
			h.refreshMu.Lock()
			h.refreshRunning = false
			close(done)
			h.refreshMu.Unlock()
		}()
		for {
			refreshCtx, cancel := context.WithTimeout(context.Background(), timeout)
			fn(refreshCtx)
			cancel()

			h.refreshMu.Lock()
			if !h.refreshPending {
				h.refreshMu.Unlock()
				return
			}
			h.refreshPending = false
			h.refreshMu.Unlock()
		}
	}()
}

// waitInventoryRefresh waits for any in-flight background inventory refresh to
// finish. Used by tests for deterministic synchronization.
func (h *localCubeRunTemplateManager) waitInventoryRefresh() {
	h.refreshMu.Lock()
	done := h.refreshDone
	h.refreshMu.Unlock()
	if done != nil {
		<-done
	}
}

func (h *localCubeRunTemplateManager) refreshLocalTemplates(ctx context.Context) {
	if err := h.recoverLocalTemplatesFromSnapshotRoot(ctx, constants.DefaultSnapshotDir, ""); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Warn("failed to recover local templates from snapshot root")
	}
	if err := h.recoverBackendMetadataHomes(ctx, cow.BackendXFS, "cubebox", ""); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Warn("failed to recover local templates from xfs snapshot root")
	}
	if err := h.recoverBackendMetadataHomes(ctx, cow.BackendJFS, "cubebox", ""); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Warn("failed to recover local templates from jfs snapshot root")
	}
	if err := h.recoverS3LocalTemplates(ctx, ""); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Warn("failed to recover local templates from s3 snapshot root")
	}
	if err := h.removeMissingLocalTemplates(ctx); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Warn("failed to remove missing local templates")
	}
}

// removeMissingLocalTemplates reconciles the persisted local-template store
// with snapshot data on disk. CleanupTemplate removes snapshot directories,
// but historical records may remain in this store and otherwise continue to
// be reported in every node heartbeat.
func (h *localCubeRunTemplateManager) removeMissingLocalTemplates(ctx context.Context) error {
	templates, err := h.store.ListGeneric()
	if err != nil {
		return err
	}
	for _, template := range templates {
		if template == nil {
			continue
		}
		snapshotPath := strings.TrimSpace(template.Snapshot.Snapshot.Path)
		if snapshotPath == "" {
			continue
		}
		exists, err := utils.DenExist(snapshotPath)
		if err != nil {
			log.G(ctx).WithFields(CubeLog.Fields{
				"template_id": template.TemplateID,
				"path":        snapshotPath,
				"err":         err.Error(),
			}).Warn("failed to inspect local template path")
			continue
		}
		if exists {
			continue
		}

		// Snapshot writers build the replacement under <snapshotPath>.tmp,
		// then remove the old final directory and rename the temporary one.
		// During that publish window the final path is briefly absent. Check
		// the temporary path, then the final path again in case the rename
		// completed between the two checks, before treating metadata as stale.
		for _, path := range []string{snapshotPath + ".tmp", snapshotPath} {
			exists, err = utils.DenExist(path)
			if err != nil {
				log.G(ctx).WithFields(CubeLog.Fields{
					"template_id": template.TemplateID,
					"path":        path,
					"err":         err.Error(),
				}).Warn("failed to inspect local template path")
				break
			}
			if exists {
				break
			}
		}
		if err != nil || exists {
			continue
		}
		if err := h.store.Delete(template); err != nil {
			return fmt.Errorf("delete missing local template %s: %w", template.TemplateID, err)
		}
		log.G(ctx).WithFields(CubeLog.Fields{
			"template_id": template.TemplateID,
			"path":        snapshotPath,
		}).Info("removed missing local template metadata")
	}
	return nil
}

func (h *localCubeRunTemplateManager) EnsureCubeRunTemplate(ctx context.Context, templateID, backend string) (*templatetypes.LocalRunTemplate, error) {
	h.lock.Lock()
	delete(h.unusedTemplateMap, templateID)
	h.lock.Unlock()

	if !h.IsReady() {
		return nil, fmt.Errorf("local template manager is not ready")
	}
	cloned, err := h.cloneAndHydrate(templateID)
	if err != nil {
		return nil, err
	}
	if cloned != nil {
		return cloned, nil
	}
	backend, err = cow.NormalizeBackend(backend)
	if err != nil {
		return nil, err
	}
	reportRecoveryError := func(err error) {
		if err != nil {
			log.G(ctx).WithFields(CubeLog.Fields{
				"template_id": templateID,
				"backend":     backend,
				"err":         err.Error(),
			}).Warn("failed to recover run template")
		}
	}
	// Only inspect the requested backend. In particular, a local XFS miss
	// must never wait for a shared JuiceFS mount or scan other nodes.
	switch backend {
	case cow.BackendXFS:
		reportRecoveryError(h.recoverLocalTemplatesFromSnapshotRoot(ctx, constants.DefaultSnapshotDir, templateID))
		if cloned, err := h.cloneAndHydrate(templateID); err != nil || cloned != nil {
			return cloned, err
		}
		reportRecoveryError(h.recoverBackendMetadataHomes(ctx, backend, "cubebox", templateID))
	case cow.BackendS3:
		if recovered, err := h.recoverS3LocalTemplateForEnsure(ctx, templateID); err != nil {
			reportRecoveryError(err)
		} else if recovered != nil {
			return recovered, nil
		}
	case cow.BackendJFS:
		reportRecoveryError(h.recoverBackendMetadataHomes(ctx, backend, "cubebox", templateID))
		if cloned, err := h.cloneAndHydrate(templateID); err != nil || cloned != nil {
			return cloned, err
		}
	}
	cloned, err = h.cloneAndHydrate(templateID)
	if err != nil {
		return nil, err
	}
	if cloned != nil {
		return cloned, nil
	}
	log.G(ctx).WithFields(CubeLog.Fields{
		"template_id": templateID,
	}).Warn("template is not available in local metadata store")
	return nil, fmt.Errorf("template %s is not available locally", templateID)
}

func (h *localCubeRunTemplateManager) SetInstanceType(instanceType string) {
	h.instanceType = instanceType
}

func (h *localCubeRunTemplateManager) recoverS3LocalTemplates(ctx context.Context, templateID string) error {
	id := strings.TrimSpace(templateID)
	if id != "" {
		_, err := h.recoverS3LocalTemplateForEnsure(ctx, id)
		return err
	}
	// Inventory discovery only inspects reusable templates under snapshots/.
	// Completed pause packages under pause-snapshots/ are internal per-sandbox
	// Resume artifacts and must never be scanned or mounted by heartbeat
	// discovery, otherwise idle paused sandboxes leak permanent NVMe namespaces.
	normalRoot := recoverySnapshotKindRoot(cow.BackendS3, storage.SnapshotKindNormal)
	if err := h.recoverMetadataHomes(ctx, normalRoot, cow.BackendS3, ""); err != nil {
		return err
	}
	// Finalized S3 templates keep snapshot/config.json on a sealed, unmounted
	// metadata snapshot while clearing host metadata/. Probe read-only catalog
	// existence without cloning, activating, or mounting any metadata disk.
	for _, pkgID := range s3PackageIDsUnder(ctx, normalRoot) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if storage.S3PackageOperationInProgress(pkgID) {
			continue
		}
		home := filepath.Join(normalRoot, pkgID)
		if _, err := os.Stat(metadataHomeConfigPath(home)); err == nil {
			continue
		}
		h.recoverSealedS3Package(ctx, pkgID, home, filepath.Join(home, storage.SnapshotMetadataDir), true)
	}
	return nil
}

// recoverS3LocalTemplateForEnsure resolves one explicit S3 package ID on demand
// (create from template/snapshot or Resume fallback to a pause package) without
// mounting or retaining the package's metadata disk.
func (h *localCubeRunTemplateManager) recoverS3LocalTemplateForEnsure(ctx context.Context, templateID string) (*templatetypes.LocalRunTemplate, error) {
	id := strings.TrimSpace(templateID)
	if id == "" {
		return nil, nil
	}
	if err := h.recoverBackendMetadataHomes(ctx, cow.BackendS3, cow.BackendS3, id); err != nil {
		return nil, err
	}
	if cloned, err := h.cloneAndHydrate(id); err != nil || cloned != nil {
		return cloned, err
	}
	for _, kind := range []string{storage.SnapshotKindNormal, storage.SnapshotKindPause} {
		root := recoverySnapshotKindRoot(cow.BackendS3, kind)
		if root == "" {
			continue
		}
		home := filepath.Join(root, id)
		if st, err := os.Stat(home); err != nil || !st.IsDir() {
			continue
		}
		metaDir := filepath.Join(home, storage.SnapshotMetadataDir)
		persist := kind == storage.SnapshotKindNormal
		if tpl := h.recoverSealedS3Package(ctx, id, home, metaDir, persist); tpl != nil {
			return tpl.Clone(), nil
		}
	}
	return nil, nil
}

// recoverSealedS3Package builds a LocalRunTemplate for a finalized S3 package
// whose host metadata directory was cleared after sealing, using read-only
// catalog/object probing so no RW clone or NVMe namespace is attached.
func (h *localCubeRunTemplateManager) recoverSealedS3Package(ctx context.Context, id, home, metaDir string, persist bool) *templatetypes.LocalRunTemplate {
	if storage.S3PackageOperationInProgress(id) {
		return nil
	}
	entry, err := recoveryS3CatalogLookup(ctx, cow.BackendS3, id)
	if err != nil || entry == nil {
		return nil
	}
	if strings.TrimSpace(entry.RootfsVol) == "" || strings.TrimSpace(entry.MetadataVol) == "" {
		return nil
	}
	// Re-check after lookup so a write/cleanup that started concurrently is
	// never published prematurely.
	if storage.S3PackageOperationInProgress(id) {
		return nil
	}
	if entry.SpecDir != "" {
		_ = storage.EnsureShimSpecDirLink(home, entry.SpecDir)
	}
	if strings.TrimSpace(entry.MetaDir) != "" {
		metaDir = filepath.Clean(entry.MetaDir)
	}
	template := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			Namespace:          "default",
			Name:               "recovered-" + id,
			DistributionName:   "recovered-" + id,
			DistributionTaskID: "recovered-" + id,
			TemplateID:         id,
		},
		Snapshot: templatetypes.LocalSnapshot{
			Snapshot: templatetypes.Snapshot{
				ID:    id,
				Media: cow.BackendS3,
				Path:  metaDir,
			},
		},
		Volumes:  map[string]templatetypes.LocalBaseVolume{},
		Componts: map[string]templatetypes.LocalComponent{},
	}
	if len(entry.ComponentVersions) > 0 {
		templatetypes.ApplyVersionMap(template, entry.ComponentVersions)
	} else if existing, err := h.cloneAndHydrate(id); err == nil && existing != nil && len(existing.Componts) > 0 {
		// Preserve previously persisted component versions across restarts when
		// deriveS3CatalogEntry cannot read catalog.json from the unmounted snap.
		template.Componts = existing.Componts
	}
	if persist {
		if err := h.store.Update(template); err != nil {
			log.G(ctx).WithFields(CubeLog.Fields{
				"template_id": template.TemplateID,
				"path":        template.Snapshot.Snapshot.Path,
				"err":         err.Error(),
			}).Warn("failed to persist recovered sealed s3 local template")
			return nil
		}
	}
	return template
}

func s3PackageIDsUnder(ctx context.Context, roots ...string) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			if !os.IsNotExist(err) {
				log.G(ctx).WithFields(CubeLog.Fields{
					"path": root,
					"err":  err.Error(),
				}).Warn("failed to list s3 snapshot homes for template recovery")
			}
			continue
		}
		// Sandbox homes share <work>/s3/snapshots/<id> with template packages.
		// Only tpl- directories there are reusable template packages.
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			name := ent.Name()
			if name == "" || strings.HasSuffix(name, ".tmp") {
				continue
			}
			if !strings.HasPrefix(name, "tpl-") {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			ids = append(ids, name)
		}
	}
	sort.Strings(ids)
	return ids
}

// metadataHomeConfigPath is the cube-runtime config a package home must have
// for the recovery scan to accept it as a run template.
func metadataHomeConfigPath(home string) string {
	return filepath.Join(home, storage.SnapshotMetadataDir, runTemplateSnapshotDir, runTemplateConfigFile)
}

func (h *localCubeRunTemplateManager) recoverBackendMetadataHomes(ctx context.Context, backend, media, templateID string) error {
	kinds := []string{storage.SnapshotKindNormal}
	if strings.TrimSpace(templateID) != "" {
		kinds = append(kinds, storage.SnapshotKindPause)
	}
	var first error
	for _, kind := range kinds {
		if err := h.recoverMetadataHomes(ctx, recoverySnapshotKindRoot(backend, kind), media, templateID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (h *localCubeRunTemplateManager) recoverMetadataHomes(ctx context.Context, kindRoot, media, templateID string) error {
	if kindRoot == "" {
		return nil
	}
	pattern := metadataHomeConfigPath(filepath.Join(kindRoot, "*"))
	if templateID != "" {
		pattern = metadataHomeConfigPath(filepath.Join(kindRoot, templateID))
	}
	configPaths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	for _, configPath := range configPaths {
		metaDir := filepath.Dir(filepath.Dir(configPath))
		home := filepath.Dir(metaDir)
		if media == cow.BackendS3 && storage.S3PackageOperationInProgress(filepath.Base(home)) {
			continue
		}
		template := recoveredS3LocalTemplate(home, metaDir)
		if template == nil {
			continue
		}
		if entry, err := storage.ReadSnapshotCatalogAt(home); err == nil && entry != nil {
			_ = storage.EnsureShimSpecDirLink(home, entry.SpecDir)
		}
		if media != "" {
			template.Snapshot.Snapshot.Media = media
		}
		if err := h.store.Update(template); err != nil {
			log.G(ctx).WithFields(CubeLog.Fields{
				"template_id": template.TemplateID,
				"path":        template.Snapshot.Snapshot.Path,
				"err":         err.Error(),
			}).Warn("failed to persist recovered local template")
		}
	}
	return nil
}

func recoveredS3LocalTemplate(home, metaDir string) *templatetypes.LocalRunTemplate {
	if home == "" || metaDir == "" {
		return nil
	}
	home = filepath.Clean(home)
	metaDir = filepath.Clean(metaDir)
	if isTemporarySnapshotPath(home) {
		return nil
	}
	configPath := filepath.Join(metaDir, runTemplateSnapshotDir, runTemplateConfigFile)
	if _, err := os.Stat(configPath); err != nil {
		return nil
	}
	templateID := filepath.Base(home)
	if templateID == "." || templateID == string(filepath.Separator) || templateID == "" {
		return nil
	}
	template := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			Namespace:          "default",
			Name:               "recovered-" + templateID,
			DistributionName:   "recovered-" + templateID,
			DistributionTaskID: "recovered-" + templateID,
			TemplateID:         templateID,
		},
		Snapshot: templatetypes.LocalSnapshot{
			Snapshot: templatetypes.Snapshot{
				ID:    templateID,
				Media: "s3",
				Path:  metaDir,
			},
		},
		Volumes:  map[string]templatetypes.LocalBaseVolume{},
		Componts: map[string]templatetypes.LocalComponent{},
	}
	hydrateLocalTemplateComponentVersions(template)
	return template
}

func (h *localCubeRunTemplateManager) recoverLocalTemplatesFromSnapshotRoot(ctx context.Context, snapshotRoot string, templateID string) error {
	if snapshotRoot == "" {
		return nil
	}
	pattern := filepath.Join(snapshotRoot, "*", "*", "*", "snapshot", "config.json")
	if templateID != "" {
		pattern = filepath.Join(snapshotRoot, "*", templateID, "*", "snapshot", "config.json")
	}
	configPaths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	for _, configPath := range configPaths {
		basePath := filepath.Dir(filepath.Dir(configPath))
		template := recoveredLocalTemplateFromSnapshotPath(basePath)
		if template == nil {
			continue
		}
		if err := h.store.Update(template); err != nil {
			log.G(ctx).WithFields(CubeLog.Fields{
				"template_id": template.TemplateID,
				"path":        template.Snapshot.Snapshot.Path,
				"err":         err.Error(),
			}).Warn("failed to persist recovered local template")
		}
	}
	return nil
}

func recoveredLocalTemplateFromSnapshotPath(snapshotPath string) *templatetypes.LocalRunTemplate {
	if snapshotPath == "" {
		return nil
	}
	snapshotPath = filepath.Clean(snapshotPath)
	if isTemporarySnapshotPath(snapshotPath) {
		return nil
	}
	configPath := filepath.Join(snapshotPath, "snapshot", "config.json")
	if _, err := os.Stat(configPath); err != nil {
		return nil
	}
	specID := filepath.Base(snapshotPath)
	templateDir := filepath.Dir(snapshotPath)
	templateID := filepath.Base(templateDir)
	if templateID == "." || templateID == string(filepath.Separator) || templateID == "" {
		return nil
	}
	instanceType := filepath.Base(filepath.Dir(templateDir))
	template := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			Namespace:          "default",
			Name:               "recovered-" + templateID,
			DistributionName:   "recovered-" + templateID,
			DistributionTaskID: "recovered-" + templateID,
			TemplateID:         templateID,
		},
		Snapshot: templatetypes.LocalSnapshot{
			Snapshot: templatetypes.Snapshot{
				ID:    specID,
				Media: instanceType,
				Path:  snapshotPath,
			},
		},
		Volumes:  map[string]templatetypes.LocalBaseVolume{},
		Componts: map[string]templatetypes.LocalComponent{},
	}
	hydrateLocalTemplateComponentVersions(template)
	return template
}

func isTemporarySnapshotPath(snapshotPath string) bool {
	base := filepath.Base(filepath.Clean(snapshotPath))
	return strings.HasSuffix(base, ".tmp")
}

func (h *localCubeRunTemplateManager) cloneAndHydrate(templateID string) (*templatetypes.LocalRunTemplate, error) {
	templates, err := h.store.ByIndexGeneric(templateIDIndexerKey, templateID)
	if err != nil {
		return nil, err
	}
	for _, template := range templates {
		if template == nil {
			continue
		}
		cloned := template.Clone()
		hydrateLocalTemplateComponentVersions(cloned)
		return cloned, nil
	}
	return nil, nil
}

func hydrateLocalTemplateComponentVersions(local *templatetypes.LocalRunTemplate) {
	if local == nil {
		return
	}
	if len(templatetypes.VersionMapFromComponts(local)) > 0 {
		return
	}
	snapshotPath := local.Snapshot.Snapshot.Path
	if snapshotPath == "" {
		return
	}
	entry, err := storage.ReadSnapshotCatalogAt(snapshotPath)
	if err != nil || entry == nil || len(entry.ComponentVersions) == 0 {
		return
	}
	templatetypes.ApplyVersionMap(local, entry.ComponentVersions)
}

type unusedTemplate struct {
	localTemplate *templatetypes.LocalRunTemplate
	detectedTime  time.Time
}
