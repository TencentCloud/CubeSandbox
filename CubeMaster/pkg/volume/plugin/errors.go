// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package plugin

// InvalidArgumentError reports caller input rejected by a volume plugin.
// Message must be safe to show to the caller and must not contain credentials
// or internal execution diagnostics.
type InvalidArgumentError struct {
	Message string
}

func (e *InvalidArgumentError) Error() string { return e.Message }

// NotStartedError confirms that the plugin operation was never invoked.
type NotStartedError struct {
	Cause error
}

func (e *NotStartedError) Error() string { return e.Cause.Error() }
func (e *NotStartedError) Unwrap() error { return e.Cause }
