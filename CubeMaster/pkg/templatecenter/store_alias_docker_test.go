// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func TestAliasStoreMySQL(t *testing.T) {
	env := newMySQLDockerEnv(t)
	defer env.teardown()
	gormDB := openMigratedMySQLGORM(t, env)
	oldDB := store.db
	store.db = gormDB
	defer func() { store.db = oldDB }()
	runAliasStoreCases(t, gormDB)
}

func TestAliasStorePostgreSQL(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()
	gormDB := openMigratedPostgresGORM(t, env)
	oldDB := store.db
	store.db = gormDB
	defer func() { store.db = oldDB }()
	runAliasStoreCases(t, gormDB)
}

func runAliasStoreCases(t *testing.T, db *gorm.DB) {
	t.Helper()
	t.Run("GetByAliasExcludesSnapshots", func(t *testing.T) {
		testGetByAliasExcludesSnapshots(t, db)
	})
	t.Run("ConcurrentCreateRedoIsMutex", func(t *testing.T) {
		testConcurrentCreateRedoIsMutex(t, db)
	})
	t.Run("CreateRedoDoesNotStealReadyHolder", func(t *testing.T) {
		testCreateRedoDoesNotStealReadyHolder(t, db)
	})
	t.Run("IdempotentReclaimOwnAlias", func(t *testing.T) {
		testIdempotentReclaimOwnAlias(t, db)
	})
	t.Run("FailedHolderOccupiesAliasKey", func(t *testing.T) {
		testFailedHolderOccupiesAliasKey(t, db)
	})
	t.Run("SetAliasSyncsCreateRedoLeavesOthers", func(t *testing.T) {
		testSetAliasSyncsCreateRedoLeavesOthers(t, db)
	})
	t.Run("SetAliasSyncFailureRollsBackDisplayName", func(t *testing.T) {
		testSetAliasSyncFailureRollsBackDisplayName(t, db)
	})
	t.Run("OperatorClaimRejectsNonReadyTarget", func(t *testing.T) {
		testOperatorClaimRejectsNonReadyTarget(t, db)
	})
	t.Run("SetTemplateAliasIdempotentReclaim", func(t *testing.T) {
		testSetTemplateAliasIdempotentReclaim(t, db)
	})
	t.Run("ClearAliasSyncsCreateRedoLeavesCommit", func(t *testing.T) {
		testClearAliasSyncsCreateRedoLeavesCommit(t, db)
	})
}

func aliasCaseSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func insertTemplate(t *testing.T, db *gorm.DB, templateID, status, displayName string) {
	t.Helper()
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:  templateID,
		Kind:        TemplateKindTemplate,
		Status:      status,
		DisplayName: displayName,
		RequestJSON: "{}",
	}).Error)
}

func insertReadyTemplate(t *testing.T, db *gorm.DB, templateID, displayName string) {
	t.Helper()
	insertTemplate(t, db, templateID, StatusReady, displayName)
}

func insertImageJob(t *testing.T, db *gorm.DB, job *models.TemplateImageJob) {
	t.Helper()
	require.NoError(t, db.Create(job).Error)
}

func cleanupTemplatesAndJobs(t *testing.T, db *gorm.DB, templateIDs, jobIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		if len(jobIDs) > 0 {
			db.Unscoped().Where("job_id IN ?", jobIDs).Delete(&models.TemplateImageJob{})
		}
		if len(templateIDs) > 0 {
			db.Unscoped().Where("template_id IN ?", templateIDs).Delete(&models.TemplateDefinition{})
		}
	})
}

func testGetByAliasExcludesSnapshots(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplID := "tpl-test-" + suf
	snapID := "snap-test-" + suf
	tplAlias := "alias-tpl-" + suf
	snapAlias := "alias-snap-" + suf

	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:  tplID,
		Kind:        TemplateKindTemplate,
		DisplayName: tplAlias,
		Status:      StatusReady,
		RequestJSON: "{}",
	}).Error)
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:  snapID,
		Kind:        TemplateKindSnapshot,
		DisplayName: snapAlias,
		Status:      StatusReady,
		RequestJSON: "{}",
	}).Error)
	cleanupTemplatesAndJobs(t, db, []string{tplID, snapID}, nil)

	got, err := GetTemplateByAlias(context.Background(), tplAlias)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tplID, got.TemplateID)
	assert.Equal(t, TemplateKindTemplate, got.Kind)

	_, err = GetTemplateByAlias(context.Background(), snapAlias)
	assert.True(t, errors.Is(err, ErrTemplateNotFound),
		"a snapshot's alias must NOT resolve; got err=%v", err)
}

