// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxrestart formats restart-policy strings for Cubelet and
// CubeMaster. Both accept the k8s names and the on-wire RESTART_POLICY_* values.
package sandboxrestart

import "strings"

const (
	WireNever     = "RESTART_POLICY_NEVER"
	WireOnFailure = "RESTART_POLICY_ON_FAILURE"
	WireAlways    = "RESTART_POLICY_ALWAYS"

	DisplayNever     = "Never"
	DisplayOnFailure = "OnFailure"
	DisplayAlways    = "Always"
)

type policyForm struct {
	wire    string
	display string
}

func knownForms() []policyForm {
	return []policyForm{
		{wire: WireNever, display: DisplayNever},
		{wire: WireOnFailure, display: DisplayOnFailure},
		{wire: WireAlways, display: DisplayAlways},
	}
}

func match(raw string) (policyForm, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" || key == "never" || key == strings.ToLower(WireNever) {
		return policyForm{wire: WireNever, display: DisplayNever}, true
	}
	for _, form := range knownForms()[1:] {
		if key == strings.ToLower(form.display) || key == strings.ToLower(form.wire) {
			return form, true
		}
	}
	return policyForm{}, false
}

// Normalize returns the on-wire RESTART_POLICY_* value.
// An empty string is Never. Unknown text is rejected.
func Normalize(raw string) (string, bool) {
	form, ok := match(raw)
	if !ok {
		return "", false
	}
	return form.wire, true
}

// Display returns the k8s name users send and read.
// An empty string is Never. Unknown text is returned trimmed.
func Display(raw string) string {
	form, ok := match(raw)
	if !ok {
		return strings.TrimSpace(raw)
	}
	return form.display
}
