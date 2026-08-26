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
	t.Run("NewerBuildTransfersAlias", func(t *testing.T) {
		testNewerBuildTransfersAlias(t, db)
	})
	t.Run("OlderBuildCannotReclaimAlias", func(t *testing.T) {
		testOlderBuildCannotReclaimAlias(t, db)
	})
	t.Run("ConcurrentBuildClaimsConvergeToNewer", func(t *testing.T) {
		testConcurrentBuildClaimsConvergeToNewer(t, db)
	})
	t.Run("UnorderedBuildDoesNotStealAlias", func(t *testing.T) {
		testUnorderedBuildDoesNotStealAlias(t, db)
	})
	t.Run("BuildClaimsAliasFromDeletingHolderWithoutJob", func(t *testing.T) {
		testBuildClaimsAliasFromDeletingHolderWithoutJob(t, db)
	})
	t.Run("BuildClearsDeletingHolderJobAlias", func(t *testing.T) {
		testBuildClearsDeletingHolderJobAlias(t, db)
	})
	t.Run("PublishStatusClaimsTrimmedAlias", func(t *testing.T) {
		testPublishStatusClaimsTrimmedAlias(t, db)
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

func insertCreateJob(t *testing.T, db *gorm.DB, templateID, jobID, alias string) {
	t.Helper()
	insertImageJob(t, db, &models.TemplateImageJob{
		JobID:       jobID,
		TemplateID:  templateID,
		RequestID:   "req-" + jobID,
		Operation:   JobOperationCreate,
		Status:      JobStatusReady,
		RequestJSON: `{"alias":"` + alias + `"}`,
	})
}

func testNewerBuildTransfersAlias(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	oldTemplateID := "tpl-ordered-old-" + suf
	newTemplateID := "tpl-ordered-new-" + suf
	oldJobID := "job-ordered-old-" + suf
	newJobID := "job-ordered-new-" + suf
	alias := "alias-ordered-" + suf
	insertReadyTemplate(t, db, oldTemplateID, alias)
	insertTemplate(t, db, newTemplateID, StatusPending, "")
	insertCreateJob(t, db, oldTemplateID, oldJobID, alias)
	insertImageJob(t, db, &models.TemplateImageJob{
		JobID:       newJobID,
		TemplateID:  newTemplateID,
		RequestID:   "req-" + newJobID,
		Operation:   JobOperationCreate,
		Status:      JobStatusRunning,
		RequestJSON: `{"alias":"` + alias + `"}`,
	})
	cleanupTemplatesAndJobs(t, db, []string{oldTemplateID, newTemplateID}, []string{oldJobID, newJobID})

	displayName, warning, err := publishTemplateStatusWithAlias(
		context.Background(), newTemplateID, newJobID, StatusReady, "",
	)
	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Equal(t, alias, displayName)

	holder, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err)
	assert.Equal(t, newTemplateID, holder.TemplateID)
	oldDef, err := GetDefinition(context.Background(), oldTemplateID)
	require.NoError(t, err)
	assert.Empty(t, oldDef.DisplayName)
	newDef, err := GetDefinition(context.Background(), newTemplateID)
	require.NoError(t, err)
	assert.Equal(t, StatusReady, newDef.Status)
	assert.Equal(t, alias, newDef.DisplayName)
	var oldJob models.TemplateImageJob
	require.NoError(t, db.Where("job_id = ?", oldJobID).First(&oldJob).Error)
	assert.Empty(t, aliasFromRequestJSON(oldJob.RequestJSON))
}

func testOlderBuildCannotReclaimAlias(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	oldTemplateID := "tpl-late-old-" + suf
	newTemplateID := "tpl-late-new-" + suf
	oldJobID := "job-late-old-" + suf
	newJobID := "job-late-new-" + suf
	alias := "alias-late-" + suf
	insertReadyTemplate(t, db, oldTemplateID, "")
	insertCreateJob(t, db, oldTemplateID, oldJobID, alias)
	insertReadyTemplate(t, db, newTemplateID, alias)
	insertCreateJob(t, db, newTemplateID, newJobID, alias)
	cleanupTemplatesAndJobs(t, db, []string{oldTemplateID, newTemplateID}, []string{oldJobID, newJobID})

	displayName, warning, err := publishTemplateStatusWithAlias(
		context.Background(), oldTemplateID, oldJobID, StatusReady, "",
	)
	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Empty(t, displayName)

	holder, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err)
	assert.Equal(t, newTemplateID, holder.TemplateID)
}

func testConcurrentBuildClaimsConvergeToNewer(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	oldTemplateID := "tpl-concurrent-old-" + suf
	newTemplateID := "tpl-concurrent-new-" + suf
	oldJobID := "job-concurrent-old-" + suf
	newJobID := "job-concurrent-new-" + suf
	alias := "alias-concurrent-ordered-" + suf
	insertReadyTemplate(t, db, oldTemplateID, "")
	insertReadyTemplate(t, db, newTemplateID, "")
	insertCreateJob(t, db, oldTemplateID, oldJobID, alias)
	insertCreateJob(t, db, newTemplateID, newJobID, alias)
	cleanupTemplatesAndJobs(t, db, []string{oldTemplateID, newTemplateID}, []string{oldJobID, newJobID})

	errCh := make(chan error, 2)
	go func() {
		_, _, err := publishTemplateStatusWithAlias(context.Background(), oldTemplateID, oldJobID, StatusReady, "")
		errCh <- err
	}()
	go func() {
		_, _, err := publishTemplateStatusWithAlias(context.Background(), newTemplateID, newJobID, StatusReady, "")
		errCh <- err
	}()
	require.NoError(t, <-errCh)
	require.NoError(t, <-errCh)

	holder, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err)
	assert.Equal(t, newTemplateID, holder.TemplateID)
}

