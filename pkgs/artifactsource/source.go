// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package artifactsource defines the source values used in rootfs artifact
// download annotations shared by CubeMaster and Cubelet.
package artifactsource

// Source is the source identifier carried in the
// cube.master.rootfs.artifact.source annotation.
type Source string

const (
	// S3 is an object-store presigned URL. Authorization is carried by the URL.
	S3 Source = "s3"
	// TemplateCenter is the CubeTemplateCenter direct-download endpoint.
	TemplateCenter Source = "tc"
	// CubeMaster is the CubeMaster direct-download endpoint.
	CubeMaster Source = "master"
	// Legacy is the backward-compatible CubeMaster route using the artifact's
	// long-lived token. Legacy sources do not use the direct_url annotation.
	Legacy Source = "legacy"
)

// Valid reports whether s is a source identifier understood by this version.
func (s Source) Valid() bool {
	switch s {
	case S3, TemplateCenter, CubeMaster, Legacy:
		return true
	default:
		return false
	}
}

// RequiresIntegrityCheck reports whether a source requires sha256, size, and
// expiry metadata before Cubelet starts downloading it.
func (s Source) RequiresIntegrityCheck() bool {
	return s == S3 || s == TemplateCenter || s == CubeMaster
}

// RequiresToken reports whether a source requires a short-lived HTTP
// capability token. S3 presigned URLs carry their own authorization.
func (s Source) RequiresToken() bool {
	return s == TemplateCenter || s == CubeMaster
}
