// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package handler contains gin HTTP handlers for CubeOps.
//
// Handlers in this package are thin adapters — they decode the request,
// delegate to a service, and serialise the result. Business logic lives in
// the service package (or in dedicated helpers that are themselves easy to
// unit-test) so the gin machinery doesn't get in the way of tests.
//
// Helpers that span multiple handlers (response shaping, query-param
// helpers, etc.) are exposed by the httputil package.
package handler
