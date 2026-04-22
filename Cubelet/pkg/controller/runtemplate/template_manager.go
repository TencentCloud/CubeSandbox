// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
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
	templates, err := h.store.ListGeneric()
	if err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"err": err.Error(),
		}).Error("failed to list local templates")
		return nil, err
	}
	templateMap := make(map[string]*templatetypes.LocalRunTemplate)
	for _, template := range templates {
		templateMap[template.TemplateID] = template
	}
	return templateMap, nil
}

func (h *localCubeRunTemplateManager) EnsureCubeRunTemplate(ctx context.Context, templateID string) (*templatetypes.LocalRunTemplate, error) {

	h.lock.Lock()
	delete(h.unusedTemplateMap, templateID)
	h.lock.Unlock()

	if !h.IsReady() {
		return nil, fmt.Errorf("local template manager is not ready")
	}
	templates, err := h.store.ByIndexGeneric(templateIDIndexerKey, templateID)
	if err != nil {
		return nil, err
	}
	for _, template := range templates {
		if template != nil {
			return template, nil
		}
	}
	if err := h.recoverLocalTemplatesFromSnapshotRoot(ctx, constants.DefaultSnapshotDir, templateID); err != nil {
		log.G(ctx).WithFields(CubeLog.Fields{
			"template_id": templateID,
			"err":         err.Error(),
		}).Warn("failed to recover template from snapshot root")
	}
	templates, err = h.store.ByIndexGeneric(templateIDIndexerKey, templateID)
	if err != nil {
		return nil, err
	}
	for _, template := range templates {
		if template != nil {
			return template, nil
		}
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
	return &templatetypes.LocalRunTemplate{
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
		Components: map[string]templatetypes.LocalComponent{},
	}
}

type unusedTemplate struct {
	localTemplate *templatetypes.LocalRunTemplate
	detectedTime  time.Time
}
