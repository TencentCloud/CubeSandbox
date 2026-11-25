// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"strings"
	"testing"
)

func TestLabelChangeDetailNamesWhatMoved(t *testing.T) {
	before := map[string]string{"keep": "v", "same": "1", "gone": "x"}
	after := map[string]string{"keep": "v2", "same": "1", "new": "y"}

	got := labelChangeDetail(before, after)
	for _, want := range []string{"added=1", "changed=1", "removed=1", "-gone", "+new", "~keep"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "same") {
		t.Errorf("detail %q should not name unchanged keys", got)
	}
}

func TestLabelChangeDetailNoChange(t *testing.T) {
	same := map[string]string{"a": "1"}
	if got := labelChangeDetail(same, same); got != "no label change" {
		t.Errorf("detail = %q, want no label change", got)
	}
}
