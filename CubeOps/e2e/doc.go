// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package e2e holds end-to-end tests that drive the real cubeops binary against
// a real database over HTTP.
//
// Every test file in this package carries the e2e build tag, so the default
// unit gate (go test ./...) compiles this file and reports "no test files"
// instead of failing on a package with no buildable sources. Run the suite with:
//
//	go test -tags e2e ./e2e/...
package e2e
