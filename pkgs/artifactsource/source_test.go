// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package artifactsource

import "testing"

func TestSourceContract(t *testing.T) {
	cases := []struct {
		source        Source
		valid         bool
		integrity     bool
		requiresToken bool
	}{
		{S3, true, true, false},
		{TemplateCenter, true, true, true},
		{CubeMaster, true, true, true},
		{Legacy, true, false, false},
		{Source(""), false, false, false},
		{Source("template_center"), false, false, false},
		{Source("unexpected"), false, false, false},
	}
	for _, tc := range cases {
		if got := tc.source.Valid(); got != tc.valid {
			t.Errorf("Source(%q).Valid() = %v, want %v", tc.source, got, tc.valid)
		}
		if got := tc.source.RequiresIntegrityCheck(); got != tc.integrity {
			t.Errorf("Source(%q).RequiresIntegrityCheck() = %v, want %v", tc.source, got, tc.integrity)
		}
		if got := tc.source.RequiresToken(); got != tc.requiresToken {
			t.Errorf("Source(%q).RequiresToken() = %v, want %v", tc.source, got, tc.requiresToken)
		}
	}
}
