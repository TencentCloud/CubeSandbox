// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pathutil

import (
	"testing"
)

func TestValidateSafeID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{
			name:    "valid_id",
			id:      "valid-id-123",
			wantErr: false,
		},
		{
			name:    "valid_id_with_underscores",
			id:      "valid_id_456",
			wantErr: false,
		},
		{
			name:    "empty_id",
			id:      "",
			wantErr: true,
		},
		{
			name:    "id_with_forward_slash",
			id:      "invalid/id",
			wantErr: true,
		},
		{
			name:    "id_with_backslash",
			id:      "invalid\\id",
			wantErr: true,
		},
		{
			name:    "id_with_double_dot",
			id:      "invalid..id",
			wantErr: true,
		},
		{
			name:    "id_with_traversal_prefix",
			id:      "../etc/passwd",
			wantErr: true,
		},
		{
			name:    "id_with_traversal_suffix",
			id:      "id/../../../etc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSafeID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSafeID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestValidateNoTraversal(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "valid_absolute_path",
			path:    "/usr/local/services/cubetoolbox/cube-snapshot",
			wantErr: false,
		},
		{
			name:    "valid_relative_path",
			path:    "relative/path/to/file",
			wantErr: false,
		},
		{
			name:    "empty_path",
			path:    "",
			wantErr: false,
		},
		{
			name:    "path_with_dot",
			path:    "/usr/local/./services",
			wantErr: false,
		},
		{
			name:    "path_with_traversal_resolves_clean",
			path:    "/usr/local/../../../etc/passwd",
			wantErr: false,
		},
		{
			name:    "path_with_double_dot_middle_resolves",
			path:    "/usr/local/../services",
			wantErr: false,
		},
		{
			name:    "path_with_traversal_relative",
			path:    "../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "complex_path_still_has_traversal",
			path:    "/a/b/c/./d/../../e",
			wantErr: false,
		},
		{
			name:    "path_with_orphaned_traversal",
			path:    "a/b/../../..",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNoTraversal(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateNoTraversal(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePathUnderBase(t *testing.T) {
	tests := []struct {
		name       string
		basePath   string
		inputPath  string
		wantErr    bool
		wantResult bool
	}{
		{
			name:       "valid_path_under_base",
			basePath:   "/usr/local/services",
			inputPath:  "/usr/local/services/cubetoolbox",
			wantErr:    false,
			wantResult: true,
		},
		{
			name:       "path_equals_base",
			basePath:   "/usr/local/services",
			inputPath:  "/usr/local/services",
			wantErr:    false,
			wantResult: true,
		},
		{
			name:       "path_not_under_base",
			basePath:   "/usr/local/services",
			inputPath:  "/etc/passwd",
			wantErr:    true,
			wantResult: false,
		},
		{
			name:       "traversal_escape_attempt",
			basePath:   "/usr/local/services",
			inputPath:  "/usr/local/services/../../../etc/passwd",
			wantErr:    true,
			wantResult: false,
		},
		{
			name:       "empty_input_path",
			basePath:   "/usr/local/services",
			inputPath:  "",
			wantErr:    true,
			wantResult: false,
		},
		{
			name:       "relative_input_path",
			basePath:   "/usr/local/services",
			inputPath:  "relative/path",
			wantErr:    true,
			wantResult: false,
		},
		{
			name:       "nested_path_under_base",
			basePath:   "/usr/local/services",
			inputPath:  "/usr/local/services/a/b/c/d",
			wantErr:    false,
			wantResult: true,
		},
		{
			name:       "similar_prefix_not_under",
			basePath:   "/usr/local/services",
			inputPath:  "/usr/local/services2/something",
			wantErr:    true,
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidatePathUnderBase(tt.basePath, tt.inputPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePathUnderBase(%q, %q) error = %v, wantErr %v", tt.basePath, tt.inputPath, err, tt.wantErr)
				return
			}
			if !tt.wantErr && (result == "") != !tt.wantResult {
				t.Errorf("ValidatePathUnderBase(%q, %q) got result=%q, want result to be non-empty=%v", tt.basePath, tt.inputPath, result, tt.wantResult)
			}
		})
	}
}

func TestValidateIfName(t *testing.T) {
	tests := []struct {
		name    string
		ifName  string
		wantErr bool
	}{
		{name: "ok-eth0", ifName: "eth0", wantErr: false},
		{name: "ok-vlan", ifName: "eth0.100", wantErr: false},
		{name: "ok-colon", ifName: "eth0:1", wantErr: false},
		{name: "ok-underscore", ifName: "tap_abc-1", wantErr: false},
		{name: "empty", ifName: "", wantErr: true},
		{name: "with-space", ifName: "eth 0", wantErr: true},
		{name: "with-slash", ifName: "eth0/0", wantErr: true},
		{name: "with-semicolon", ifName: "eth0;rm", wantErr: true},
		{name: "with-pipe", ifName: "eth0|cat", wantErr: true},
		{name: "with-dollar", ifName: "eth0$IFS", wantErr: true},
		{name: "with-nul", ifName: "eth0\x00", wantErr: true},
		{name: "too-long", ifName: "abcdefghijklmnopqrstuvwxyz01234567", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIfName(tt.ifName)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateIfName(%q) error = %v, wantErr %v", tt.ifName, err, tt.wantErr)
			}
		})
	}
}

func TestValidateUUID(t *testing.T) {
	tests := []struct {
		name    string
		uuid    string
		wantErr bool
	}{
		{name: "ok-standard", uuid: "550e8400-e29b-41d4-a716-446655440000", wantErr: false},
		{name: "ok-hex-only", uuid: "deadbeefcafebabe", wantErr: false},
		{name: "ok-upper", uuid: "ABCDEF0123456789", wantErr: false},
		{name: "empty", uuid: "", wantErr: true},
		{name: "with-non-hex", uuid: "g50e8400-e29b-41d4-a716-446655440000", wantErr: true},
		{name: "with-space", uuid: "550e 8400-e29b-41d4-a716-446655440000", wantErr: true},
		{name: "with-semicolon", uuid: "550e8400;rm-rf-/", wantErr: true},
		{name: "with-nul", uuid: "550e8400\x00", wantErr: true},
		{name: "too-long", uuid: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUUID(tt.uuid)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateUUID(%q) error = %v, wantErr %v", tt.uuid, err, tt.wantErr)
			}
		})
	}
}
