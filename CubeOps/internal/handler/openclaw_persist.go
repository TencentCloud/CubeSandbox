// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// This file previously held the OpenClaw host-side state directory helpers
// (shared_files persistence). They have been extracted to package service
// (internal/service/openclaw.go) so they can be unit-tested independently of
// gin. The handler package now imports them via the aliases declared in
// openclaw_apply.go; this file is intentionally left empty to preserve the
// historical file layout.
package handler
