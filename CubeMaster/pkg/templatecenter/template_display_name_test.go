// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func TestNormalizeTemplateDisplayName(t *testing.T) {
	t.Parallel()

	got, err := NormalizeTemplateDisplayName("  cubesandbox-template  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "cubesandbox-template" {
		t.Fatalf("got %q", got)
	}

	if _, err := NormalizeTemplateDisplayName(""); err != nil {
		t.Fatalf("empty name should be allowed: %v", err)
	}

	if _, err := NormalizeTemplateDisplayName("tpl-custom"); !errors.Is(err, ErrTemplateNameInvalid) {
		t.Fatalf("expected ErrTemplateNameInvalid for tpl- prefix, got %v", err)
	}
	if _, err := NormalizeTemplateDisplayName("SNAP-backup"); !errors.Is(err, ErrTemplateNameInvalid) {
		t.Fatalf("expected ErrTemplateNameInvalid for snap- prefix, got %v", err)
	}

	longName := strings.Repeat("a", maxTemplateDisplayNameLen+1)
	if _, err := NormalizeTemplateDisplayName(longName); !errors.Is(err, ErrTemplateNameInvalid) {
		t.Fatalf("expected length validation error, got %v", err)
	}

	if _, err := NormalizeTemplateDisplayName("<script>"); !errors.Is(err, ErrTemplateNameInvalid) {
		t.Fatalf("expected ErrTemplateNameInvalid for HTML metacharacters, got %v", err)
	}
	if _, err := NormalizeTemplateDisplayName("a\nb"); !errors.Is(err, ErrTemplateNameInvalid) {
		t.Fatalf("expected ErrTemplateNameInvalid for newline, got %v", err)
	}
	if _, err := NormalizeTemplateDisplayName("-leading-hyphen"); !errors.Is(err, ErrTemplateNameInvalid) {
		t.Fatalf("expected ErrTemplateNameInvalid for leading hyphen, got %v", err)
	}
}

func TestMapDefinitionCreateDuplicateError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dupName := errors.New("Error 1062 (23000): Duplicate entry 'my-env' for key 'idx_template_display_name_key'")
	if !errors.Is(mapDefinitionCreateDuplicateError(ctx, dupName, "my-env"), ErrTemplateNameInUse) {
		t.Fatalf("expected ErrTemplateNameInUse")
	}

	dupTemplate := errors.New("Error 1062 (23000): Duplicate entry 'tpl-1' for key 'idx_template_id'")
	if !errors.Is(mapDefinitionCreateDuplicateError(ctx, dupTemplate, "my-env"), ErrDuplicateTemplate) {
		t.Fatalf("expected ErrDuplicateTemplate")
	}
	if !errors.Is(mapDefinitionCreateDuplicateError(ctx, gorm.ErrDuplicatedKey, "my-env"), ErrDuplicateTemplate) {
		t.Fatalf("expected ErrDuplicateTemplate for gorm.ErrDuplicatedKey")
	}

	dbErr := errors.New("connection reset by peer")
	mapped := mapDefinitionCreateDuplicateError(ctx, dbErr, "my-env")
	if mapped.Error() != "internal error" {
		t.Fatalf("expected generic internal error, got %v", mapped)
	}
}

func TestTemplateDisplayNameLookupUsesTemplateKind(t *testing.T) {
	t.Parallel()
	if TemplateKindTemplate != "template" {
		t.Fatalf("unexpected template kind constant %q", TemplateKindTemplate)
	}
}

func TestDefinitionSupportsTemplateDisplayName(t *testing.T) {
	t.Parallel()
	if !definitionSupportsTemplateDisplayName(TemplateKindTemplate) {
		t.Fatal("template kind should be supported")
	}
	if definitionSupportsTemplateDisplayName(TemplateKindSnapshot) {
		t.Fatal("snapshot kind should not support template display name updates")
	}
	// Empty kind must NOT be nameable: it stays consistent with the
	// display_name_key generated column (NULL unless kind = 'template') and the
	// kind = 'template' filter in FindTemplateIDByDisplayName.
	if definitionSupportsTemplateDisplayName("") {
		t.Fatal("empty kind should not support template display name updates")
	}
}

func TestNormalizeRequiredTemplateDisplayName(t *testing.T) {
	t.Parallel()
	got, err := normalizeRequiredTemplateDisplayName(" cubesandbox-template ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "cubesandbox-template" {
		t.Fatalf("got %q", got)
	}
	if _, err := normalizeRequiredTemplateDisplayName("  "); !errors.Is(err, ErrTemplateNameInvalid) {
		t.Fatalf("expected ErrTemplateNameInvalid, got %v", err)
	}
}

