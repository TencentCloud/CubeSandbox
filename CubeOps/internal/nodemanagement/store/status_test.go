// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestStatusProjectionIncludesModelFields(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=localhost user=test dbname=test",
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	query := (&gormNodeStore{db: db}).statuses(context.Background())
	if err := query.Statement.Parse(&NodeStatus{}); err != nil {
		t.Fatal(err)
	}
	projection := strings.Join(query.Statement.Selects, ",")
	for _, column := range query.Statement.Schema.DBNames {
		expression := regexp.MustCompile(`\bt_cube_node_status\.` + regexp.QuoteMeta(column) + `\b`)
		if !expression.MatchString(projection) {
			t.Errorf("status projection omits model column %q", column)
		}
	}
}
