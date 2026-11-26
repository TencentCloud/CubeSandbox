// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

func TestValidateHostMemoryQuota(t *testing.T) {
	for _, tc := range []struct {
		value   string
		invalid bool
	}{
		{"", false}, {"1Mi", false}, {"4Gi", false},
		{"0", true}, {"-1Gi", true}, {"512Ki", true}, {"invalid", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			err := validate(&Config{HostConf: &HostConf{Quota: HostConfigQuota{Mem: tc.value}}})
			if (err != nil) != tc.invalid {
				t.Fatalf("validate mem_limit %q: %v", tc.value, err)
			}
		})
	}
}
