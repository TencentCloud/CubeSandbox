// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package hostcompat

import (
	"regexp"
	"strings"
	"testing"
)

func TestTemplateVariantID(t *testing.T) {
	got, key, ok := TemplateVariantID("tpl-a", " sha256:cpu ", " 5.15.0 ")
	if !ok || got != "tpl-a-"+key || !strings.HasPrefix(key, VariantPolicyV1+"-") {
		t.Fatalf("TemplateVariantID() = %q, %q, %v", got, key, ok)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]+(?:[._-](?:[A-Za-z0-9]+))*$`).MatchString(got) {
		t.Fatalf("TemplateVariantID() returned unsafe sandbox id %q", got)
	}
	again, _, ok := TemplateVariantID("tpl-a", "sha256:cpu", "5.15.0")
	if !ok || again != got {
		t.Fatalf("trimmed input changed identity: %q != %q", again, got)
	}
	other, _, ok := TemplateVariantID("tpl-a", "sha256:other", "5.15.0")
	if !ok || other == got {
		t.Fatalf("different host facts did not change identity: %q", other)
	}
}

func TestTemplateVariantIDRequiresFacts(t *testing.T) {
	for _, tc := range []struct {
		logical, cpuid, kernel string
	}{
		{"", "cpu", "kernel"},
		{"tpl-a", "", "kernel"},
		{"tpl-a", "cpu", ""},
	} {
		if _, _, ok := TemplateVariantID(tc.logical, tc.cpuid, tc.kernel); ok {
			t.Fatalf("TemplateVariantID(%q, %q, %q) unexpectedly succeeded", tc.logical, tc.cpuid, tc.kernel)
		}
	}
}
