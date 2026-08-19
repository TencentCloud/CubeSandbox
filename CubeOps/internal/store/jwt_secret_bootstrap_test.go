// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestBootstrapJWTSecret(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	raw, err := sql.Open("mysql", env.dsn)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer raw.Close()

	setRow := func(t *testing.T, value any) {
		t.Helper()
		if _, err := raw.ExecContext(ctx,
			"REPLACE INTO t_system_setting (setting_key, setting_value) VALUES (?, ?)",
			"jwt_secret", value); err != nil {
			t.Fatalf("seed jwt_secret=%v: %v", value, err)
		}
	}
	clearRow := func(t *testing.T) {
		t.Helper()
		if _, err := raw.ExecContext(ctx,
			"DELETE FROM t_system_setting WHERE setting_key = ?", "jwt_secret"); err != nil {
			t.Fatalf("delete jwt_secret: %v", err)
		}
	}
	readRow := func(t *testing.T) string {
		t.Helper()
		var v sql.NullString
		if err := raw.QueryRowContext(ctx,
			"SELECT setting_value FROM t_system_setting WHERE setting_key = ?", "jwt_secret").Scan(&v); err != nil {
			t.Fatalf("read jwt_secret: %v", err)
		}
		return v.String
	}

	t.Run("generates and reuses on a clean database", func(t *testing.T) {
		clearRow(t)

		first, err := s.BootstrapJWTSecret(ctx, "")
		if err != nil {
			t.Fatalf("BootstrapJWTSecret: %v", err)
		}
		if first == "" {
			t.Fatal("returned an empty secret on first run")
		}
		second, err := s.BootstrapJWTSecret(ctx, "")
		if err != nil {
			t.Fatalf("BootstrapJWTSecret (second run): %v", err)
		}
		if second != first {
			t.Fatalf("a later start got a different secret: %q != %q", second, first)
		}
	})

	t.Run("repairs degenerate stored values", func(t *testing.T) {
		for _, stored := range []any{"", " ", "\t", "   \t ", "\n", nil} {
			setRow(t, stored)

			secret, err := s.BootstrapJWTSecret(ctx, "")
			if err != nil {
				t.Errorf("did not repair stored value %#v: %v", stored, err)
				continue
			}
			if strings.TrimSpace(secret) == "" {
				t.Errorf("returned %q after repairing %#v", secret, stored)
				continue
			}
			if got := readRow(t); got != secret {
				t.Errorf("repair for %#v was not persisted: row=%q returned=%q", stored, got, secret)
			}
		}
	})

	t.Run("does not overwrite a healthy value", func(t *testing.T) {
		setRow(t, "already-good-32-bytes-long-ok!!!")

		secret, err := s.BootstrapJWTSecret(ctx, "")
		if err != nil {
			t.Fatalf("BootstrapJWTSecret: %v", err)
		}
		if secret != "already-good-32-bytes-long-ok!!!" {
			t.Fatalf("BootstrapJWTSecret = %q, want the stored value untouched", secret)
		}
	})

	t.Run("prefers a usable environment secret", func(t *testing.T) {
		setRow(t, "db-secret-32-bytes-long-enough!!")

		secret, err := s.BootstrapJWTSecret(ctx, "env-secret-32-bytes-long-enough!")
		if err != nil {
			t.Fatalf("BootstrapJWTSecret: %v", err)
		}
		if secret != "env-secret-32-bytes-long-enough!" {
			t.Fatalf("BootstrapJWTSecret = %q, want the env secret", secret)
		}
	})

	t.Run("ignores a whitespace-only environment secret", func(t *testing.T) {
		setRow(t, "db-secret-32-bytes-long-enough!!")

		secret, err := s.BootstrapJWTSecret(ctx, "   ")
		if err != nil {
			t.Fatalf("BootstrapJWTSecret: %v", err)
		}
		if secret != "db-secret-32-bytes-long-enough!!" {
			t.Fatalf("BootstrapJWTSecret = %q, want the stored secret; a whitespace-only "+
				"JWT_SECRET would make every token operation fail at runtime", secret)
		}
	})
}
