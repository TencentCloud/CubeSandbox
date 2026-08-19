// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package crypto

import "testing"

func TestVerifyPasswordRejectsEmptyInputs(t *testing.T) {
	cases := []struct {
		name      string
		stored    string
		candidate string
	}{
		{"both empty", "", ""},
		{"empty stored, non-empty candidate", "", "hunter2"},
		{"non-empty stored, empty candidate", "legacy-password", ""},
		{"empty stored, empty-looking bcrypt candidate", "", "$2a$10$abcdefghijklmnopqrstuv"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyPassword(tc.stored, tc.candidate) {
				t.Fatalf("VerifyPassword(%q, %q) = true, want false", tc.stored, tc.candidate)
			}
		})
	}
}

func TestVerifyPasswordStillAcceptsValidCredentials(t *testing.T) {
	hashed, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hashed, "correct horse") {
		t.Fatal("bcrypt round trip rejected the correct password")
	}
	if VerifyPassword(hashed, "wrong horse") {
		t.Fatal("bcrypt accepted the wrong password")
	}
	if !VerifyPassword("legacy-password", "legacy-password") {
		t.Fatal("legacy plaintext comparison rejected a matching password")
	}
	if VerifyPassword("legacy-password", "wrong") {
		t.Fatal("legacy plaintext comparison accepted a wrong password")
	}
}
