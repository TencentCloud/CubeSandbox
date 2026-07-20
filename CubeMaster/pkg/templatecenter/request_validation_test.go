// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"strings"
	"testing"
)

// TestValidateTemplateAlias exercises the alias format contract used by the
// template-alias feature (#584): the alias must be lowercase alphanumeric and
// hyphens, start with an alphanumeric character, be at most 64 characters,
// and must not collide with the tpl-/snap- ID-prefix namespace. An empty
// alias is valid — it means "no alias requested".
func TestValidateTemplateAlias(t *testing.T) {
	const maxLen = 64
	longAlias := strings.Repeat("a", maxLen+1) // 65 chars — one over the limit

	tests := []struct {
		name      string
		alias     string
		wantValid bool
	}{
		// Valid cases.
		{name: "simple", alias: "my-app", wantValid: true},
		{name: "with-version", alias: "app-v2", wantValid: true},
		{name: "single-char", alias: "a", wantValid: true},
		{name: "alphanumeric", alias: "abc123", wantValid: true},
		{name: "leading-digit", alias: "1", wantValid: true},
		{name: "digit-prefixed", alias: "2nd-app", wantValid: true},
		{name: "max-length-63-chars", alias: strings.Repeat("a", maxLen-1), wantValid: true},
		{name: "max-length-64-chars", alias: strings.Repeat("a", maxLen), wantValid: true},
		{name: "trailing-dash", alias: "myapp-", wantValid: true},
		{name: "empty-means-no-alias", alias: "", wantValid: true},
		{name: "whitespace-only-trims-to-empty", alias: "   ", wantValid: true},

		// Invalid cases.
		{name: "uppercase", alias: "MyApp", wantValid: false},
		{name: "tpl-prefix", alias: "tpl-xxx", wantValid: false},
		{name: "snap-prefix", alias: "snap-xxx", wantValid: false},
		{name: "too-long-65-chars", alias: longAlias, wantValid: false},
		{name: "underscore", alias: "my_app", wantValid: false},
		{name: "leading-dash", alias: "-myapp", wantValid: false},
		{name: "internal-space", alias: "my app", wantValid: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTemplateAlias(tc.alias)
			if tc.wantValid && err != nil {
				t.Fatalf("validateTemplateAlias(%q) returned unexpected error: %v", tc.alias, err)
			}
			if !tc.wantValid && err == nil {
				t.Fatalf("validateTemplateAlias(%q) returned nil, want an error", tc.alias)
			}
		})
	}
}