func testUnorderedBuildDoesNotStealAlias(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	holderID := "tpl-unordered-holder-" + suf
	claimantID := "tpl-unordered-claimant-" + suf
	claimantJobID := "job-unordered-claimant-" + suf
	alias := "alias-unordered-" + suf
	insertReadyTemplate(t, db, holderID, alias)
	insertTemplate(t, db, claimantID, StatusPending, "")
	insertCreateJob(t, db, claimantID, claimantJobID, alias)
	cleanupTemplatesAndJobs(t, db, []string{holderID, claimantID}, []string{claimantJobID})

	displayName, warning, err := publishTemplateStatusWithAlias(
		context.Background(), claimantID, claimantJobID, StatusReady, "",
	)
	require.NoError(t, err)
	assert.Contains(t, warning, "has no CREATE/REDO job metadata")
	assert.Empty(t, displayName)

	holder, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err)
	assert.Equal(t, holderID, holder.TemplateID)
}

func testBuildClaimsAliasFromDeletingHolderWithoutJob(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	holderID := "tpl-deleting-holder-" + suf
	claimantID := "tpl-deleting-claimant-" + suf
	claimantJobID := "job-deleting-claimant-" + suf
	alias := "alias-deleting-holder-" + suf
	insertTemplate(t, db, holderID, StatusDeleting, alias)
	insertTemplate(t, db, claimantID, StatusPending, "")
	insertCreateJob(t, db, claimantID, claimantJobID, alias)
	cleanupTemplatesAndJobs(t, db, []string{holderID, claimantID}, []string{claimantJobID})

	displayName, warning, err := publishTemplateStatusWithAlias(
		context.Background(), claimantID, claimantJobID, StatusReady, "",
	)
	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Equal(t, alias, displayName)

	holder, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err)
	assert.Equal(t, claimantID, holder.TemplateID)
	deletingDef, err := GetDefinition(context.Background(), holderID)
	require.NoError(t, err)
	assert.Empty(t, deletingDef.DisplayName)
}

func testBuildClearsDeletingHolderJobAlias(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	holderID := "tpl-deleting-job-holder-" + suf
	claimantID := "tpl-deleting-job-claimant-" + suf
	holderJobID := "job-deleting-holder-" + suf
	claimantJobID := "job-deleting-job-claimant-" + suf
	alias := "alias-deleting-job-holder-" + suf
	insertTemplate(t, db, holderID, StatusDeleting, alias)
	insertTemplate(t, db, claimantID, StatusPending, "")
	insertCreateJob(t, db, holderID, holderJobID, alias)
	insertCreateJob(t, db, claimantID, claimantJobID, alias)
	cleanupTemplatesAndJobs(t, db, []string{holderID, claimantID}, []string{holderJobID, claimantJobID})

	displayName, warning, err := publishTemplateStatusWithAlias(
		context.Background(), claimantID, claimantJobID, StatusReady, "",
	)
	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Equal(t, alias, displayName)

	var holderJob models.TemplateImageJob
	require.NoError(t, db.Where("job_id = ?", holderJobID).First(&holderJob).Error)
	assert.Empty(t, aliasFromRequestJSON(holderJob.RequestJSON))
}

func testPublishStatusClaimsTrimmedAlias(t *testing.T, db *gorm.DB) {
	suf := aliasCaseSuffix()
	templateID := "tpl-publish-" + suf
	jobID := "job-publish-" + suf
	alias := "alias-publish-" + suf
	insertTemplate(t, db, templateID, StatusPending, "")
	insertImageJob(t, db, &models.TemplateImageJob{
		JobID:       jobID,
		TemplateID:  templateID,
		RequestID:   "req-" + jobID,
		Operation:   JobOperationCreate,
		Status:      JobStatusRunning,
		RequestJSON: `{"alias":"  ` + alias + `  "}`,
	})
	cleanupTemplatesAndJobs(t, db, []string{templateID}, []string{jobID})

	displayName, warning, err := publishTemplateStatusWithAlias(
		context.Background(), templateID, jobID, StatusReady, "",
	)
	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Equal(t, alias, displayName)

	def, err := GetDefinition(context.Background(), templateID)
	require.NoError(t, err)
	assert.Equal(t, StatusReady, def.Status)
	assert.Equal(t, alias, def.DisplayName)

	holder, err := GetTemplateByAlias(context.Background(), alias)
	require.NoError(t, err)
	assert.Equal(t, templateID, holder.TemplateID)
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

	err := SetTemplateAlias(context.Background(), tplID, alias)
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
