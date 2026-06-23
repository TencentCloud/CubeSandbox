// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const maxTemplateDisplayNameLen = 256

var (
	ErrTemplateNameNotFound  = errors.New("template name not found")
	ErrTemplateNameAmbiguous = errors.New("template name is ambiguous")
	ErrTemplateNameInUse     = errors.New("template name is already in use")
	ErrTemplateNameInvalid   = errors.New("template name is invalid")
)

// NormalizeTemplateDisplayName trims and validates an E2B-style template name.
// An empty string means no display name (optional). Allowed characters are ASCII
// letters, digits, hyphen, and underscore only (same charset as E2B template names).
func NormalizeTemplateDisplayName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if len(name) > maxTemplateDisplayNameLen {
		return "", fmt.Errorf("%w: exceeds %d characters", ErrTemplateNameInvalid, maxTemplateDisplayNameLen)
	}
	for i, r := range name {
		if !isAllowedTemplateDisplayNameRune(r) {
			return "", fmt.Errorf("%w: must contain only letters, digits, hyphens, and underscores", ErrTemplateNameInvalid)
		}
		if i == 0 && !isAllowedTemplateDisplayNameStartRune(r) {
			return "", fmt.Errorf("%w: must start with a letter or digit", ErrTemplateNameInvalid)
		}
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "tpl-") || strings.HasPrefix(lower, "snap-") {
		return "", fmt.Errorf("%w: must not use tpl- or snap- prefix", ErrTemplateNameInvalid)
	}
	return name, nil
}

func isAllowedTemplateDisplayNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_'
}

func isAllowedTemplateDisplayNameStartRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

// FindTemplateIDByDisplayName resolves a template name to its templateID using a
// case-insensitive exact match on display_name. Results are cached briefly with
// concurrent lookup coalescing on the same normalized name.
//
// Cache is in-process only. With multiple CubeMaster replicas, prefer
// FindTemplateIDByDisplayNameFresh on consistency-sensitive paths (rename, create
// validation); otherwise callers may observe stale positive or negative entries
// until TTL expiry or until that replica executes a mutating display-name path.
func FindTemplateIDByDisplayName(ctx context.Context, name string) (string, error) {
	return findTemplateIDByDisplayName(ctx, name, false)
}

// FindTemplateIDByDisplayNameFresh bypasses the read cache (still uses singleflight).
func FindTemplateIDByDisplayNameFresh(ctx context.Context, name string) (string, error) {
	return findTemplateIDByDisplayName(ctx, name, true)
}

func findTemplateIDByDisplayName(ctx context.Context, name string, fresh bool) (string, error) {
	if !isReady() {
		return "", ErrTemplateStoreNotInitialized
	}
	normalized, err := NormalizeTemplateDisplayName(name)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "", ErrTemplateNameNotFound
	}
	cacheKey := strings.ToLower(normalized)
	if !fresh {
		if templateID, ok := getCachedTemplateIDByDisplayName(cacheKey); ok {
			return templateID, nil
		}
		if isDisplayNameNotFoundCached(cacheKey) {
			return "", ErrTemplateNameNotFound
		}
	}
	groupKey := cacheKey
	if fresh {
		groupKey = "fresh:" + cacheKey
	}
	result, err := templateDisplayNameFetchGroup.Do(groupKey, func() (interface{}, error) {
		// Re-check after entering singleflight: a concurrent goroutine may have
		// populated the cache between the outer check and Do().
		if !fresh {
			if templateID, ok := getCachedTemplateIDByDisplayName(cacheKey); ok {
				return templateID, nil
			}
			if isDisplayNameNotFoundCached(cacheKey) {
				return "", ErrTemplateNameNotFound
			}
		}
		templateID, lookupErr := findTemplateIDByDisplayNameFromDB(ctx, normalized)
		if lookupErr != nil {
			if errors.Is(lookupErr, ErrTemplateNameNotFound) && !fresh {
				setTemplateDisplayNameNotFoundCache(cacheKey)
			}
			return "", lookupErr
		}
		setTemplateDisplayNameCache(cacheKey, templateID)
		return templateID, nil
	})
	if err != nil {
		return "", err
	}
	templateID, _ := result.(string)
	return templateID, nil
}

