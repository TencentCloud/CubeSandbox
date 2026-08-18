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
