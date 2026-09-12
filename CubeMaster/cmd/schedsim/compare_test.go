// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import "testing"

func TestParseCompareList(t *testing.T) {
	variants, err := parseCompareList("legacy=a.yaml, spread = b.yaml")
	if err != nil {
		t.Fatalf("parseCompareList: %v", err)
	}
	if len(variants) != 2 || variants[0] != [2]string{"legacy", "a.yaml"} || variants[1] != [2]string{"spread", "b.yaml"} {
		t.Fatalf("unexpected variants: %v", variants)
	}

	for _, raw := range []string{
		"",
		"onlyone=a.yaml",
		"noequalsign",
		"=a.yaml,b=c.yaml",
		"a=,b=c.yaml",
		"dup=a.yaml,dup=b.yaml",
	} {
		if _, err := parseCompareList(raw); err == nil {
			t.Fatalf("parseCompareList(%q) should fail", raw)
		}
	}
}
