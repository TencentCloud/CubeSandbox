// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import "testing"

// Inject.Render is the operator-facing preview of the header value CubeEgress
// will send. CubeEgress substitutes only the first ${SECRET}
// (`string.gsub(fmt, "%${SECRET}", escaped, 1)` in
// CubeEgress/lua/access_phase.lua), so the preview has to do the same or an
// operator is shown a credential the upstream never receives.
func TestInjectRenderSubstitutesOnlyFirstPlaceholder(t *testing.T) {
	cases := []struct {
		name   string
		format string
		secret string
		want   string
	}{
		{"bare placeholder", "${SECRET}", "tok", "tok"},
		{"prefixed", "Bearer ${SECRET}", "tok", "Bearer tok"},
		{"repeated", "Basic ${SECRET}:${SECRET}", "tok", "Basic tok:${SECRET}"},
		{
			"repeated three times",
			"${SECRET}-${SECRET}-${SECRET}",
			"tok",
			"tok-${SECRET}-${SECRET}",
		},
		{"empty format falls back", "", "tok", "tok"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Inject{Header: "Authorization", Secret: tc.secret, Format: tc.format}.Render()
			if got != tc.want {
				t.Errorf("Render() = %q, want %q", got, tc.want)
			}
		})
	}
}
