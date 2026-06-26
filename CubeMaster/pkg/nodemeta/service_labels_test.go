// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestValidateNodeLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		wantErr string
	}{
		{
			name:   "empty map is valid",
			labels: map[string]string{},
		},
		{
			name:   "nil map is valid",
			labels: nil,
		},
		{
			name:    "empty key rejected",
			labels:  map[string]string{"": "val"},
			wantErr: "must not be empty",
		},
		// -- reserved namespace prefix tests --
		{
			name:    "kubernetes.io prefix reserved",
			labels:  map[string]string{"kubernetes.io/os": "linux"},
			wantErr: "is reserved for system use",
		},
		{
			name:    "kubernetes.io hostname reserved",
			labels:  map[string]string{"kubernetes.io/hostname": "node-1"},
			wantErr: "is reserved for system use",
		},
		{
			name:    "beta.kubernetes.io prefix reserved",
			labels:  map[string]string{"beta.kubernetes.io/arch": "amd64"},
			wantErr: "is reserved for system use",
		},
		{
			name:    "subdomain of kubernetes.io reserved",
			labels:  map[string]string{"topology.kubernetes.io/zone": "us-west"},
			wantErr: "is reserved for system use",
		},
		{
			name:    "subdomain of beta.kubernetes.io reserved",
			labels:  map[string]string{"failure-domain.beta.kubernetes.io/zone": "us-west"},
			wantErr: "is reserved for system use",
		},
		{
			name:    "cube.cloud.tencentcloud.com prefix reserved",
			labels:  map[string]string{"cube.cloud.tencentcloud.com/instance-type": "cubebox"},
			wantErr: "is reserved for system use",
		},
		// -- previously-enumerated canonical keys still blocked --
		{
			name:    "AffinityKeyZone still reserved",
			labels:  map[string]string{constants.AffinityKeyZone: "us-west"},
			wantErr: "is reserved for system use",
		},
		{
			name:    "AffinityKeyCPUType still reserved",
			labels:  map[string]string{constants.AffinityKeyCPUType: "intel"},
			wantErr: "is reserved for system use",
		},
		// -- valid keys --
		{
			name:   "simple non-reserved key accepted",
			labels: map[string]string{"env": "production"},
		},
		{
			name:   "non-reserved domain prefix accepted",
			labels: map[string]string{"example.com/env": "production"},
		},
		{
			name:   "empty value is valid",
			labels: map[string]string{"env": ""},
		},
		// -- key format tests --
		{
			name:    "too many slashes rejected",
			labels:  map[string]string{"a/b/c": "val"},
			wantErr: "must be in the form prefix/name or name",
		},
		{
			name:    "empty prefix rejected",
			labels:  map[string]string{"/name": "val"},
			wantErr: "prefix part must not be empty",
		},
		{
			name:    "empty name rejected",
			labels:  map[string]string{"prefix/": "val"},
			wantErr: "name part must not be empty",
		},
		{
			name:    "name too long rejected",
			labels:  map[string]string{strings.Repeat("a", 64): "val"},
			wantErr: "name part must be no more than 63 characters",
		},
		{
			name:    "prefix too long rejected",
			labels:  map[string]string{strings.Repeat("a.", 127) + "a/name": "val"},
			wantErr: "prefix part must be no more than 253 characters",
		},
		{
			name:    "invalid name chars rejected",
			labels:  map[string]string{"name@bad": "val"},
			wantErr: "name part a qualified name must consist of",
		},
		{
			name:    "name starts with dash rejected",
			labels:  map[string]string{"-name": "val"},
			wantErr: "name part a qualified name must consist of",
		},
		{
			name:    "name ends with dash rejected",
			labels:  map[string]string{"name-": "val"},
			wantErr: "name part a qualified name must consist of",
		},
		{
			name:    "invalid prefix format rejected",
			labels:  map[string]string{"EXAMPLE.COM/name": "val"},
			wantErr: "prefix part a DNS-1123 subdomain must consist of",
		},
		// -- value format tests --
		{
			name:    "value too long rejected",
			labels:  map[string]string{"env": strings.Repeat("a", 64)},
			wantErr: "must be no more than 63 characters",
		},
		{
			name:    "invalid value chars rejected",
			labels:  map[string]string{"env": "bad@val"},
			wantErr: "a qualified name must consist of",
		},
		{
			name:   "value with dots and dashes valid",
			labels: map[string]string{"env": "prod-west.zone1"},
		},
		{
			name: "too many labels rejected",
			labels: func() map[string]string {
				m := make(map[string]string, 65)
				for i := 0; i < 65; i++ {
					m[fmt.Sprintf("k%d", i)] = "v"
				}
				return m
			}(),
			wantErr: "label update request cannot contain more than 64 labels",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNodeLabels(tt.labels)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateNodeLabelKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{
			name:    "empty key rejected",
			key:     "",
			wantErr: "must not be empty",
		},
		{
			name:    "too many slashes rejected",
			key:     "a//b/c/",
			wantErr: "must be in the form prefix/name or name",
		},
		{
			name:    "slash only rejected",
			key:     "//",
			wantErr: "must be in the form prefix/name or name",
		},
		{
			name:    "invalid character rejected",
			key:     "name@bad",
			wantErr: "qualified name must consist of",
		},
		{
			name:    "reserved key rejected",
			key:     "kubernetes.io/os",
			wantErr: "is reserved for system use",
		},
		{
			name: "simple key accepted",
			key:  "env",
		},
		{
			name: "prefixed key accepted",
			key:  "example.com/env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNodeLabelKey(tt.key)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestParseLabelsJSON(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr string
	}{
		{
			name: "empty string becomes empty map",
			raw:  "",
			want: map[string]string{},
		},
		{
			name: "whitespace becomes empty map",
			raw:  "   \n\t",
			want: map[string]string{},
		},
		{
			name: "json null becomes empty map",
			raw:  "null",
			want: map[string]string{},
		},
		{
			name: "valid labels",
			raw:  `{"env":"prod","zone":"ap-guangzhou"}`,
			want: map[string]string{"env": "prod", "zone": "ap-guangzhou"},
		},
		{
			name:    "invalid json returns error",
			raw:     "{",
			wantErr: "unexpected end of JSON input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLabelsJSON(tt.raw)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestIsReservedLabelKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		// Direct namespace matches
		{"kubernetes.io/os", true},
		{"beta.kubernetes.io/arch", true},
		{"cube.cloud.tencentcloud.com/instance-type", true},

		// Subdomain suffix matches
		{"topology.kubernetes.io/zone", true},
		{"failure-domain.beta.kubernetes.io/region", true},
		{"node.kubernetes.io/instance-type", true},

		// Canonical keys from constants (they all have kubernetes.io prefix)
		{constants.AffinityKeyCPUType, true},
		{constants.AffinityKeyZone, true},
		{constants.AffinityKeyClusterID, true},
		{constants.AffinityKeyMemorySize, true},
		{constants.AffinityKeyCPUCores, true},
		{constants.AffinityKeyInstanceType, true},

		// Removed from reserved list — no longer reserved
		{"k8s.io/foo", false},
		{"node.k8s.io/bar", false},

		// Non-reserved keys
		{"env", false},
		{"example.com/env", false},
		{"my-custom-label", false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			assert.Equal(t, tt.want, config.IsReservedLabelKey(tt.key))
		})
	}
}
