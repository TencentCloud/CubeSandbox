// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminalcore

import (
	"reflect"
	"testing"
)

func TestValidateOpenRequiresBoundTarget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sandboxID   string
		containerID string
	}{
		{name: "missing sandbox", containerID: "container-1"},
		{name: "missing container", sandboxID: "sandbox-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateOpen(tc.sandboxID, tc.containerID, 120, 30); err == nil {
				t.Fatal("ValidateOpen() accepted an unbound target")
			}
		})
	}
}

func TestValidateOpenRejectsUnsafeDimensions(t *testing.T) {
	for _, tc := range []struct {
		cols uint32
		rows uint32
	}{
		{cols: 0, rows: 30},
		{cols: 120, rows: 0},
		{cols: 1, rows: 30},
		{cols: 501, rows: 30},
		{cols: 120, rows: 201},
	} {
		if err := ValidateOpen("sandbox-1", "container-1", tc.cols, tc.rows); err == nil {
			t.Fatalf("ValidateOpen() accepted dimensions %dx%d", tc.cols, tc.rows)
		}
	}
}

func TestValidateOpenAcceptsSupportedDimensions(t *testing.T) {
	if err := ValidateOpen("sandbox-1", "container-1", 120, 30); err != nil {
		t.Fatalf("ValidateOpen() error = %v", err)
	}
}

func TestCommandDefaultsToBinSh(t *testing.T) {
	if got := Command(nil); !reflect.DeepEqual(got, []string{"/bin/sh"}) {
		t.Fatalf("Command(nil) = %#v", got)
	}
}

func TestMergeEnvOverridesInPlaceAndAddsTerminal(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root"}
	overrides := []string{"PATH=/opt/bin", "LANG=C.UTF-8"}
	want := []string{"PATH=/opt/bin", "HOME=/root", "LANG=C.UTF-8", "TERM=xterm-256color"}

	if got := MergeEnv(base, overrides); !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeEnv() = %#v, want %#v", got, want)
	}
}

func TestMergeEnvPreservesExplicitTermAndIgnoresMalformedEntries(t *testing.T) {
	want := []string{"TERM=screen", "PATH=/bin"}
	if got := MergeEnv([]string{"TERM=screen", "BROKEN"}, []string{"PATH=/bin", "=bad"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeEnv() = %#v, want %#v", got, want)
	}
}
