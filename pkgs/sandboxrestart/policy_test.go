// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxrestart

import "testing"

func TestPolicyStrings(t *testing.T) {
	cases := []struct {
		raw     string
		wire    string
		display string
		ok      bool
	}{
		{raw: "", wire: WireNever, display: DisplayNever, ok: true},
		{raw: "   ", wire: WireNever, display: DisplayNever, ok: true},
		{raw: "Never", wire: WireNever, display: DisplayNever, ok: true},
		{raw: "never", wire: WireNever, display: DisplayNever, ok: true},
		{raw: WireNever, wire: WireNever, display: DisplayNever, ok: true},
		{raw: "OnFailure", wire: WireOnFailure, display: DisplayOnFailure, ok: true},
		{raw: "onfailure", wire: WireOnFailure, display: DisplayOnFailure, ok: true},
		{raw: "RESTART_POLICY_ON_FAILURE", wire: WireOnFailure, display: DisplayOnFailure, ok: true},
		{raw: "Always", wire: WireAlways, display: DisplayAlways, ok: true},
		{raw: "always", wire: WireAlways, display: DisplayAlways, ok: true},
		{raw: "ReStArT_PoLiCy_AlWaYs", wire: WireAlways, display: DisplayAlways, ok: true},
		{raw: "nope", ok: false},
	}
	for _, tc := range cases {
		wire, ok := Normalize(tc.raw)
		if ok != tc.ok || (tc.ok && wire != tc.wire) {
			t.Errorf("Normalize(%q)=%q,%v want %q,%v", tc.raw, wire, ok, tc.wire, tc.ok)
		}
		got := Display(tc.raw)
		want := tc.display
		if !tc.ok {
			want = "nope"
		}
		if got != want {
			t.Errorf("Display(%q)=%q want %q", tc.raw, got, want)
		}
	}
}
