//go:build integration

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestGetTemplateByAliasIntegrationExcludesSnapshots is the behavioral
// integration test for the issue-#584 read-path alias fix. It runs against a
// REAL MySQL instance: it is excluded from the default `go test` suite by the
// `integration` build tag and only runs when CUBE_TEST_DB_DSN points at a live
// database (cube:<pwd>@tcp(127.0.0.1:3306)/cube_mvp?parseTime=true).
//
// It inserts a TEMPLATE-kind row and a SNAPSHOT-kind row, each with its own
// distinct display_name used as an alias, then proves the read path only
// resolves aliases to template-kind definitions: the template alias resolves
// (Assertion A) while the snapshot alias returns ErrTemplateNotFound
// (Assertion B). Without the `kind = template` filter in GetTemplateByAlias,
// Assertion B fails because First() returns the snapshot row. Both rows are
// hard-deleted in cleanup so the dev DB is left clean even on failure.
func TestGetTemplateByAliasIntegrationExcludesSnapshots(t *testing.T) {
	dsn := os.Getenv("CUBE_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("set CUBE_TEST_DB_DSN to run this integration test")
	}

	sqlDB, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)
	if err := sqlDB.Ping(); err != nil {
		t.Skipf("MySQL unreachable: %v", err)
	}

	// Swap the package-level store DB so GetTemplateByAlias uses our live
	// connection, restoring the original afterwards.
	oldDB := store.db
	store.db = gormDB
	t.Cleanup(func() { store.db = oldDB })

	suf := fmt.Sprintf("584int-%d", time.Now().UnixNano())
	tplID := "tpl-test-" + suf
	snapID := "snap-test-" + suf
	tplAlias := "alias-tpl-" + suf
	snapAlias := "alias-snap-" + suf

	tplDef := &models.TemplateDefinition{
		TemplateID:  tplID,
		Kind:        TemplateKindTemplate,
		DisplayName: tplAlias,
		Status:      "READY",
		RequestJSON: "{}",
	}
	snapDef := &models.TemplateDefinition{
		TemplateID:  snapID,
		Kind:        TemplateKindSnapshot,
		DisplayName: snapAlias,
		Status:      "READY",
		RequestJSON: "{}",
	}
	require.NoError(t, gormDB.Create(tplDef).Error)
	require.NoError(t, gormDB.Create(snapDef).Error)

	// Hard-delete both rows regardless of test outcome so the dev DB is left clean.
	t.Cleanup(func() {
		gormDB.Unscoped().Where("template_id IN ?", []string{tplID, snapID}).
			Delete(&models.TemplateDefinition{})
	})

	// Assertion A (positive): the template alias resolves to its definition.
	got, err := GetTemplateByAlias(context.Background(), tplAlias)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tplID, got.TemplateID)
	assert.Equal(t, TemplateKindTemplate, got.Kind)

	// Assertion B (the bug — negative): a snapshot's alias must NOT resolve.
	// Before the fix First() returned the snapshot row; the kind = template
	// filter now scopes the read to template-kind definitions only.
	_, err = GetTemplateByAlias(context.Background(), snapAlias)
	assert.True(t, errors.Is(err, ErrTemplateNotFound),
		"a snapshot's alias must NOT resolve; got err=%v, def resolved when it should be ErrTemplateNotFound", err)
}
