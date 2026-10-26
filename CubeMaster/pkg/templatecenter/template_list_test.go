// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---- sliceTemplateInfos ----

func TestSliceTemplateInfos(t *testing.T) {
	infos := []TemplateInfo{
		{TemplateID: "tpl-0"},
		{TemplateID: "tpl-1"},
		{TemplateID: "tpl-2"},
		{TemplateID: "tpl-3"},
	}
	tests := []struct {
		name    string
		opts    TemplateListOptions
		wantIDs []string
	}{
		{"limit only", TemplateListOptions{Limit: 2}, []string{"tpl-0", "tpl-1"}},
		{"limit and offset", TemplateListOptions{Limit: 2, Offset: 1}, []string{"tpl-1", "tpl-2"}},
		{"offset only keeps tail", TemplateListOptions{Offset: 2}, []string{"tpl-2", "tpl-3"}},
		{"limit beyond length clamps", TemplateListOptions{Limit: 10, Offset: 1}, []string{"tpl-1", "tpl-2", "tpl-3"}},
		{"offset at boundary returns empty", TemplateListOptions{Offset: 4}, []string{}},
		{"offset past boundary returns empty", TemplateListOptions{Offset: 99}, []string{}},
		{"negative offset treated as zero", TemplateListOptions{Limit: 1, Offset: -3}, []string{"tpl-0"}},
		{"non-positive limit keeps tail", TemplateListOptions{Limit: 0, Offset: 3}, []string{"tpl-3"}},
		{"negative limit keeps tail", TemplateListOptions{Limit: -1, Offset: 1}, []string{"tpl-1", "tpl-2", "tpl-3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sliceTemplateInfos(infos, tt.opts)
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("sliceTemplateInfos(%+v) = %d entries, want %d", tt.opts, len(got), len(tt.wantIDs))
			}
			for i := range tt.wantIDs {
				if got[i].TemplateID != tt.wantIDs[i] {
					t.Fatalf("sliceTemplateInfos(%+v)[%d] = %s, want %s", tt.opts, i, got[i].TemplateID, tt.wantIDs[i])
				}
			}
		})
	}

	if got := sliceTemplateInfos(nil, TemplateListOptions{Limit: 2, Offset: 1}); len(got) != 0 {
		t.Fatalf("sliceTemplateInfos(nil) = %v, want empty", got)
	}
}

// ---- ListTemplatesWithOptions ----

func TestListTemplatesWithOptionsServesPagesFromCache(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()
	store.db = stubListTemplatesDB(t, nil, nil)
	templateListCache.Flush()
	defer templateListCache.Flush()

	setTemplateListCache([]TemplateInfo{
		{TemplateID: "tpl-a"},
		{TemplateID: "tpl-b"},
		{TemplateID: "tpl-c"},
	})

	got, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{})
	if err != nil {
		t.Fatalf("unpaged ListTemplatesWithOptions: %v", err)
	}
	if got.Total != 3 || len(got.Templates) != 3 {
		t.Fatalf("unpaged = total %d len %d, want 3/3", got.Total, len(got.Templates))
	}

	// Consecutive pages must concatenate to the full cached list.
	var paged []string
	for offset := 0; ; offset += 2 {
		page, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("page offset=%d: %v", offset, err)
		}
		if page.Total != 3 {
			t.Fatalf("page offset=%d total = %d, want 3", offset, page.Total)
		}
		for _, info := range page.Templates {
			paged = append(paged, info.TemplateID)
		}
		if len(page.Templates) == 0 {
			break
		}
	}
	want := []string{"tpl-a", "tpl-b", "tpl-c"}
	if len(paged) != len(want) {
		t.Fatalf("paged concat = %v, want %v", paged, want)
	}
	for i := range want {
		if paged[i] != want[i] {
			t.Fatalf("paged concat = %v, want %v", paged, want)
		}
	}

	page, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{Limit: 2, Offset: 3})
	if err != nil {
		t.Fatalf("past-end page: %v", err)
	}
	if page.Total != 3 || len(page.Templates) != 0 {
		t.Fatalf("past-end page = total %d len %d, want 3/0", page.Total, len(page.Templates))
	}
}

