// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"
)

func TestBootstrapJWTSecretRejectsAnEmptyStoredValue(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	if err := s.SetSystemSetting(ctx, "jwt_secret", ""); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}

	secret, err := s.BootstrapJWTSecret(ctx, "")
	if err == nil {
		t.Fatalf("BootstrapJWTSecret returned %q with no error for an empty stored secret", secret)
	}
	if secret != "" {
		t.Fatalf("BootstrapJWTSecret returned %q alongside an error, want empty", secret)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("BootstrapJWTSecret error = %v, want it to mention the empty value", err)
	}
}

func TestBootstrapJWTSecretGeneratesAndReusesANonEmptySecret(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	first, err := s.BootstrapJWTSecret(ctx, "")
	if err != nil {
		t.Fatalf("BootstrapJWTSecret: %v", err)
	}
	if first == "" {
		t.Fatal("BootstrapJWTSecret returned an empty secret on first run")
	}

	second, err := s.BootstrapJWTSecret(ctx, "")
	if err != nil {
		t.Fatalf("BootstrapJWTSecret (second run): %v", err)
	}
	if second != first {
		t.Fatalf("second run returned a different secret: %q != %q", second, first)
	}
}

func TestBootstrapJWTSecretPrefersTheEnvironmentSecret(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	ctx := context.Background()

	got, err := env.store.BootstrapJWTSecret(ctx, "env-secret-32-bytes-long-enough!")
	if err != nil {
		t.Fatalf("BootstrapJWTSecret: %v", err)
	}
	if got != "env-secret-32-bytes-long-enough!" {
		t.Fatalf("BootstrapJWTSecret = %q, want the env secret", got)
	}
}
