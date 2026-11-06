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
	"time"

	"github.com/glebarez/sqlite"
	_ "github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type templateListSQLRecorder struct {
	logger.Interface
	queries []string
}

func (r *templateListSQLRecorder) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	sql, _ := fc()
	r.queries = append(r.queries, sql)
}

func TestTemplateInfoFromJobBackend(t *testing.T) {
	for _, tc := range []struct{ request, want string }{
		{`{"backend":"S3"}`, "s3"}, {`{"backend":"xfs"}`, "xfs"}, {`{}`, "xfs"}, {`invalid`, ""},
	} {
		info := templateInfoFromJob(&models.TemplateImageJob{RequestJSON: tc.request})
		if info.Backend != tc.want || info.StorageBackend != tc.want {
			t.Fatalf("request=%s info=%+v", tc.request, info)
		}
	}
}

func TestListTemplatesLightweightPagination(t *testing.T) {
	db := newTemplateListSQLiteDB(t)
	for i := 0; i < 3; i++ {
		if err := db.Create(&models.TemplateDefinition{TemplateID: fmt.Sprintf("tpl-%d", i), Status: StatusReady}).Error; err != nil {
			t.Fatal(err)
		}
	}
	recorder := &templateListSQLRecorder{Interface: logger.Discard}
	db.Logger = recorder
	for _, tc := range []struct {
		offset, wantLen int
		more            bool
	}{{0, 2, true}, {2, 1, false}, {99, 0, false}} {
		page, err := ListTemplatesWithOptions(t.Context(), TemplateListOptions{Limit: 2, Offset: tc.offset, SkipTotal: true})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != -1 || len(page.Templates) != tc.wantLen || page.HasMore != tc.more {
			t.Fatalf("offset=%d page=%+v", tc.offset, page)
		}
	}
	if len(recorder.queries) == 0 {
		t.Fatal("SQL recorder did not observe any queries")
	}
	for _, query := range recorder.queries {
		if strings.Contains(query, "COUNT(*) AS total FROM visible_templates") {
			t.Fatal("lightweight page executed exact COUNT")
		}
	}
}

func TestListTemplatesBoundsHistoricalJobs(t *testing.T) {
	db := newTemplateListSQLiteDB(t)
	defs := make([]models.TemplateDefinition, 250)
	jobs := make([]models.TemplateImageJob, 0, 2000)
	for i := range defs {
		id := fmt.Sprintf("tpl-%04d", i)
		defs[i] = models.TemplateDefinition{TemplateID: id, Status: StatusReady}
		for attempt := 0; attempt < 8; attempt++ {
			jobs = append(jobs, models.TemplateImageJob{TemplateID: id, JobID: fmt.Sprintf("job-%d-%d", i, attempt), Operation: JobOperationCreate, AttemptNo: int32(attempt + 1)})
		}
	}
	if err := db.CreateInBatches(defs, 50).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.CreateInBatches(jobs, 50).Error; err != nil {
		t.Fatal(err)
	}
	page, err := ListTemplatesWithOptions(t.Context(), TemplateListOptions{Limit: 99999, SkipTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Templates) != MaxTemplateListLimit || !page.HasMore {
		t.Fatalf("unbounded page: %+v", page)
	}
	page, err = ListTemplatesWithOptions(t.Context(), TemplateListOptions{Limit: 10, Offset: 200})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != len(defs) || len(page.Templates) != 10 {
		t.Fatalf("historical jobs incorrectly inflated list: %+v", page)
	}
}

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

func newTemplateListSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{
		&models.TemplateDefinition{},
		&models.TemplateImageJob{},
		&models.SnapshotRecord{},
		&models.TemplateReplica{},
	} {
		if err := db.AutoMigrate(model); err != nil {
			t.Fatal(err)
		}
	}
	oldDB := store.db
	oldRegistryAuthoritative := cleanupNodeRegistryAuthoritative
	store.db = db
	cleanupNodeRegistryAuthoritative = func() bool { return false }
	t.Cleanup(func() {
		store.db = oldDB
		cleanupNodeRegistryAuthoritative = oldRegistryAuthoritative
	})
	return db
}

