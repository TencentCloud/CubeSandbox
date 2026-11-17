// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"reflect"
	"testing"
)

type quarantinePolicyProbe struct {
	clearErr, denyErr error
	allow, deny       bool
	calls             []string
}

func (p *quarantinePolicyProbe) CleanupTAPPolicy(uint32) error {
	p.calls = append(p.calls, "clear")
	if p.clearErr == nil {
		p.allow = false
		p.deny = false
	}
	return p.clearErr
}
func (p *quarantinePolicyProbe) InstallTAPDefaultDenyPolicy(uint32) error {
	p.calls = append(p.calls, "deny")
	if p.denyErr == nil {
		p.deny = true
	}
	return p.denyErr
}
func TestResetTAPQuarantinePolicyRejectsIncompleteIsolation(t *testing.T) {
	clearErr, denyErr := errors.New("allow map unavailable"), errors.New("deny map unavailable")
	for _, tc := range []struct {
		name        string
		clear, deny error
	}{
		{"success", nil, nil}, {"clear failure", clearErr, nil}, {"deny failure", nil, denyErr}, {"both failures", clearErr, denyErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &quarantinePolicyProbe{clearErr: tc.clear, denyErr: tc.deny, allow: true}
			err := resetTAPQuarantinePolicy(probe, 42)
			if (err == nil) != (tc.clear == nil && tc.deny == nil) {
				t.Fatalf("unexpected result: %v", err)
			}
			if tc.clear != nil && !errors.Is(err, tc.clear) {
				t.Fatalf("lost clear failure: %v", err)
			}
			if tc.deny != nil && !errors.Is(err, tc.deny) {
				t.Fatalf("lost deny failure: %v", err)
			}
			if !reflect.DeepEqual(probe.calls, []string{"clear", "deny"}) {
				t.Fatalf("calls=%v", probe.calls)
			}
			if err == nil && (probe.allow || !probe.deny) {
				t.Fatal("incomplete policy reported as quarantined")
			}
		})
	}
}
