// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestValidateTrustedProxiesAcceptsSpecificProxies(t *testing.T) {
	for _, entries := range [][]string{
		nil,
		{},
		{"127.0.0.1"},
		{"::1"},
		{"10.42.0.7", "10.42.0.8"},
		{"10.42.0.0/16"},
		{"fd00::/64"},
	} {
		if err := validateTrustedProxies(entries); err != nil {
			t.Errorf("validateTrustedProxies(%v) = %v, want nil", entries, err)
		}
	}
}

func TestValidateTrustedProxiesRejectsTrustAll(t *testing.T) {
	for _, entry := range []string{"0.0.0.0/0", "::/0", "*"} {
		err := validateTrustedProxies([]string{entry})
		if err == nil {
			t.Errorf("validateTrustedProxies(%q) = nil, want an error", entry)
			continue
		}
		if !strings.Contains(err.Error(), entry) {
			t.Errorf("error for %q does not name the entry: %v", entry, err)
		}
	}
}

func TestValidateTrustedProxiesRejectsMalformedEntries(t *testing.T) {
	for _, entry := range []string{"not-an-ip", "10.0.0.1/33", "example.com", "10.0.0.256"} {
		if err := validateTrustedProxies([]string{entry}); err == nil {
			t.Errorf("validateTrustedProxies(%q) = nil, want an error", entry)
		}
	}
}
