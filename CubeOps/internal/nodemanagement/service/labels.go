// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"errors"
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

// systemLabelPrefixes are namespaces owned by the data plane: Cubelet reports
// them on registration and the scheduler reads them for affinity. The admin
// label API treats them as read-only. Kept in sync with CubeMaster's
// reservedLabelNamespaces (pinned by tests).
var systemLabelPrefixes = []string{
	"kubernetes.io",
	"beta.kubernetes.io",
	"cube.cloud.tencentcloud.com",
}

// IsSystemLabelKey reports whether key belongs to a data-plane owned
// namespace. Such keys are read-only for the admin label API.
func IsSystemLabelKey(key string) bool {
	ns := key
	if prefix, _, ok := strings.Cut(key, "/"); ok {
		ns = prefix
	}
	for _, reserved := range systemLabelPrefixes {
		if ns == reserved || strings.HasSuffix(ns, "."+reserved) {
			return true
		}
	}
	return false
}

// ErrReservedLabel is returned when a caller tries to write a control-plane
// owned label through the generic label API.
var ErrReservedLabel = errors.New("label is reserved for system use")

// ErrInvalidLabel is returned for malformed keys/values or limit violations.
var ErrInvalidLabel = errors.New("invalid label")

// Label key classification. Each predicate expresses one ownership behaviour,
// so a new key only has to be classified once:
//
//	IsSystemLabelKey          data-plane namespace  -> 403 on write, kept by replace
//	IsReservedLabelKey        data-plane OR machine-type -> 403 on the generic label API
//	cubeletProtectedLabelKeys control-plane keys    -> Cubelet must not touch
//	replaceProtectedLabelKeys not part of a label mutation -> kept at stored value
//
// machine-type is operator-managed, but only through the machine type API.

// cubeletProtectedLabelKeys must survive a Cubelet registration.
var cubeletProtectedLabelKeys = map[string]struct{}{
	model.LabelSchedulingDisabled: {},
	model.LabelMachineType:        {},
}

// replaceProtectedLabelKeys survive every generic label write.
var replaceProtectedLabelKeys = map[string]struct{}{
	model.LabelSchedulingDisabled: {},
	model.LabelMachineType:        {},
}

// IsReservedLabelKey reports whether key cannot be written through the generic
// label API. Machine-type membership is changed by the machine type API.
func IsReservedLabelKey(key string) bool {
	return IsSystemLabelKey(key) || key == model.LabelMachineType
}

// IsCubeletProtectedLabelKey reports whether a Cubelet registration must leave
// the stored value untouched.
func IsCubeletProtectedLabelKey(key string) bool {
	_, ok := cubeletProtectedLabelKeys[key]
	return ok
}

// mergeLabels applies incoming user labels onto existing user labels. The
// caller (mutateNodeLabels) has already excluded protected keys from both, so
// this is a plain merge.
func mergeLabels(existing, incoming map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(incoming))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range incoming {
		out[k] = v
	}
	return out
}

// mutableLabels returns the subset of labels a given mutation may rewrite.
// Protected keys are removed here and re-attached from a fresh read inside
// mutateNodeLabels.
func mutableLabels(labels map[string]string, protected map[string]struct{}) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if _, ok := protected[k]; ok || IsSystemLabelKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// protectedLabels is the complement of mutableLabels: the values that must be
// re-attached after a mutation so a caller cannot drop them.
func protectedLabels(labels map[string]string, protected map[string]struct{}) map[string]string {
	out := make(map[string]string, len(protected))
	for k, v := range labels {
		if _, ok := protected[k]; ok || IsSystemLabelKey(k) {
			out[k] = v
		}
	}
	return out
}

var machineTypeValueRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateMachineTypeValue enforces the machine-type shape. An empty value is
// rejected: callers remove the label instead of setting "".
func ValidateMachineTypeValue(v string) error {
	if !machineTypeValueRegexp.MatchString(v) {
		return fmt.Errorf("%w: machine-type must match ^[a-z0-9][a-z0-9-]{0,62}$", ErrInvalidLabel)
	}
	return nil
}