func findTemplateIDByDisplayNameFromDB(ctx context.Context, normalized string) (string, error) {
	owners, err := findDisplayNameOwnersFromDB(ctx, normalized)
	if err != nil {
		return "", err
	}
	switch len(owners) {
	case 0:
		return "", ErrTemplateNameNotFound
	case 1:
		return owners[0].TemplateID, nil
	default:
		return "", ErrTemplateNameAmbiguous
	}
}

type displayNameOwner struct {
	TemplateID string
	Status     string
}

func findDisplayNameOwnersFromDB(ctx context.Context, normalized string) ([]displayNameOwner, error) {
	var owners []displayNameOwner
	if err := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Select("template_id", "status").
		Where("display_name_key = ? AND kind = ?", strings.ToLower(normalized), TemplateKindTemplate).
		Limit(2).
		Find(&owners).Error; err != nil {
		return nil, err
	}
	return owners, nil
}

func displayNameLockKey(normalized string) string {
	return "display-name:" + strings.ToLower(normalized)
}

func displayNameFromDefinition(def *models.TemplateDefinition) string {
	if def == nil {
		return ""
	}
	return def.DisplayName
}

func assertDisplayNameAvailableForCreate(ctx context.Context, normalized, templateID string) error {
	owners, err := findDisplayNameOwnersFromDB(ctx, normalized)
	if err != nil {
		return err
	}
	switch len(owners) {
	case 0:
		return nil
	case 1:
		owner := owners[0]
		if owner.TemplateID == templateID {
			return nil
		}
		if strings.EqualFold(owner.Status, StatusFailed) {
			def, defErr := GetDefinition(ctx, owner.TemplateID)
			if errors.Is(defErr, ErrTemplateNotFound) {
				return nil
			}
			if defErr != nil {
				return defErr
			}
			// Caller (withDisplayNameCreateLock) already holds the per-name write lock.
			return clearDefinitionDisplayNameLocked(ctx, owner.TemplateID, def)
		}
		return fmt.Errorf("%w: %q", ErrTemplateNameInUse, normalized)
	default:
		return ErrTemplateNameAmbiguous
	}
}

// withDisplayNameCreateLock runs fn while holding the per-name write lock after
// verifying the name is free. Used to keep availability check and definition INSERT
// atomic in ensureTemplateDefinition.
func withDisplayNameCreateLock(ctx context.Context, displayName, templateID string, fn func() error) error {
	normalized, err := NormalizeTemplateDisplayName(displayName)
	if err != nil {
		return err
	}
	if normalized == "" {
		return fn()
	}
	return withTemplateWriteLock(displayNameLockKey(normalized), func() error {
		if err := assertDisplayNameAvailableForCreate(ctx, normalized, templateID); err != nil {
			return err
		}
		return fn()
	})
}

// ReserveTemplateDisplayNameForCreate checks that displayName is free before persisting
// a template definition. Names on FAILED templates are cleared so they can be reused.
func ReserveTemplateDisplayNameForCreate(ctx context.Context, displayName, templateID string) error {
	return withDisplayNameCreateLock(ctx, displayName, templateID, func() error { return nil })
}

// ReleaseTemplateDisplayNameAfterBuildFailure clears display_name on a template
// whose image build failed so the name can be reused on a later create.
func ReleaseTemplateDisplayNameAfterBuildFailure(ctx context.Context, templateID string) {
	if strings.TrimSpace(templateID) == "" {
		return
	}
	def, err := GetDefinition(ctx, templateID)
	if err != nil {
		if !errors.Is(err, ErrTemplateNotFound) {
			log.G(ctx).Warnf("release template display name after build failure: lookup template=%s err=%v", templateID, err)
		}
		return
	}
	normalized, normErr := NormalizeTemplateDisplayName(def.DisplayName)
	if normErr != nil || normalized == "" {
		return
	}
	if err := clearDefinitionDisplayName(ctx, templateID); err != nil &&
		!errors.Is(err, ErrTemplateNotFound) {
		log.G(ctx).Warnf("release template display name after build failure: template=%s err=%v", templateID, err)
	}
}

// clearDefinitionDisplayNameLocked clears display_name without acquiring the per-name
// write lock. Callers that already hold displayNameLockKey(normalized) must use this
// path to avoid reentrant lock deadlock.
func clearDefinitionDisplayNameLocked(ctx context.Context, templateID string, def *models.TemplateDefinition) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return errors.New("template_id is required")
	}
	result := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id = ?", templateID).
		Update("display_name", "")
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrTemplateNotFound
	}
	if def != nil {
		invalidateTemplateDisplayNameCache(def.DisplayName)
	}
	invalidateTemplateCaches(templateID)
	return nil
}

