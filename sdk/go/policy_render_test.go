// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import "testing"

func TestInjectRenderMatchesServerSubstitution(t *testing.T) {
	cases := []struct {
		name   string
		format string
		secret string
		want   string
	}{
		{"default format", "", "tok", "tok"},
		{"single placeholder", "Bearer ${SECRET}", "tok", "Bearer tok"},
		{"repeated placeholder", "Basic ${SECRET}:${SECRET}", "tok", "Basic tok:${SECRET}"},
		{"three placeholders", "${SECRET}-${SECRET}-${SECRET}", "tok", "tok-${SECRET}-${SECRET}"},
		{"percent in secret", "Bearer ${SECRET}", "a%2Fb", "Bearer a%2Fb"},
		{"no placeholder", "Bearer static", "tok", "Bearer static"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Inject{Header: "H", Secret: tc.secret, Format: tc.format}.Render()
			if got != tc.want {
				t.Fatalf("Render() = %q, want %q", got, tc.want)
			}
		})
	}
}
