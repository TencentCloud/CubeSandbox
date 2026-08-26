// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

const (
	qualifiedNameMaxLength    = 63
	dns1123SubdomainMaxLength = 253
	maxLabelsPerNode          = 64
	qualifiedNameFmt          = `([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]`
	dns1123SubdomainFmt       = `[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*`
	qualifiedNameErrMsg       = "must consist of alphanumeric characters, '-', '_' or '.', and start/end with alphanumeric"
	dns1123SubdomainErrMsg    = "must consist of lower case alphanumeric characters, '-' or '.', and start/end with alphanumeric"
)

var (
	qualifiedNameRegexp    = regexp.MustCompile("^" + qualifiedNameFmt + "$")
	dns1123SubdomainRegexp = regexp.MustCompile("^" + dns1123SubdomainFmt + "$")
)

var reservedLabelKeys = map[string]struct{}{
	model.LabelSchedulingDisabled: {},
}

func IsReservedLabelKey(key string) bool {
	_, ok := reservedLabelKeys[key]
	return ok
}

func CountUserLabels(labels map[string]string) int {
	n := len(labels)
	if _, ok := labels[model.LabelSchedulingDisabled]; ok {
		n--
	}
	return n
}

func ValidateLabels(labels map[string]string) error {
	if len(labels) > maxLabelsPerNode {
		return fmt.Errorf("label request cannot contain more than %d labels, got %d", maxLabelsPerNode, len(labels))
	}
	for k, v := range labels {
		if err := ValidateLabelKey(k); err != nil {
			return err
		}
		if errs := validateLabelValue(v); len(errs) != 0 {
			return fmt.Errorf("label value for key %q is invalid: %s", k, strings.Join(errs, ", "))
		}
	}
	return nil
}

// ValidateLabelsSkippingReserved validates user labels while exempting
// control-plane reserved labels (scheduling-disabled) kept on the register path.
func ValidateLabelsSkippingReserved(labels map[string]string) error {
	if len(labels) > maxLabelsPerNode {
		return fmt.Errorf("label request cannot contain more than %d labels, got %d", maxLabelsPerNode, len(labels))
	}
	for k, v := range labels {
		if IsReservedLabelKey(k) {
			continue
		}
		if err := ValidateLabelKey(k); err != nil {
			return err
		}
		if errs := validateLabelValue(v); len(errs) != 0 {
			return fmt.Errorf("label value for key %q is invalid: %s", k, strings.Join(errs, ", "))
		}
	}
	return nil
}

func ValidateLabelKey(key string) error {
	if errs := isQualifiedLabelKey(key); len(errs) != 0 {
		return fmt.Errorf("label key %q is invalid: %s", key, strings.Join(errs, ", "))
	}
	return nil
}

func isQualifiedLabelKey(key string) []string {
	var errs []string
	if key == "" {
		return append(errs, "must not be empty")
	}
	if IsReservedLabelKey(key) {
		return append(errs, "is reserved for system use")
	}
	parts := strings.Split(key, "/")
	var name string
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		prefix := parts[0]
		name = parts[1]
		if prefix == "" {
			errs = append(errs, "prefix part must not be empty")
		} else if len(prefix) > dns1123SubdomainMaxLength {
			errs = append(errs, fmt.Sprintf("prefix part must be no more than %d characters", dns1123SubdomainMaxLength))
		} else if !dns1123SubdomainRegexp.MatchString(prefix) {
			errs = append(errs, "prefix part "+dns1123SubdomainErrMsg)
		}
	default:
		return append(errs, "must be in the form prefix/name or name")
	}
	if name == "" {
		errs = append(errs, "name part must not be empty")
	} else if len(name) > qualifiedNameMaxLength {
		errs = append(errs, fmt.Sprintf("name part must be no more than %d characters", qualifiedNameMaxLength))
	} else if !qualifiedNameRegexp.MatchString(name) {
		errs = append(errs, "name part "+qualifiedNameErrMsg)
	}
	return errs
}

func validateLabelValue(value string) []string {
	var errs []string
	if value == "" {
		return errs
	}
	if len(value) > qualifiedNameMaxLength {
		errs = append(errs, fmt.Sprintf("must be no more than %d characters", qualifiedNameMaxLength))
	}
	if !qualifiedNameRegexp.MatchString(value) {
		errs = append(errs, qualifiedNameErrMsg)
	}
	return errs
}

func StripAndPreserveSchedulingLabel(existing, cubeletLabels map[string]string) map[string]string {
	ctrlVal, hasCtrl := existing[model.LabelSchedulingDisabled]
	for k, v := range cubeletLabels {
		if k == model.LabelSchedulingDisabled {
			continue
		}
		existing[k] = v
	}
	if hasCtrl {
		existing[model.LabelSchedulingDisabled] = ctrlVal
	} else {
		delete(existing, model.LabelSchedulingDisabled)
	}
	return existing
}

func DecodeSchedulingDisabled(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	_, ok := labels[model.LabelSchedulingDisabled]
	return ok
}