func TestDisplayNameFromTemplateImageJob(t *testing.T) {
	t.Parallel()

	if got := displayNameFromTemplateImageJob(context.Background(), nil); got != "" {
		t.Fatalf("nil job should return empty string, got %q", got)
	}

	job := &models.TemplateImageJob{
		RequestJSON: `{"display_name":" cubesandbox-template "}`,
	}
	if got := displayNameFromTemplateImageJob(context.Background(), job); got != "cubesandbox-template" {
		t.Fatalf("got %q", got)
	}

	invalid := &models.TemplateImageJob{
		RequestJSON: `{"display_name":"tpl-reserved"}`,
	}
	if got := displayNameFromTemplateImageJob(context.Background(), invalid); got != "" {
		t.Fatalf("invalid reserved prefix should be omitted, got %q", got)
	}
}

func TestDisplayNameLockKeyIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	if got := displayNameLockKey("My-Env"); got != "display-name:my-env" {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayNameFromDefinition(t *testing.T) {
	t.Parallel()
	if got := displayNameFromDefinition(nil); got != "" {
		t.Fatalf("nil definition should return empty string, got %q", got)
	}
	def := &models.TemplateDefinition{DisplayName: "my-env"}
	if got := displayNameFromDefinition(def); got != "my-env" {
		t.Fatalf("got %q", got)
	}
}

func TestFindTemplateIDByDisplayNameUsesCache(t *testing.T) {
	templateDisplayNameCache.Flush()
	templateDisplayNameNotFoundCache.Flush()
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = origDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	dbCalls := 0
	patches.ApplyFunc(findTemplateIDByDisplayNameFromDB, func(context.Context, string) (string, error) {
		dbCalls++
		return "tpl-cached", nil
	})

	got, err := FindTemplateIDByDisplayName(context.Background(), "My-Env")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "tpl-cached" {
		t.Fatalf("got %q", got)
	}
	if dbCalls != 1 {
		t.Fatalf("expected one DB lookup, got %d", dbCalls)
	}

	gotAgain, err := FindTemplateIDByDisplayName(context.Background(), "my-env")
	if err != nil {
		t.Fatalf("unexpected error on cache hit: %v", err)
	}
	if gotAgain != "tpl-cached" {
		t.Fatalf("got %q", gotAgain)
	}
	if dbCalls != 1 {
		t.Fatalf("expected cache hit without second DB lookup, got %d calls", dbCalls)
	}
}

func TestReserveTemplateDisplayNameForCreateAllowsFreeName(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(withTemplateWriteLock, func(_ string, fn func() error) error {
		return fn()
	})
	patches.ApplyFunc(findDisplayNameOwnersFromDB, func(context.Context, string) ([]displayNameOwner, error) {
		return nil, nil
	})
	if err := ReserveTemplateDisplayNameForCreate(context.Background(), "my-env", "tpl-new"); err != nil {
		t.Fatalf("expected free name, got %v", err)
	}
}

func TestReserveTemplateDisplayNameForCreateRejectsActiveName(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(withTemplateWriteLock, func(_ string, fn func() error) error {
		return fn()
	})
	patches.ApplyFunc(findDisplayNameOwnersFromDB, func(context.Context, string) ([]displayNameOwner, error) {
		return []displayNameOwner{{TemplateID: "tpl-old", Status: StatusReady}}, nil
	})
	err := ReserveTemplateDisplayNameForCreate(context.Background(), "my-env", "tpl-new")
	if !errors.Is(err, ErrTemplateNameInUse) {
		t.Fatalf("expected ErrTemplateNameInUse, got %v", err)
	}
}

func TestReserveTemplateDisplayNameForCreateReclaimsFailedName(t *testing.T) {
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = origDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(withTemplateWriteLock, func(_ string, fn func() error) error {
		return fn()
	})
	patches.ApplyFunc(findDisplayNameOwnersFromDB, func(context.Context, string) ([]displayNameOwner, error) {
		return []displayNameOwner{{TemplateID: "tpl-old", Status: StatusFailed}}, nil
	})
	patches.ApplyFunc(GetDefinition, func(_ context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID:  templateID,
			DisplayName: "my-env",
			Status:      StatusFailed,
		}, nil
	})
	cleared := false
	patches.ApplyFunc(clearDefinitionDisplayNameLocked, func(_ context.Context, templateID string, _ *models.TemplateDefinition) error {
		if templateID != "tpl-old" {
			t.Fatalf("templateID = %q, want tpl-old", templateID)
		}
		cleared = true
		return nil
	})
	if err := ReserveTemplateDisplayNameForCreate(context.Background(), "my-env", "tpl-new"); err != nil {
		t.Fatalf("expected reclaim, got %v", err)
	}
	if !cleared {
		t.Fatal("expected failed template display name to be cleared")
	}
}

