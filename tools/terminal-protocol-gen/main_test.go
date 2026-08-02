// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestValidateRejectsAmbiguousManifestValues(t *testing.T) {
	valid := testManifest()
	tests := []struct {
		name   string
		mutate func(*manifest)
	}{
		{name: "channel name", mutate: func(spec *manifest) { spec.Channels[1].Name = spec.Channels[0].Name }},
		{name: "channel ts name", mutate: func(spec *manifest) { spec.Channels[1].TSName = spec.Channels[0].TSName }},
		{name: "channel value", mutate: func(spec *manifest) { spec.Channels[1].Value = spec.Channels[0].Value }},
		{name: "error value", mutate: func(spec *manifest) { spec.ErrorCodes[1].Value = spec.ErrorCodes[0].Value }},
		{name: "close name", mutate: func(spec *manifest) { spec.CloseReasons[1].Name = spec.CloseReasons[0].Name }},
		{name: "reason value", mutate: func(spec *manifest) { spec.ReasonCodes[1] = spec.ReasonCodes[0] }},
		{name: "missing error reason", mutate: func(spec *manifest) { spec.ReasonCodes = spec.ReasonCodes[1:] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			spec.Channels = append([]channel(nil), valid.Channels...)
			spec.ErrorCodes = append([]code(nil), valid.ErrorCodes...)
			spec.CloseReasons = append([]code(nil), valid.CloseReasons...)
			spec.ReasonCodes = append([]string(nil), valid.ReasonCodes...)
			test.mutate(&spec)
			if err := validate(spec); err == nil {
				t.Fatal("validate accepted an ambiguous manifest")
			}
		})
	}
}

func TestQuoteTSEscapesStringSyntax(t *testing.T) {
	quoted := quoteTS("quote' slash\\ line\n")
	for _, escaped := range []string{`\'`, `\\`, `\n`} {
		if !strings.Contains(quoted, escaped) {
			t.Fatalf("quoteTS result %q does not contain %q", quoted, escaped)
		}
	}
}

func testManifest() manifest {
	return manifest{
		Subprotocol: "cube-terminal.v1",
		GrantPrefix: "cube-grant.",
		Channels: []channel{
			{Name: "Stdin", TSName: "stdin", Value: 0},
			{Name: "Stdout", TSName: "stdout", Value: 1},
		},
		Limits:       limits{MaxPayloadBytes: 1, MaxStatusBytes: 1, MinDimension: 1, MaxDimension: 2},
		ErrorCodes:   []code{{Name: "Internal", Value: "INTERNAL"}, {Name: "ProtocolError", Value: "PROTOCOL_ERROR"}},
		CloseReasons: []code{{Name: "UserClosed", Value: "USER_CLOSED"}, {Name: "Internal", Value: "INTERNAL"}},
		ReasonCodes:  []string{"INTERNAL", "PROTOCOL_ERROR", "USER_CLOSED"},
	}
}
