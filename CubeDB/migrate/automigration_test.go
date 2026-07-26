// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

import (
	"os"
	"testing"
)

func TestAutoMigrationEnabled(t *testing.T) {
	cases := []struct {
		name string
		set  bool // whether to set the env var at all
		val  string
		want bool
	}{
		{name: "unset", set: false, want: true},
		{name: "empty", set: true, val: "", want: true},
		{name: "true", set: true, val: "true", want: true},
		{name: "one", set: true, val: "1", want: true},
		{name: "True mixed case", set: true, val: "True", want: true},
		{name: "on", set: true, val: "on", want: true},
		{name: "yes", set: true, val: "yes", want: true},
		{name: "garbage keeps default", set: true, val: "maybe", want: true},
		{name: "false", set: true, val: "false", want: false},
		{name: "zero", set: true, val: "0", want: false},
		{name: "no", set: true, val: "no", want: false},
		{name: "off", set: true, val: "off", want: false},
		{name: "FALSE upper case", set: true, val: "FALSE", want: false},
		{name: "false with surrounding whitespace", set: true, val: "  false  ", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(autoMigrationEnv, tc.val)
			} else {
				// t.Setenv registers restoration of the prior value on cleanup;
				// call it once so the original env is restored, then Unsetenv to
				// model a genuinely absent variable for this case.
				t.Setenv(autoMigrationEnv, "")
				os.Unsetenv(autoMigrationEnv)
			}
			if got := AutoMigrationEnabled(); got != tc.want {
				t.Errorf("AutoMigrationEnabled() with set=%v val=%q = %v, want %v", tc.set, tc.val, got, tc.want)
			}
		})
	}
}