func testConcurrentCreateRedoIsMutex(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplA := "tpl-conc-a-" + suf
	tplB := "tpl-conc-b-" + suf
	alias := "alias-conc-" + suf
	insertReadyTemplate(t, db, tplA, "")
	insertReadyTemplate(t, db, tplB, "")
	cleanupTemplatesAndJobs(t, db, []string{tplA, tplB}, nil)

	type result struct{ err error }
	resCh := make(chan result, 2)
	for _, id := range []string{tplA, tplB} {
		go func(templateID string) {
			resCh <- result{err: claimTemplateAlias(context.Background(), templateID, alias, false)}
		}(id)
	}
	r1, r2 := <-resCh, <-resCh

	successes, duplicates := 0, 0
	for _, r := range []result{r1, r2} {
		if r.err == nil {
			successes++
			continue
		}
		assert.True(t, isDuplicateAliasError(r.err),
			"if a claim fails it must be a duplicate-key error; got: %v", r.err)
		duplicates++
	}
	assert.Equal(t, 1, successes, "exactly one claim must succeed")
	assert.Equal(t, 1, duplicates, "the loser must see a duplicate-key error")

	got, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err)
	assert.True(t, got.TemplateID == tplA || got.TemplateID == tplB,
		"alias must resolve to one of the two templates; got %s", got.TemplateID)

	otherID := tplA
	if got.TemplateID == tplA {
		otherID = tplB
	}
	otherDef, err := GetDefinition(context.Background(), otherID)
	require.NoError(t, err)
	assert.Empty(t, otherDef.DisplayName,
		"the non-owning template's display_name must be empty; got %q", otherDef.DisplayName)
}

func testCreateRedoDoesNotStealReadyHolder(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplReady := "tpl-ready-" + suf
	tplNew := "tpl-new-" + suf
	alias := "alias-steal-" + suf
	insertReadyTemplate(t, db, tplReady, alias)
	insertReadyTemplate(t, db, tplNew, "")
	cleanupTemplatesAndJobs(t, db, []string{tplReady, tplNew}, nil)

	err := claimTemplateAlias(context.Background(), tplNew, alias, false)
	require.Error(t, err)
	assert.True(t, isDuplicateAliasError(err), "create/redo collision must be duplicate-key; got %v", err)

	readyDef, err := GetDefinition(context.Background(), tplReady)
	require.NoError(t, err)
	assert.Equal(t, alias, readyDef.DisplayName,
		"READY holder display_name must never be cleared by create/redo claim")

	newDef, err := GetDefinition(context.Background(), tplNew)
	require.NoError(t, err)
	assert.Empty(t, newDef.DisplayName, "losing create/redo claimant must stay without alias")

	require.NoError(t, claimTemplateAlias(context.Background(), tplNew, alias, true),
		"operator path must still be able to transfer the alias")
	readyDef, err = GetDefinition(context.Background(), tplReady)
	require.NoError(t, err)
	assert.Empty(t, readyDef.DisplayName)
	newDef, err = GetDefinition(context.Background(), tplNew)
	require.NoError(t, err)
	assert.Equal(t, alias, newDef.DisplayName)
}

func testIdempotentReclaimOwnAlias(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplID := "tpl-reclaim-" + suf
	alias := "alias-reclaim-" + suf
	insertReadyTemplate(t, db, tplID, alias)
	cleanupTemplatesAndJobs(t, db, []string{tplID}, nil)

	require.NoError(t, claimTemplateAlias(context.Background(), tplID, alias, false))
	def, err := GetDefinition(context.Background(), tplID)
	require.NoError(t, err)
	assert.Equal(t, alias, def.DisplayName)
}

