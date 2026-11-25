// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package blobstore

// Config captures the data every driver needs. Concrete drivers read
// additional knobs out of Extra. The shape is stable; new drivers extend
// via Extra rather than by growing required fields.
type Config struct {
	// Driver selects a registered driver: "s3", "fs", "memory".
	Driver string
	// Namespace is the bucket (s3) or the root directory (fs).
	Namespace string
	// Prefix is prepended to every key so several tenants can share one
	// namespace: "template-artifacts", "warehouse", ...
	Prefix string
	// Retention is declarative and enforced in GC (fs and memory expire
	// objects and clean partial state; s3 only uses AbortIncompleteAfter in
	// GC). Drivers never write or overwrite bucket-level lifecycle rules.
	Retention []RetentionRule
	// Extra carries driver-specific settings.
	Extra map[string]string
}
