// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"strings"
	"testing"
)

func TestSafePrintImageSpecMasksCredentials(t *testing.T) {
	spec := &ImageSpec{
		Image: "repo/image:tag",
		Annotations: map[string]string{
			MasterAnnotationsImageUserName: "user",
			MasterAnnotationsImagetoken:    "secret-token",
			"plain.key":                    "visible",
		},
	}
	out := SafePrintImageSpec(spec)
	if strings.Contains(out, "secret-token") {
		t.Errorf("SafePrintImageSpec leaked token: %s", out)
	}
	if !strings.Contains(out, "\"plain.key\":\"visible\"") {
		t.Errorf("non-sensitive annotation should stay visible: %s", out)
	}
	// The input spec must be left untouched.
	if got := spec.GetToken(); got != "secret-token" {
		t.Errorf("original spec mutated: token = %q", got)
	}
	if got := spec.GetUsername(); got != "user" {
		t.Errorf("original spec mutated: username = %q", got)
	}
}

func TestSafePrintImageSpecNil(t *testing.T) {
	if got := SafePrintImageSpec(nil); got != "nil" {
		t.Errorf("SafePrintImageSpec(nil) = %q, want %q", got, "nil")
	}
}