// CountUserLabels counts the labels an operator manages through the label API.
// Reserved keys (data-plane namespaces, isolation and machine-type) do not
// consume the operator's budget.
func CountUserLabels(labels map[string]string) int {
	n := 0
	for k := range labels {
		if IsReservedLabelKey(k) {
			continue
		}
		n++
	}
	return n
}

func ValidateLabels(labels map[string]string) error {
	if len(labels) > maxLabelsPerNode {
		return fmt.Errorf("%w: label request cannot contain more than %d labels, got %d",
			ErrInvalidLabel, maxLabelsPerNode, len(labels))
	}
	for k, v := range labels {
		if err := ValidateLabelKey(k); err != nil {
			return err
		}
		if errs := validateLabelValue(v); len(errs) != 0 {
			return fmt.Errorf("%w: label value for key %q is invalid: %s",
				ErrInvalidLabel, k, strings.Join(errs, ", "))
		}
	}
	return nil
}

// maxReservedLabelsPerNode bounds the keys the operator budget excludes:
// machine-type, isolation, and the data-plane labels Cubelet reports. It is a
// guard against an unbounded labels_json, not a policy knob -- the real sets are
// a handful of fixed keys, so this leaves plenty of headroom.
const maxReservedLabelsPerNode = 32

// ValidateLabelsSkippingReserved validates user labels while exempting
// reserved keys (data plane, isolation and machine-type) kept on the register
// path.
//
// The operator budget is enforced by the caller with CountUserLabels, the same
// definition the label API uses, so a node filled with 64 operator labels plus
// reserved keys still re-registers. A total bound is still applied here, with a
// separate reserved allowance: without it a Cubelet reporting an unbounded set
// of data-plane keys could grow labels_json (and every copy of it) without
// limit.
func ValidateLabelsSkippingReserved(labels map[string]string) error {
	reserved := 0
	for k := range labels {
		if IsReservedLabelKey(k) {
			reserved++
		}
	}
	if reserved > maxReservedLabelsPerNode {
		return fmt.Errorf("%w: label request cannot contain more than %d reserved labels, got %d",
			ErrInvalidLabel, maxReservedLabelsPerNode, reserved)
	}
	for k, v := range labels {
		if IsReservedLabelKey(k) {
			continue
		}
		if err := ValidateLabelKey(k); err != nil {
			return err
		}
		if errs := validateLabelValue(v); len(errs) != 0 {
			return fmt.Errorf("%w: label value for key %q is invalid: %s",
				ErrInvalidLabel, k, strings.Join(errs, ", "))
		}
	}
	return nil
}

// ValidateLabelKey rejects a control-plane reserved key with ErrReservedLabel
// and malformed keys with ErrInvalidLabel, so handlers can map them to 403 and
// 400 respectively.
func ValidateLabelKey(key string) error {
	if IsReservedLabelKey(key) {
		return fmt.Errorf("%w: label key %q", ErrReservedLabel, key)
	}
	if errs := isQualifiedLabelKey(key); len(errs) != 0 {
		return fmt.Errorf("%w: label key %q is invalid: %s", ErrInvalidLabel, key, strings.Join(errs, ", "))
	}
	return nil
}

func isQualifiedLabelKey(key string) []string {
	var errs []string
	if key == "" {
		return append(errs, "must not be empty")
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

// StripAndPreserveSchedulingLabel merges Cubelet-reported labels into existing
// while preserving every Cubelet-protected key (isolation and machine-type) at
// its stored value.
func StripAndPreserveSchedulingLabel(existing, cubeletLabels map[string]string) map[string]string {
	if existing == nil {
		existing = map[string]string{}
	}
	for k, v := range cubeletLabels {
		if IsCubeletProtectedLabelKey(k) {
			continue
		}
		existing[k] = v
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
