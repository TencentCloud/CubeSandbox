// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"
)

func TestBootstrapJWTSecretRepairsAnEmptyStoredValue(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	if err := s.SetSystemSetting(ctx, "jwt_secret", ""); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}

	secret, err := s.BootstrapJWTSecret(ctx, "")
	if err != nil {
		t.Fatalf("BootstrapJWTSecret did not repair an empty stored secret: %v", err)
	}
	if strings.TrimSpace(secret) == "" {
		t.Fatal("BootstrapJWTSecret returned an empty secret after repair")
	}

	persisted, err := s.GetSystemSetting(ctx, "jwt_secret")
	if err != nil {
		t.Fatalf("GetSystemSetting: %v", err)
	}
	if persisted != secret {
		t.Fatalf("repaired secret was not persisted: row=%q returned=%q", persisted, secret)
	}

	again, err := s.BootstrapJWTSecret(ctx, "")
	if err != nil {
		t.Fatalf("BootstrapJWTSecret after repair: %v", err)
	}
	if again != secret {
		t.Fatalf("a later start got a different secret: %q != %q", again, secret)
	}
}

func TestBootstrapJWTSecretDoesNotOverwriteAHealthyValue(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	if err := s.SetSystemSetting(ctx, "jwt_secret", "already-good-32-bytes-long-ok!!!"); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}

	secret, err := s.BootstrapJWTSecret(ctx, "")
	if err != nil {
		t.Fatalf("BootstrapJWTSecret: %v", err)
	}
	if secret != "already-good-32-bytes-long-ok!!!" {
		t.Fatalf("BootstrapJWTSecret = %q, want the stored value untouched", secret)
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