func clearDefinitionDisplayName(ctx context.Context, templateID string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return errors.New("template_id is required")
	}
	def, defErr := GetDefinition(ctx, templateID)
	if errors.Is(defErr, ErrTemplateNotFound) {
		return ErrTemplateNotFound
	}
	if defErr != nil {
		return defErr
	}
	if normalized, normErr := NormalizeTemplateDisplayName(def.DisplayName); normErr == nil && normalized != "" {
		return withTemplateWriteLock(displayNameLockKey(normalized), func() error {
			return clearDefinitionDisplayNameLocked(ctx, templateID, def)
		})
	}
	return clearDefinitionDisplayNameLocked(ctx, templateID, def)
}

// ValidateTemplateDisplayNameAvailable ensures name is not used by another template.
func ValidateTemplateDisplayNameAvailable(ctx context.Context, name, excludeTemplateID string) error {
	existing, err := findTemplateIDByDisplayNameFromDB(ctx, name)
	if errors.Is(err, ErrTemplateNameNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if excludeTemplateID != "" && existing == excludeTemplateID {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrTemplateNameInUse, strings.TrimSpace(name))
}

// UpdateDefinitionDisplayName sets the display_name for an existing template definition.
func UpdateDefinitionDisplayName(ctx context.Context, templateID, displayName string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return errors.New("template_id is required")
	}
	def, err := GetDefinition(ctx, templateID)
	if err != nil {
		return err
	}
	if kind := strings.TrimSpace(def.Kind); !definitionSupportsTemplateDisplayName(kind) {
		return ErrTemplateNotFound
	}
	normalized, err := normalizeRequiredTemplateDisplayName(displayName)
	if err != nil {
		return err
	}
	return withTemplateWriteLock(displayNameLockKey(normalized), func() error {
		if err := ValidateTemplateDisplayNameAvailable(ctx, normalized, templateID); err != nil {
			return err
		}
		oldDisplayName := def.DisplayName
		result := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
			Where("template_id = ?", templateID).
			Update("display_name", normalized)
		if result.Error != nil {
			return mapDefinitionCreateDuplicateError(ctx, result.Error, normalized)
		}
		if result.RowsAffected == 0 {
			return ErrTemplateNotFound
		}
		invalidateTemplateDisplayNameCache(oldDisplayName, normalized)
		invalidateTemplateCaches(templateID)
		return nil
	})
}

// definitionSupportsTemplateDisplayName reports whether a definition kind can
// carry a user-facing template name. Only kind == "template" qualifies, keeping
// this in lockstep with the display_name_key generated column (NULL unless
// kind = 'template') and FindTemplateIDByDisplayName (filters kind = 'template').
func definitionSupportsTemplateDisplayName(kind string) bool {
	return strings.TrimSpace(kind) == TemplateKindTemplate
}

func normalizeRequiredTemplateDisplayName(displayName string) (string, error) {
	normalized, err := NormalizeTemplateDisplayName(displayName)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "", fmt.Errorf("%w: name is required", ErrTemplateNameInvalid)
	}
	return normalized, nil
}

func mapDefinitionCreateDuplicateError(ctx context.Context, err error, displayName string) error {
	if err == nil {
		return err
	}
	if !isDuplicateKeyError(err) {
		log.G(ctx).Errorf("template definition DB error: %v", err)
		return errors.New("internal error")
	}
	if strings.Contains(err.Error(), "display_name_key") && strings.TrimSpace(displayName) != "" {
		return fmt.Errorf("%w: %q", ErrTemplateNameInUse, strings.TrimSpace(displayName))
	}
	return ErrDuplicateTemplate
}

func displayNameFromTemplateImageJob(ctx context.Context, job *models.TemplateImageJob) string {
	if job == nil || strings.TrimSpace(job.RequestJSON) == "" {
		return ""
	}
	var req types.CreateTemplateFromImageReq
	if err := json.Unmarshal([]byte(job.RequestJSON), &req); err != nil {
		log.G(ctx).Warnf("template image job display_name unmarshal failed: %v", err)
		return ""
	}
	normalized, err := NormalizeTemplateDisplayName(req.DisplayName)
	if err != nil {
		log.G(ctx).Warnf("template image job display_name normalize failed: %v", err)
		return ""
	}
	return normalized
}
