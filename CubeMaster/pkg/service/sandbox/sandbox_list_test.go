// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import "testing"

func TestParseCPUCount(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int32
	}{
		{name: "empty", input: "", want: 0},
		{name: "millicores", input: "2000m", want: 2},
		{name: "sub core", input: "500m", want: 0},
		{name: "whole cores", input: "2", want: 2},
		{name: "invalid", input: "bad", want: 0},
		{name: "leading spaces", input: " 4000m", want: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCPUCount(tt.input); got != tt.want {
				t.Fatalf("parseCPUCount(%q)=%d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseMemoryMB(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int32
	}{
		{name: "empty", input: "", want: 0},
		{name: "kibibytes", input: "1Ki", want: 1},
		{name: "mebibytes", input: "2048Mi", want: 2148},
		{name: "gibibytes", input: "2Gi", want: 2148},
		{name: "tebibytes", input: "1Ti", want: 1099512},
		{name: "gigabytes", input: "2G", want: 2000},
		{name: "megabytes", input: "512M", want: 512},
		{name: "milliunits", input: "256m", want: 1},
		{name: "plain bytes", input: "1024", want: 1},
		{name: "overflow", input: "2147483648M", want: 2147483647},
		{name: "invalid", input: "bad", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMemoryMB(tt.input); got != tt.want {
				t.Fatalf("parseMemoryMB(%q)=%d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCPUMilli(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int32
	}{
		{name: "empty", input: "", want: 0},
		{name: "millicores", input: "2000m", want: 2000},
		{name: "sub core", input: "100m", want: 100},
		{name: "whole cores", input: "2", want: 2000},
		{name: "invalid", input: "bad", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCPUMilli(tt.input); got != tt.want {
				t.Fatalf("parseCPUMilli(%q)=%d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseMemoryMiB(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int32
	}{
		{name: "empty", input: "", want: 0},
		{name: "list detail regression", input: "500Mi", want: 500},
		{name: "webui regression", input: "4096Mi", want: 4096},
		{name: "gibibytes", input: "2Gi", want: 2048},
		{name: "gigabytes", input: "2G", want: 1908},
		{name: "plain bytes round up", input: "1024", want: 1},
		{name: "overflow", input: "2147483648Mi", want: 2147483647},
		{name: "invalid", input: "bad", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMemoryMiB(tt.input); got != tt.want {
				t.Fatalf("parseMemoryMiB(%q)=%d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
