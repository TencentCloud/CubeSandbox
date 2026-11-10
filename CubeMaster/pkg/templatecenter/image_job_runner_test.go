// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
)

// nilLogger satisfies the logger interface used by
// markDefinitionFailedOnPersistErr. It is safe because the test stubs
// control publishTemplateStatusWithAlias and the helper only calls
// Errorf when the stub returns a non-nil update error.
type nilLogger struct{}

func (nilLogger) Errorf(string, ...any) {}

// TestMarkDefinitionFailedOnPersistErr_PublishesFailed verifies that when
// replica persistence fails, the template definition is published as FAILED
// with a sanitized error message, and the returned error is the original
// persistErr when the publish succeeds.
func TestMarkDefinitionFailedOnPersistErr_PublishesFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")

	var publishedStatus, publishedLastError string
	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			publishedStatus = status
			publishedLastError = lastError
			return "", "", nil
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-1", 2, persistErr, nilLogger{},
	)

	// Definition must be published as FAILED.
	if publishedStatus != StatusFailed {
		t.Fatalf("published status = %q, want %q", publishedStatus, StatusFailed)
	}
	// The error written to the definition must be sanitized: no raw DB error.
	if strings.Contains(publishedLastError, "invalid connection") {
		t.Fatalf("last_error leaks raw DB error: %q", publishedLastError)
	}
	if !strings.Contains(publishedLastError, "2 node(s)") {
		t.Fatalf("last_error missing node count: %q", publishedLastError)
	}
	if !strings.Contains(publishedLastError, "job-1") {
		t.Fatalf("last_error missing job ID: %q", publishedLastError)
	}
	// Returned error is the original persistErr (publish succeeded).
	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
}

// TestMarkDefinitionFailedOnPersistErr_TemplateNotFound verifies that when
// publishTemplateStatusWithAlias returns ErrTemplateNotFound (concurrent
// delete removed the definition row), the update error is suppressed and
// the returned error contains only persistErr.
func TestMarkDefinitionFailedOnPersistErr_TemplateNotFound(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")

	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			return "", "", ErrTemplateNotFound
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-gone", "job-2", 1, persistErr, nilLogger{},
	)

	// ErrTemplateNotFound must be suppressed.
	if errors.Is(err, ErrTemplateNotFound) {
		t.Fatal("returned error should not wrap ErrTemplateNotFound")
	}
	// Only persistErr should remain.
	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
}

// TestMarkDefinitionFailedOnPersistErr_JoinsErrors verifies that when
// publishTemplateStatusWithAlias returns a non-ErrTemplateNotFound error,
// the returned error satisfies errors.Is for both the original persistErr
// and the update error (via errors.Join).
func TestMarkDefinitionFailedOnPersistErr_JoinsErrors(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")
	updateErr := errors.New("database connection timeout")

	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			return "", "", updateErr
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-3", 3, persistErr, nilLogger{},
	)

	// Both errors must be matchable through errors.Is.
	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	if !errors.Is(err, updateErr) {
		t.Fatalf("returned error %v does not wrap updateErr", err)
	}
	// The wrapped update error must also be unwrap-able via the %w verb.
	if !strings.Contains(err.Error(), "database connection timeout") {
		t.Fatalf("returned error %q missing update error message", err)
	}
}

// TestMarkDefinitionFailedOnPersistErr_SanitizesErrorMessage verifies that
// the sanitized message written to the definition never contains raw SQL or
// driver error fragments, even when the persist error carries sensitive
// internal details.
func TestMarkDefinitionFailedOnPersistErr_SanitizesErrorMessage(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	// Simulate a raw GORM/driver error with SQL fragments.
	rawDBErr := fmt.Errorf(
		"Error 1045 (28000): Access denied for user 'cube'@'10.0.0.5' " +
			"(using password: YES) [table=template_replicas, col=node_id]",
	)

	var publishedLastError string
	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			publishedLastError = lastError
			return "", "", nil
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	_ = markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-sanitize", 1, rawDBErr, nilLogger{},
	)

	// None of the raw DB error content should appear in the published message.
	for _, leak := range []string{"Error 1045", "Access denied", "template_replicas", "col=node_id", "10.0.0.5"} {
		if strings.Contains(publishedLastError, leak) {
			t.Fatalf("last_error leaks %q: %q", leak, publishedLastError)
		}
	}
}
