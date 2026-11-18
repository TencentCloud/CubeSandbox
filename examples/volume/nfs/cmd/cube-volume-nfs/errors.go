// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import "errors"

type invalidArgumentError struct {
	message string
	cause   error
}

func (e *invalidArgumentError) Error() string { return e.message }
func (e *invalidArgumentError) Unwrap() error { return e.cause }

func errorResult(op string, err error) result {
	var invalid *invalidArgumentError
	if errors.As(err, &invalid) {
		return result{Error: invalid.message, ErrorKind: "invalid_argument"}
	}
	// Internal paths and helper output belong in stderr, not in API responses.
	switch op {
	case "create":
		return result{Error: "NFS volume registration failed; check the plugin logs"}
	case "attach":
		return result{Error: "NFS volume attach failed; check the plugin logs"}
	case "detach":
		return result{Error: "NFS volume detach failed; check the plugin logs"}
	case "destroy":
		return result{Error: "NFS volume unregistration failed; check the plugin logs"}
	default:
		return result{Error: "unsupported NFS volume operation"}
	}
}