func TestListTemplatesWithOptionsUsesDatabasePagination(t *testing.T) {
	db := newTemplateListSQLiteDB(t)
	base := time.Now().UTC().Truncate(time.Second)
	defs := []models.TemplateDefinition{
		{TemplateID: "tpl-b", Status: StatusReady, Model: gorm.Model{CreatedAt: base, UpdatedAt: base.Add(4 * time.Minute)}},
		{TemplateID: "tpl-a", Status: StatusReady, Model: gorm.Model{CreatedAt: base, UpdatedAt: base.Add(4 * time.Minute)}},
		{TemplateID: "pause-hidden", Kind: pausesnap.KindPauseSnapshot, Status: StatusReady, Model: gorm.Model{UpdatedAt: base.Add(6 * time.Minute)}},
		{TemplateID: "snap-deleted", Kind: TemplateKindSnapshot, Status: StatusReady, Model: gorm.Model{UpdatedAt: base.Add(5 * time.Minute)}},
	}
	if err := db.Create(&defs).Error; err != nil {
		t.Fatal(err)
	}
	jobs := []models.TemplateImageJob{
		{TemplateID: "job-only", JobID: "job-old", Operation: JobOperationCreate, AttemptNo: 1, Status: JobStatusFailed, Model: gorm.Model{UpdatedAt: base.Add(time.Minute)}},
		{TemplateID: "job-only", JobID: "job-new", Operation: JobOperationRedo, AttemptNo: 2, Status: JobStatusReady, Model: gorm.Model{UpdatedAt: base.Add(3 * time.Minute)}},
		{TemplateID: "ignored-migrate", JobID: "job-migrate", Operation: JobOperationMigrate, AttemptNo: 1, Status: JobStatusReady, Model: gorm.Model{UpdatedAt: base.Add(10 * time.Minute)}},
	}
	if err := db.Create(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.SnapshotRecord{SnapshotID: "snap-deleted", Status: StatusDeleted}).Error; err != nil {
		t.Fatal(err)
	}

	first, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Total != 3 || len(first.Templates) != 2 {
		t.Fatalf("first page = total %d len %d, want 3/2", first.Total, len(first.Templates))
	}
	if first.Templates[0].TemplateID != "tpl-a" || first.Templates[1].TemplateID != "tpl-b" {
		t.Fatalf("stable first page order = %v, want [tpl-a tpl-b]", []string{first.Templates[0].TemplateID, first.Templates[1].TemplateID})
	}

	second, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if second.Total != 3 || len(second.Templates) != 1 ||
		second.Templates[0].TemplateID != "job-only" ||
		second.Templates[0].JobID != "job-new" {
		t.Fatalf("second page = %+v, want latest job-only entry and total 3", second)
	}

	pastEnd, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{Limit: 2, Offset: 99})
	if err != nil {
		t.Fatalf("past-end page: %v", err)
	}
	if pastEnd.Total != 3 || len(pastEnd.Templates) != 0 {
		t.Fatalf("past-end page = total %d len %d, want 3/0", pastEnd.Total, len(pastEnd.Templates))
	}
}

func TestListTemplatesWithOptionsDefaultsToFinitePage(t *testing.T) {
	db := newTemplateListSQLiteDB(t)
	defs := make([]models.TemplateDefinition, 0, DefaultTemplateListLimit+5)
	for i := 0; i < DefaultTemplateListLimit+5; i++ {
		defs = append(defs, models.TemplateDefinition{
			TemplateID: fmt.Sprintf("tpl-%03d", i),
			Status:     StatusReady,
		})
	}
	if err := db.Create(&defs).Error; err != nil {
		t.Fatal(err)
	}

	got, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{})
	if err != nil {
		t.Fatalf("ListTemplatesWithOptions: %v", err)
	}
	if got.Total != len(defs) || len(got.Templates) != DefaultTemplateListLimit {
		t.Fatalf("default page = total %d len %d, want %d/%d", got.Total, len(got.Templates), len(defs), DefaultTemplateListLimit)
	}
}

func TestListTemplatesBackCompatReturnsAll(t *testing.T) {
	db := newTemplateListSQLiteDB(t)
	defs := make([]models.TemplateDefinition, 0, DefaultTemplateListLimit+5)
	for i := 0; i < DefaultTemplateListLimit+5; i++ {
		defs = append(defs, models.TemplateDefinition{
			TemplateID: fmt.Sprintf("tpl-%03d", i),
			Status:     StatusReady,
		})
	}
	if err := db.Create(&defs).Error; err != nil {
		t.Fatal(err)
	}

	got, err := ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(got) != len(defs) {
		t.Fatalf("ListTemplates returned %d entries, want %d", len(got), len(defs))
	}
}

func TestListTemplatesWithOptionsIgnoresLegacyFullListCache(t *testing.T) {
	newTemplateListSQLiteDB(t)
	templateListCache.Flush()
	defer templateListCache.Flush()

	setTemplateListCache([]TemplateInfo{
		{TemplateID: "tpl-a"},
		{TemplateID: "tpl-b"},
		{TemplateID: "tpl-c"},
	})

	page, err := ListTemplatesWithOptions(context.Background(), TemplateListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListTemplatesWithOptions: %v", err)
	}
	if page.Total != 0 || len(page.Templates) != 0 {
		t.Fatalf("database page unexpectedly came from legacy cache: %+v", page)
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