func testFailedHolderOccupiesAliasKey(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplFailed := "tpl-failed-" + suf
	tplReady := "tpl-ready-" + suf
	alias := "alias-failed-" + suf
	insertTemplate(t, db, tplFailed, StatusFailed, alias)
	insertReadyTemplate(t, db, tplReady, "")
	cleanupTemplatesAndJobs(t, db, []string{tplFailed, tplReady}, nil)

	err := claimTemplateAlias(context.Background(), tplReady, alias, false)
	require.Error(t, err)
	assert.True(t, isDuplicateAliasError(err), "FAILED holder must occupy alias_key; got %v", err)

	readyDef, err := GetDefinition(context.Background(), tplReady)
	require.NoError(t, err)
	assert.Empty(t, readyDef.DisplayName, "READY challenger must stay without alias")

	failedDef, err := GetDefinition(context.Background(), tplFailed)
	require.NoError(t, err)
	assert.Equal(t, alias, failedDef.DisplayName, "FAILED holder must keep the alias")

	require.NoError(t, claimTemplateAlias(context.Background(), tplReady, alias, true),
		"operator path must still be able to transfer the alias off a FAILED holder")
	failedDef, err = GetDefinition(context.Background(), tplFailed)
	require.NoError(t, err)
	assert.Empty(t, failedDef.DisplayName)
	readyDef, err = GetDefinition(context.Background(), tplReady)
	require.NoError(t, err)
	assert.Equal(t, alias, readyDef.DisplayName)
}

func testSetAliasSyncsCreateRedoLeavesOthers(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplID := "tpl-json-" + suf
	alias := "alias-json-" + suf
	insertReadyTemplate(t, db, tplID, "")

	commitJSON := `{"request_id":"req-` + suf + `","cpu":"1"}`
	legacyJSON := `{"alias":"old","legacy":true}`
	snapJSON := `{"alias":"old","snapshot":true}`
	createJSON := `{"alias":"old","source_image_ref":"img"}`
	redoJSON := `{"alias":"old","source_image_ref":"img","redo":true}`

	jobs := []*models.TemplateImageJob{
		{JobID: "job-commit-" + suf, TemplateID: tplID, RequestID: "req-commit-" + suf, Operation: JobOperationCommit, Status: JobStatusReady, RequestJSON: commitJSON},
		{JobID: "job-legacy-" + suf, TemplateID: tplID, RequestID: "req-legacy-" + suf, Operation: JobOperationLegacy, Status: JobStatusReady, RequestJSON: legacyJSON},
		{JobID: "job-snap-" + suf, TemplateID: tplID, RequestID: "req-snap-" + suf, Operation: JobOperationSnapshotCreate, Status: JobStatusReady, RequestJSON: snapJSON},
		{JobID: "job-create-" + suf, TemplateID: tplID, RequestID: "req-create-" + suf, Operation: JobOperationCreate, Status: JobStatusReady, RequestJSON: createJSON},
		{JobID: "job-redo-" + suf, TemplateID: tplID, RequestID: "req-redo-" + suf, Operation: JobOperationRedo, Status: JobStatusReady, RequestJSON: redoJSON},
	}
	jobIDs := make([]string, 0, len(jobs))
	for i, job := range jobs {
		job.AttemptNo = int32(i + 1)
		insertImageJob(t, db, job)
		jobIDs = append(jobIDs, job.JobID)
	}
	cleanupTemplatesAndJobs(t, db, []string{tplID}, jobIDs)

	require.NoError(t, SetTemplateAlias(context.Background(), tplID, alias))

	loadJob := func(id string) models.TemplateImageJob {
		t.Helper()
		var job models.TemplateImageJob
		require.NoError(t, db.Where("job_id = ?", id).First(&job).Error)
		return job
	}
	assert.Equal(t, commitJSON, loadJob("job-commit-"+suf).RequestJSON, "COMMIT RequestJSON must be byte-identical")
	assert.Equal(t, legacyJSON, loadJob("job-legacy-"+suf).RequestJSON, "LEGACY RequestJSON must be byte-identical")
	assert.Equal(t, snapJSON, loadJob("job-snap-"+suf).RequestJSON, "SNAPSHOT_CREATE RequestJSON must be byte-identical")
	assert.Contains(t, loadJob("job-create-"+suf).RequestJSON, `"alias":"`+alias+`"`)
	assert.Contains(t, loadJob("job-redo-"+suf).RequestJSON, `"alias":"`+alias+`"`)

	def, err := GetDefinition(context.Background(), tplID)
	require.NoError(t, err)
	assert.Equal(t, alias, def.DisplayName)
}

