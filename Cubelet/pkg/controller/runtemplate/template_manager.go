// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type RunTemplateManager interface {
	SetInstanceType(instanceType string)

	EnsureCubeRunTemplate(ctx context.Context, templateID string) (*templatetypes.LocalRunTemplate, error)

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
	if err := h.recoverLocalTemplatesFromSnapshotRoot(ctx, constants.DefaultSnapshotDir, ""); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Warn("failed to recover local templates from snapshot root")
	}
	if err := h.removeMissingLocalTemplates(ctx); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Warn("failed to remove missing local templates")
	}
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
		templateMap[template.TemplateID] = template.Clone()
	}
	return templateMap, nil
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

func (h *localCubeRunTemplateManager) EnsureCubeRunTemplate(ctx context.Context, templateID string) (*templatetypes.LocalRunTemplate, error) {
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
	if err := h.recoverLocalTemplatesFromSnapshotRoot(ctx, constants.DefaultSnapshotDir, templateID); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"template_id": templateID,
			"err":         err.Error(),
		}).Warn("failed to recover template from snapshot root")
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