func TestWithDisplayNameCreateLockReclaimsFailedWithoutDeadlock(t *testing.T) {
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = origDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(findDisplayNameOwnersFromDB, func(context.Context, string) ([]displayNameOwner, error) {
		return []displayNameOwner{{TemplateID: "tpl-old", Status: StatusFailed}}, nil
	})
	patches.ApplyFunc(GetDefinition, func(_ context.Context, templateID string) (*models.TemplateDefinition, error) {
		if templateID != "tpl-old" {
			t.Fatalf("templateID = %q, want tpl-old", templateID)
		}
		return &models.TemplateDefinition{
			TemplateID:  "tpl-old",
			DisplayName: "my-env",
			Status:      StatusFailed,
		}, nil
	})
	cleared := false
	patches.ApplyFunc(clearDefinitionDisplayNameLocked, func(_ context.Context, templateID string, _ *models.TemplateDefinition) error {
		if templateID != "tpl-old" {
			t.Fatalf("templateID = %q, want tpl-old", templateID)
		}
		cleared = true
		return nil
	})

	done := make(chan error, 1)
	go func() {
		done <- withDisplayNameCreateLock(context.Background(), "my-env", "tpl-new", func() error { return nil })
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected reclaim without error, got %v", err)
		}
		if !cleared {
			t.Fatal("expected failed template display name to be cleared")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: withDisplayNameCreateLock did not complete within timeout")
	}
}

func TestFindTemplateIDByDisplayNameReturnsAmbiguous(t *testing.T) {
	templateDisplayNameCache.Flush()
	templateDisplayNameNotFoundCache.Flush()
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = origDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(findTemplateIDByDisplayNameFromDB, func(context.Context, string) (string, error) {
		return "", ErrTemplateNameAmbiguous
	})

	_, err := FindTemplateIDByDisplayName(context.Background(), "dup-name")
	if !errors.Is(err, ErrTemplateNameAmbiguous) {
		t.Fatalf("expected ErrTemplateNameAmbiguous, got %v", err)
	}
}

func TestUpdateDefinitionDisplayNameInvalidatesRenameCache(t *testing.T) {
	templateDisplayNameCache.Flush()
	templateDisplayNameNotFoundCache.Flush()
	setTemplateDisplayNameCache("old-name", "tpl-1")
	invalidateTemplateDisplayNameCache("old-name", "new-name")
	setTemplateDisplayNameCache("new-name", "tpl-1")
	if _, ok := getCachedTemplateIDByDisplayName("old-name"); ok {
		t.Fatal("old name should not be cached")
	}
	got, ok := getCachedTemplateIDByDisplayName("new-name")
	if !ok || got != "tpl-1" {
		t.Fatalf("new name cache = (%q, %v)", got, ok)
	}
}

func TestClearDefinitionDisplayNameReturnsGetDefinitionError(t *testing.T) {
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = origDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	dbErr := errors.New("db unavailable")
	patches.ApplyFunc(GetDefinition, func(context.Context, string) (*models.TemplateDefinition, error) {
		return nil, dbErr
	})
	called := false
	patches.ApplyFunc(clearDefinitionDisplayNameLocked, func(context.Context, string, *models.TemplateDefinition) error {
		called = true
		return nil
	})

	err := clearDefinitionDisplayName(context.Background(), "tpl-1")
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected db error, got %v", err)
	}
	if called {
		t.Fatal("should not update when GetDefinition fails unexpectedly")
	}
}

func TestReleaseTemplateDisplayNameAfterBuildFailureClearsName(t *testing.T) {
	called := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(_ context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{TemplateID: templateID, DisplayName: "my-env"}, nil
	})
	patches.ApplyFunc(clearDefinitionDisplayName, func(_ context.Context, templateID string) error {
		if templateID != "tpl-failed" {
			t.Fatalf("templateID = %q", templateID)
		}
		called = true
		return nil
	})

	ReleaseTemplateDisplayNameAfterBuildFailure(context.Background(), "tpl-failed")
	if !called {
		t.Fatal("expected clearDefinitionDisplayName to run")
	}
}

func TestReleaseTemplateDisplayNameAfterBuildFailureSkipsWhenNoDisplayName(t *testing.T) {
	called := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(_ context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{TemplateID: templateID, DisplayName: ""}, nil
	})
	patches.ApplyFunc(clearDefinitionDisplayName, func(context.Context, string) error {
		called = true
		return nil
	})

	ReleaseTemplateDisplayNameAfterBuildFailure(context.Background(), "tpl-no-name")
	if called {
		t.Fatal("expected no DB write when display name is empty")
	}
}

func TestReleaseTemplateDisplayNameAfterBuildFailureSkipsEmptyID(t *testing.T) {
	called := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(clearDefinitionDisplayName, func(context.Context, string) error {
		called = true
		return nil
	})

	ReleaseTemplateDisplayNameAfterBuildFailure(context.Background(), "  ")
	if called {
		t.Fatal("expected no-op for empty template id")
	}
}

func TestClearDefinitionDisplayNameSkipsUpdateWhenNotFound(t *testing.T) {
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = origDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(context.Context, string) (*models.TemplateDefinition, error) {
		return nil, ErrTemplateNotFound
	})

	err := clearDefinitionDisplayName(context.Background(), "tpl-missing")
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("expected ErrTemplateNotFound, got %v", err)
	}
}
