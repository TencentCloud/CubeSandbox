// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
)

func TestGettersReturnEmptyAndNilWhenTheRowIsAbsent(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	cases := []struct {
		name string
		get  func() (string, error)
	}{
		{"GetSystemSetting", func() (string, error) { return s.GetSystemSetting(ctx, "no-such-system-setting") }},
		{"GetSetting", func() (string, error) { return s.GetSetting(ctx, "no-such-agenthub-setting") }},
		{"GetUserPassword", func() (string, error) { return s.GetUserPassword(ctx, "no-such-user") }},
	}
	for _, c := range cases {
		got, err := c.get()
		if err != nil {
			t.Errorf("%s on a missing row returned an error: %v", c.name, err)
		}
		if got != "" {
			t.Errorf("%s on a missing row returned %q, want empty", c.name, got)
		}
	}
}

func TestGettersPropagateDriverErrors(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		get  func() (string, error)
	}{
		{"GetSystemSetting", func() (string, error) { return s.GetSystemSetting(ctx, "secret_master_key") }},
		{"GetSetting", func() (string, error) { return s.GetSetting(ctx, "any") }},
		{"GetUserPassword", func() (string, error) { return s.GetUserPassword(ctx, "admin") }},
	}
	for _, c := range cases {
		got, err := c.get()
		if err == nil {
			t.Errorf("%s swallowed a driver error and returned %q with no error", c.name, got)
		}
		if got != "" {
			t.Errorf("%s returned %q alongside an error, want empty", c.name, got)
		}
	}
}

func TestGetUserPasswordReturnsTheStoredHashForTheSeededAdmin(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	ctx := context.Background()

	got, err := env.store.GetUserPassword(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserPassword: %v", err)
	}
	if got == "" {
		t.Fatal("GetUserPassword returned an empty hash for the seeded admin account")
	}
}

func TestGettersReportAnEmptyStoredValueAsAbsent(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	if err := s.SetSystemSetting(ctx, "empty-on-purpose", ""); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
	got, err := s.GetSystemSetting(ctx, "empty-on-purpose")
	if err != nil {
		t.Fatalf("GetSystemSetting on an empty stored value returned an error: %v", err)
	}
	if got != "" {
		t.Fatalf("GetSystemSetting = %q, want empty", got)
	}

	if err := s.SetSetting(ctx, "empty-on-purpose", ""); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, err = s.GetSetting(ctx, "empty-on-purpose")
	if err != nil {
		t.Fatalf("GetSetting on an empty stored value returned an error: %v", err)
	}
	if got != "" {
		t.Fatalf("GetSetting = %q, want empty", got)
	}

	if err := s.SetUserPassword(ctx, "admin", ""); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}
	got, err = s.GetUserPassword(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserPassword on an empty stored value returned an error: %v", err)
	}
	if got != "" {
		t.Fatalf("GetUserPassword = %q, want empty", got)
	}
}

// GetUserPassword is not covered: t_system_user.password is NOT NULL.
func TestGettersTreatNullAsAbsent(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	cases := []struct {
		table string
		get   func() (string, error)
	}{
		{"t_system_setting", func() (string, error) { return s.GetSystemSetting(ctx, "null-on-purpose") }},
		{"t_agenthub_setting", func() (string, error) { return s.GetSetting(ctx, "null-on-purpose") }},
	}
	for _, c := range cases {
		if err := s.DB().WithContext(ctx).Exec(
			"INSERT INTO "+c.table+" (setting_key, setting_value) VALUES (?, NULL)", "null-on-purpose",
		).Error; err != nil {
			t.Fatalf("insert NULL into %s: %v", c.table, err)
		}
		got, err := c.get()
		if err != nil {
			t.Errorf("%s: NULL value returned an error: %v", c.table, err)
		}
		if got != "" {
			t.Errorf("%s: NULL value returned %q, want empty", c.table, got)
		}
	}
}
