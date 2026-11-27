// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"reflect"
	"testing"
)

func TestCanonicalInventory(t *testing.T) {
	a := LocalTemplate{Namespace: "a", TemplateID: "same", ID: "one", Path: "/one"}
	b := LocalTemplate{Namespace: "b", TemplateID: "same", ID: "two", Path: "/two"}
	input := []LocalTemplate{b, a}
	want := []LocalTemplate{a, b}
	if got := CanonicalLocalTemplates(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("templates = %#v, want %#v", got, want)
	}
	if input[0] != b {
		t.Fatal("normalization mutated the caller's templates")
	}
	if MustJSON(CanonicalLocalTemplates(input)) != MustJSON(CanonicalLocalTemplates(want)) {
		t.Fatal("order-only template changes produce different JSON")
	}
	images := []ContainerImage{{Namespace: "b", Names: []string{"z", "a"}}, {Namespace: "a", Names: []string{"b"}}}
	ordered := []ContainerImage{{Namespace: "a", Names: []string{"b"}}, {Namespace: "b", Names: []string{"a", "z"}}}
	if got := CanonicalImages(images); !reflect.DeepEqual(got, ordered) {
		t.Fatalf("images = %#v, want %#v", got, ordered)
	}
	if images[0].Names[0] != "z" {
		t.Fatal("normalization mutated the caller's image references")
	}
	if CanonicalImages(nil) != nil || CanonicalLocalTemplates(nil) != nil {
		t.Fatal("nil inventories must retain their wire representation")
	}
}
