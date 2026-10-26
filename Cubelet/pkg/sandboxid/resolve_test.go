// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxid

import (
	"errors"
	"testing"
)

func TestResolveExactMatch(t *testing.T) {
	candidates := []string{
		"aabbccddeeff00112233445566778899",
		"112233445566778899aabbccddeeff00",
	}
	got, err := Resolve("aabbccddeeff00112233445566778899", candidates)
	if err != nil {
		t.Fatalf("Resolve() err=%v", err)
	}
	if got != candidates[0] {
		t.Fatalf("Resolve()=%q, want %q", got, candidates[0])
	}
}

func TestResolveUniquePrefix(t *testing.T) {
	candidates := []string{
		"aabbccddeeff00112233445566778899",
		"112233445566778899aabbccddeeff00",
	}
	got, err := Resolve("aabbccdd", candidates)
	if err != nil {
		t.Fatalf("Resolve() err=%v", err)
	}
	if got != candidates[0] {
		t.Fatalf("Resolve()=%q, want %q", got, candidates[0])
	}
}

func TestResolveAmbiguousPrefix(t *testing.T) {
	candidates := []string{
		"aabbccddeeff00112233445566778899",
		"aabbccddeeff00112233445566778898",
	}
	_, err := Resolve("aabbccdd", candidates)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve() err=%v, want ErrAmbiguous", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	_, err := Resolve("deadbeef", []string{"aabbccddeeff00112233445566778899"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve() err=%v, want ErrNotFound", err)
	}
}

func TestResolveFullIDRequiresCandidate(t *testing.T) {
	fullID := "AABBCCDDEEFF00112233445566778899"
	_, err := Resolve(fullID, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve() err=%v, want ErrNotFound", err)
	}

	got, err := Resolve(fullID, []string{"aabbccddeeff00112233445566778899"})
	if err != nil {
		t.Fatalf("Resolve() err=%v", err)
	}
	if got != "aabbccddeeff00112233445566778899" {
		t.Fatalf("Resolve()=%q, want exact candidate", got)
	}
}

func TestIsFullID(t *testing.T) {
	if !IsFullID("aabbccddeeff00112233445566778899") {
		t.Fatal("IsFullID(full lowercase)=false, want true")
	}
	if !IsFullID("AABBCCDDEEFF00112233445566778899") {
		t.Fatal("IsFullID(full uppercase)=false, want true")
	}
	if IsFullID("aabbccdd") {
		t.Fatal("IsFullID(short)=true, want false")
	}
	if IsFullID("aabbccddeeff0011223344556677889g") {
		t.Fatal("IsFullID(non-hex)=true, want false")
	}
}

func TestNormalizeInput(t *testing.T) {
	if got := NormalizeInput("  abc  "); got != "abc" {
		t.Fatalf("NormalizeInput()=%q, want %q", got, "abc")
	}
}