func TestListTemplatesBackCompatReturnsAll(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()
	store.db = stubListTemplatesDB(t, nil, nil)
	templateListCache.Flush()
	defer templateListCache.Flush()

	setTemplateListCache([]TemplateInfo{{TemplateID: "tpl-a"}, {TemplateID: "tpl-b"}})
	got, err := ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(got) != 2 || got[0].TemplateID != "tpl-a" {
		t.Fatalf("ListTemplates = %v", got)
	}
}

func TestListTemplatesWithOptionsNotInitialized(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()
	store.db = nil
	if _, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{Limit: 1}); err != ErrTemplateStoreNotInitialized {
		t.Fatalf("err = %v, want ErrTemplateStoreNotInitialized", err)
	}
}

// ---- ListReplicasForTemplates ----

func stubReplicasDB(t *testing.T, replicas []models.TemplateReplica) *gorm.DB {
	t.Helper()
	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		if dest, ok := tx.Statement.Dest.(*[]models.TemplateReplica); ok {
			*dest = append([]models.TemplateReplica(nil), replicas...)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestListReplicasForTemplatesGroupsByID(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()
	store.db = stubReplicasDB(t, []models.TemplateReplica{
		{TemplateID: "tpl-a", NodeID: "node-1", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		{TemplateID: "tpl-a", NodeID: "node-2", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		{TemplateID: "tpl-b", NodeID: "node-1", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
	})

	got, err := ListReplicasForTemplates(context.Background(), []string{"tpl-a", "tpl-b"})
	if err != nil {
		t.Fatalf("ListReplicasForTemplates: %v", err)
	}
	if len(got["tpl-a"]) != 2 || got["tpl-a"][0].NodeID != "node-1" || got["tpl-a"][1].NodeID != "node-2" {
		t.Fatalf("tpl-a replicas = %v", got["tpl-a"])
	}
	if len(got["tpl-b"]) != 1 || got["tpl-b"][0].Status != ReplicaStatusReady {
		t.Fatalf("tpl-b replicas = %v", got["tpl-b"])
	}
}

func TestListReplicasForTemplatesEmptyIDs(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()
	store.db = stubReplicasDB(t, nil)
	got, err := ListReplicasForTemplates(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListReplicasForTemplates(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestListReplicasForTemplatesNotInitialized(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()
	store.db = nil
	if _, err := ListReplicasForTemplates(context.Background(), []string{"tpl-a"}); err != ErrTemplateStoreNotInitialized {
		t.Fatalf("err = %v, want ErrTemplateStoreNotInitialized", err)
	}
}

// inClauseExpr digs the clause.Expr carrying "template_id IN ?" out of the
// statement's WHERE clause.
func inClauseExpr(tx *gorm.DB) *clause.Expr {
	where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
	if !ok || len(where.Exprs) == 0 {
		return nil
	}
	for i := range where.Exprs {
		expr, ok := where.Exprs[i].(clause.Expr)
		if !ok || len(expr.Vars) == 0 || !strings.Contains(expr.SQL, "IN") {
			continue
		}
		return &expr
	}
	return nil
}

// orderBySQL extracts the raw ORDER BY text of a statement, if any.
func orderBySQL(tx *gorm.DB) string {
	ob, ok := tx.Statement.Clauses["ORDER BY"].Expression.(clause.OrderBy)
	if !ok {
		return ""
	}
	for _, col := range ob.Columns {
		if col.Column.Raw {
			return col.Column.Name
		}
	}
	return ""
}

// TestListTemplatesFromDBOrdersTotally pins the paging contract: the
// definition query must order by a unique key combination so equal-second
// rows keep a stable order across cache refreshes.
func TestListTemplatesFromDBOrdersTotally(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()
	templateListCache.Flush()
	defer templateListCache.Flush()

	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	var gotOrder string
	if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.TemplateDefinition); ok {
			gotOrder = orderBySQL(tx)
		}
	}); err != nil {
		t.Fatal(err)
	}
	store.db = db

	if _, err := listTemplatesFromDB(context.Background()); err != nil {
		t.Fatalf("listTemplatesFromDB: %v", err)
	}
	if !strings.Contains(gotOrder, "updated_at desc") || !strings.Contains(gotOrder, "template_id asc") {
		t.Fatalf("definition order = %q, want a unique tiebreaker like 'updated_at desc, template_id asc'", gotOrder)
	}
}

// stubChunkedReplicasDB records each batch's IN ids and answers one READY
// replica per id; a non-zero failOnBatch errors that batch.
func stubChunkedReplicasDB(t *testing.T, failOnBatch int) (*gorm.DB, *[][]string) {
	t.Helper()
	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	var batches [][]string
	if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		dest, ok := tx.Statement.Dest.(*[]models.TemplateReplica)
		if !ok {
			return
		}
		expr := inClauseExpr(tx)
		if expr == nil {
			t.Errorf("unexpected WHERE clause: %#v", tx.Statement.Clauses["WHERE"].Expression)
			return
		}
		ids, ok := expr.Vars[0].([]string)
		if !ok {
			t.Errorf("unexpected IN var type %T", expr.Vars[0])
			return
		}
		batches = append(batches, append([]string(nil), ids...))
		if failOnBatch > 0 && len(batches) == failOnBatch {
			tx.AddError(errors.New("batch query failed"))
			return
		}
		for _, id := range ids {
			*dest = append(*dest, models.TemplateReplica{
				TemplateID: id,
				NodeID:     "node-" + id,
				NodeIP:     "10.0.0.1",
				Status:     ReplicaStatusReady,
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	return db, &batches
}

func TestListReplicasForTemplatesChunksLargeIDSets(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()

	ids := make([]string, 0, 1500)
	for i := 0; i < 1500; i++ {
		ids = append(ids, fmt.Sprintf("tpl-chunk-%04d", i))
	}
	db, batches := stubChunkedReplicasDB(t, 0)
	store.db = db

	got, err := ListReplicasForTemplates(context.Background(), ids)
	if err != nil {
		t.Fatalf("ListReplicasForTemplates: %v", err)
	}
	if len(*batches) != 2 {
		t.Fatalf("expected 2 batches for 1500 ids, got %d", len(*batches))
	}
	for i, batch := range *batches {
		want := 1000
		if i == 1 {
			want = 500
		}
		if len(batch) != want {
			t.Fatalf("batch %d size = %d, want %d", i, len(batch), want)
		}
	}
	if len(got) != 1500 {
		t.Fatalf("grouped %d templates, want 1500", len(got))
	}
	for _, id := range ids {
		reps := got[id]
		if len(reps) != 1 || reps[0].NodeID != "node-"+id {
			t.Fatalf("replicas for %s = %v", id, reps)
		}
	}
}

func TestListReplicasForTemplatesFailsOnBatchError(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()

	ids := make([]string, 0, 1500)
	for i := 0; i < 1500; i++ {
		ids = append(ids, fmt.Sprintf("tpl-chunk-%04d", i))
	}
	db, batches := stubChunkedReplicasDB(t, 2) // fail on the second batch
	store.db = db

	_, err := ListReplicasForTemplates(context.Background(), ids)
	if err == nil || !strings.Contains(err.Error(), "batch query failed") {
		t.Fatalf("want batch error, got %v", err)
	}
	if len(*batches) != 2 {
		t.Fatalf("expected the failing call on batch 2, got %d batches", len(*batches))
	}
}

func TestListReplicasForTemplatesDedupesAndSkipsEmpty(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()

	db, batches := stubChunkedReplicasDB(t, 0)
	store.db = db

	got, err := ListReplicasForTemplates(context.Background(), []string{"tpl-a", "", "tpl-a", "tpl-b", "tpl-b"})
	if err != nil {
		t.Fatalf("ListReplicasForTemplates: %v", err)
	}
	if len(*batches) != 1 {
		t.Fatalf("expected a single query, got %d", len(*batches))
	}
	if batch := (*batches)[0]; len(batch) != 2 || batch[0] != "tpl-a" || batch[1] != "tpl-b" {
		t.Fatalf("deduped batch = %v, want [tpl-a tpl-b]", batch)
	}
	if len(got) != 2 {
		t.Fatalf("grouped %d templates, want 2", len(got))
	}
}
