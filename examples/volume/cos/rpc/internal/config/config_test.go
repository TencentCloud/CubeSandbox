// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

func TestResolveAppID_ExplicitValueWins(t *testing.T) {
	c := &Config{Bucket: "my-bucket-1250000000", AppID: "9999999999"}
	got, err := c.ResolveAppID()
	if err != nil {
		t.Fatalf("ResolveAppID: %v", err)
	}
	if got != "9999999999" {
		t.Fatalf("got %q, want explicit APPID to win", got)
	}
}

func TestResolveAppID_DerivedFromBucketSuffix(t *testing.T) {
	c := &Config{Bucket: "my-bucket-1250000000"}
	got, err := c.ResolveAppID()
	if err != nil {
		t.Fatalf("ResolveAppID: %v", err)
	}
	if got != "1250000000" {
		t.Fatalf("got %q, want 1250000000", got)
	}
}

func TestResolveAppID_RejectsNonNumericSuffix(t *testing.T) {
	c := &Config{Bucket: "my-bucket-prod"}
	if _, err := c.ResolveAppID(); err == nil {
		t.Fatal("expected error for non-numeric suffix")
	}
}

func TestResolveAppID_RejectsBucketWithoutHyphen(t *testing.T) {
	c := &Config{Bucket: "mybucket"}
	if _, err := c.ResolveAppID(); err == nil {
		t.Fatal("expected error for bucket without a hyphen")
	}
}