func testSetAliasSyncFailureRollsBackDisplayName(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplID := "tpl-rollback-" + suf
	alias := "alias-rollback-" + suf
	jobID := "job-badjson-" + suf
	insertReadyTemplate(t, db, tplID, "")
	insertImageJob(t, db, &models.TemplateImageJob{
		JobID:       jobID,
		TemplateID:  tplID,
		RequestID:   "req-badjson-" + suf,
		Operation:   JobOperationCreate,
		Status:      JobStatusReady,
		RequestJSON: "not-json",
	})
	cleanupTemplatesAndJobs(t, db, []string{tplID}, []string{jobID})

	err := SetTemplateAlias(context.Background(), tplID, alias)
	require.Error(t, err)

	def, err := GetDefinition(context.Background(), tplID)
	require.NoError(t, err)
	assert.Empty(t, def.DisplayName, "display_name must roll back when CREATE/REDO JSON sync fails")

	var job models.TemplateImageJob
	require.NoError(t, db.Where("job_id = ?", jobID).First(&job).Error)
	assert.Equal(t, "not-json", job.RequestJSON)
}

func testOperatorClaimRejectsNonReadyTarget(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplID := "tpl-pending-" + suf
	alias := "alias-pending-" + suf
	insertTemplate(t, db, tplID, StatusPending, "")
	cleanupTemplatesAndJobs(t, db, []string{tplID}, nil)

	err := claimTemplateAlias(context.Background(), tplID, alias, true)
	require.ErrorIs(t, err, ErrTemplateNotReady)

	def, err := GetDefinition(context.Background(), tplID)
	require.NoError(t, err)
	assert.Empty(t, def.DisplayName, "non-READY operator claim must not write display_name")
}

func testSetTemplateAliasIdempotentReclaim(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplID := "tpl-set-reclaim-" + suf
	alias := "alias-set-reclaim-" + suf
	insertReadyTemplate(t, db, tplID, alias)
	cleanupTemplatesAndJobs(t, db, []string{tplID}, nil)

	require.NoError(t, SetTemplateAlias(context.Background(), tplID, alias))
	def, err := GetDefinition(context.Background(), tplID)
	require.NoError(t, err)
	assert.Equal(t, alias, def.DisplayName)
}

func testClearAliasSyncsCreateRedoLeavesCommit(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	tplID := "tpl-clear-" + suf
	alias := "alias-clear-" + suf
	insertReadyTemplate(t, db, tplID, alias)

	commitJSON := `{"request_id":"req-clear-` + suf + `","cpu":"1"}`
	createJSON := `{"alias":"` + alias + `","source_image_ref":"img"}`
	redoJSON := `{"alias":"` + alias + `","redo":true}`
	createID := "job-clear-create-" + suf
	redoID := "job-clear-redo-" + suf
	commitID := "job-clear-commit-" + suf
	insertImageJob(t, db, &models.TemplateImageJob{
		JobID: createID, TemplateID: tplID, RequestID: "req-clear-create-" + suf,
		Operation: JobOperationCreate, Status: JobStatusReady, RequestJSON: createJSON, AttemptNo: 1,
	})
	insertImageJob(t, db, &models.TemplateImageJob{
		JobID: redoID, TemplateID: tplID, RequestID: "req-clear-redo-" + suf,
		Operation: JobOperationRedo, Status: JobStatusReady, RequestJSON: redoJSON, AttemptNo: 2,
	})
	insertImageJob(t, db, &models.TemplateImageJob{
		JobID: commitID, TemplateID: tplID, RequestID: "req-clear-commit-" + suf,
		Operation: JobOperationCommit, Status: JobStatusReady, RequestJSON: commitJSON, AttemptNo: 3,
	})
	cleanupTemplatesAndJobs(t, db, []string{tplID}, []string{createID, redoID, commitID})

	require.NoError(t, SetTemplateAlias(context.Background(), tplID, ""))

	def, err := GetDefinition(context.Background(), tplID)
	require.NoError(t, err)
	assert.Empty(t, def.DisplayName)

	loadJob := func(id string) models.TemplateImageJob {
		t.Helper()
		var job models.TemplateImageJob
		require.NoError(t, db.Where("job_id = ?", id).First(&job).Error)
		return job
	}
	assert.NotContains(t, loadJob(createID).RequestJSON, `"alias"`)
	assert.NotContains(t, loadJob(redoID).RequestJSON, `"alias"`)
	assert.Equal(t, commitJSON, loadJob(commitID).RequestJSON, "COMMIT RequestJSON must be byte-identical after clear")
}
