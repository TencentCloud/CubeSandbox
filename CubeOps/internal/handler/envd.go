// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// This file previously held the envd Connect-protocol client (command
// execution inside a sandbox). It has been extracted to package service
// (internal/service/openclaw.go) so the envd client can be unit-tested
// independently of gin. The handler package now imports it via the aliases
// declared in openclaw_apply.go; this file is intentionally left empty to
// preserve the historical file layout.
package handler
