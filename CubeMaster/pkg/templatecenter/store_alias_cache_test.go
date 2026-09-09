// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// Regression for the master e2e cases/templates/test_alias.py failures:
// #1659 added process-local caches for template info/list, but
// SetTemplateAlias never invalidated them, so GET/list kept serving the
// previous alias until the cache TTL expired. The alias lives in the cached
// payloads as display_name, and an alias TRANSFER changes two rows (new
// holder + released holder), so both must be dropped.
func TestSetTemplateAliasInvalidatesQueryCaches(t *testing.T) {
	gormDB, _ := newArtifactGCMySQL(t)
	origDB := store.db
	store.db = gormDB
	t.Cleanup(func() { store.db = origDB })

	if err := gormDB.AutoMigrate(&models.TemplateDefinition{}, &models.TemplateImageJob{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The model deliberately omits alias_key (a STORED generated column), so
	// AutoMigrate cannot create it — add it exactly as the migration does.
	for _, ddl := range []string{
		"ALTER TABLE t_cube_template_definition ADD COLUMN alias_key varchar(256) GENERATED ALWAYS AS (CASE WHEN `kind` = 'template' AND `display_name` <> '' THEN `display_name` ELSE NULL END) STORED",
		"ALTER TABLE t_cube_template_definition ADD UNIQUE INDEX idx_template_definition_alias_unique (alias_key)",
	} {
		if err := gormDB.Exec(ddl).Error; err != nil {
			t.Fatalf("alias_key ddl: %v", err)
		}
	}

	seed := func(id string) {
		def := models.TemplateDefinition{
			TemplateID:   id,
			InstanceType: "cubebox",
			Version:      DefaultTemplateVersion,
			Status:       StatusReady,
			Kind:         "template",
		}
		if err := gormDB.Create(&def).Error; err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("tpl-alias-cache-a")
	seed("tpl-alias-cache-b")

	flushAndPrime := func(ids ...string) {
		templateInfoCache.Flush()
		templateListCache.Flush()
		infos := make([]TemplateInfo, 0, len(ids))
		for _, id := range ids {
			setTemplateInfoCache(id, &TemplateInfo{TemplateID: id, DisplayName: "stale"})
			infos = append(infos, TemplateInfo{TemplateID: id, DisplayName: "stale"})
		}
		setTemplateListCache(infos)
	}
	assertDropped := func(ids ...string) {
		t.Helper()
		for _, id := range ids {
			if _, hit := getCachedTemplateInfo(id); hit {
				t.Fatalf("info cache for %s must be dropped after the alias write", id)
			}
		}
		if _, hit := getCachedTemplateList(); hit {
			t.Fatal("list cache must be dropped after the alias write")
		}
	}

	ctx := context.Background()

	// Set.
	flushAndPrime("tpl-alias-cache-a")
	if err := SetTemplateAlias(ctx, "tpl-alias-cache-a", "web"); err != nil {
		t.Fatalf("set alias: %v", err)
	}
	assertDropped("tpl-alias-cache-a")

	// Transfer: the released holder's caches must be dropped too.
	flushAndPrime("tpl-alias-cache-a", "tpl-alias-cache-b")
	if err := SetTemplateAlias(ctx, "tpl-alias-cache-b", "web"); err != nil {
		t.Fatalf("transfer alias: %v", err)
	}
	assertDropped("tpl-alias-cache-a", "tpl-alias-cache-b")
	if def, err := GetTemplateByAlias(ctx, "web"); err != nil || def.TemplateID != "tpl-alias-cache-b" {
		t.Fatalf("alias must resolve to the new holder, got %+v, %v", def, err)
	}

	// Clear.
	flushAndPrime("tpl-alias-cache-b")
	if err := SetTemplateAlias(ctx, "tpl-alias-cache-b", ""); err != nil {
		t.Fatalf("clear alias: %v", err)
	}
	assertDropped("tpl-alias-cache-b")
}
