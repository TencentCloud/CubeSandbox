// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package binding defines the private_data contract used by the S3 example
// plugin.
//
// Create receives caller input A: a JSON object naming an existing bucket, the
// object prefix that becomes the volume root, and a required credential
// reference. The plugin validates and normalizes it, provisions backend
// storage, and returns state B: a JSON object with the resolved bucket/prefix
// and the credential reference. Only B is persisted and later passed to
// Attach and Destroy.
//
// A volume created without input A keeps the legacy layout: a folder under the
// configured bucket at volumes/<volume_id>/, and a plain prefix string as B.
// Older plugins only know that shape, so legacy volumes must keep mounting
// after an upgrade.
package binding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// Version is the state schema version written into B.
	Version = 1

	// MaxPrivateDataLen is what CubeAPI and CubeMaster enforce; the plugin
	// repeats the check so it also holds when the binary is called by hand.
	MaxPrivateDataLen = 1024

	// DefaultPrefixRoot is the legacy layout: volumes/<volume_id>/.
	DefaultPrefixRoot = "volumes/"

	// ReservedPrefix is the plugin's own metadata namespace. A volume prefix
	// must never live inside it.
	ReservedPrefix = ".cube-sandbox/"

	maxPrefixLen = 512
	maxBucketLen = 255
	maxVolumeID  = 128
)

// Create is the caller input A after validation.
type Create struct {
	Bucket  string
	Prefix  string // normalized, always ends with "/"
	CredRef string
	// Dynamic reports whether the caller supplied storage fields. Legacy
	// volumes keep the plain prefix state shape for older Cubelet builds.
	Dynamic bool
}

// Binding is the persisted state B after validation.
type Binding struct {
	Bucket  string
	Prefix  string
	CredRef string
	// Dynamic is false for legacy plain-prefix state.
	Dynamic bool
}

type createInput struct {
	Bucket  string `json:"bucket"`
	Prefix  string `json:"prefix"`
	CredRef string `json:"cred_ref"`
}

type stateJSON struct {
	Version int    `json:"v"`
	Bucket  string `json:"bucket"`
	Prefix  string `json:"prefix"`
	CredRef string `json:"cred_ref"`
}

// ParseCreate validates caller input A. An empty or object-with-no-fields
// value means "legacy defaults", so existing callers keep their old binding.
func ParseCreate(raw, volumeID string) (Create, error) {
	if err := ValidateVolumeID(volumeID); err != nil {
		return Create{}, err
	}
	if err := validateRaw(raw); err != nil {
		return Create{}, err
	}

	out := Create{Prefix: DefaultPrefix(volumeID)}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return out, nil
	}

	var in createInput
	if err := json.Unmarshal([]byte(trimmed), &in); err != nil {
		return Create{}, errors.New("private_data must be a JSON object with bucket, prefix and cred_ref")
	}
	if in.Bucket == "" && in.Prefix == "" && in.CredRef == "" {
		return out, nil
	}
	if err := ValidateCredRef(in.CredRef); err != nil {
		return Create{}, fmt.Errorf("dynamic private_data requires cred_ref: %w", err)
	}

	if in.Bucket != "" {
		if err := ValidateBucket(in.Bucket); err != nil {
			return Create{}, err
		}
		out.Bucket = in.Bucket
	}
	if in.Prefix != "" {
		prefix, err := NormalizePrefix(in.Prefix)
		if err != nil {
			return Create{}, err
		}
		out.Prefix = prefix
	}
	out.CredRef = in.CredRef
	out.Dynamic = true
	return out, nil
}

// ParseState validates persisted state B. It accepts both the JSON state this
// plugin writes and the plain "volumes/<id>/" string written by older builds.
func ParseState(raw, volumeID string) (Binding, error) {
	if err := ValidateVolumeID(volumeID); err != nil {
		return Binding{}, err
	}
	if err := validateRaw(raw); err != nil {
		return Binding{}, err
	}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Binding{Prefix: DefaultPrefix(volumeID)}, nil
	}

	switch trimmed[0] {
	case '{':
		var st stateJSON
		if err := json.Unmarshal([]byte(trimmed), &st); err != nil {
			return Binding{}, errors.New("private_data is not a recognized volume state")
		}
		if st.Version != Version {
			return Binding{}, fmt.Errorf("private_data uses unsupported state version %d", st.Version)
		}
		if st.Bucket == "" || st.Prefix == "" {
			return Binding{}, errors.New("private_data is missing bucket or prefix")
		}
		if err := ValidateBucket(st.Bucket); err != nil {
			return Binding{}, err
		}
		prefix, err := NormalizeStatePrefix(st.Prefix, volumeID)
		if err != nil {
			return Binding{}, err
		}
		if err := ValidateCredRef(st.CredRef); err != nil {
			return Binding{}, fmt.Errorf("dynamic private_data requires cred_ref: %w", err)
		}
		return Binding{Bucket: st.Bucket, Prefix: prefix, CredRef: st.CredRef, Dynamic: true}, nil
	case '"':
		// Tolerate a JSON-encoded legacy prefix, should a caller ever store one.
		var prefix string
		if err := json.Unmarshal([]byte(trimmed), &prefix); err != nil {
			return Binding{}, errors.New("private_data is not a recognized volume state")
		}
		prefix, err := normalizePrefix(prefix, true)
		if err != nil {
			return Binding{}, err
		}
		if prefix != DefaultPrefix(volumeID) {
			return Binding{}, errors.New("legacy private_data prefix must match this volume ID")
		}
		return Binding{Prefix: prefix}, nil
	default:
		// Legacy plugin state: the bare prefix, e.g. "volumes/vol-123/".
		prefix, err := normalizePrefix(trimmed, true)
		if err != nil {
			return Binding{}, fmt.Errorf("private_data is not a recognized volume state: %w", err)
		}
		if prefix != DefaultPrefix(volumeID) {
			return Binding{}, errors.New("legacy private_data prefix must match this volume ID")
		}
		return Binding{Prefix: prefix}, nil
	}
}

// EncodeState serializes state B and enforces the API limit.
func EncodeState(b Binding) (string, error) {
	if err := ValidateCredRef(b.CredRef); err != nil {
		return "", fmt.Errorf("dynamic private_data requires cred_ref: %w", err)
	}
	encoded, err := json.Marshal(stateJSON{
		Version: Version,
		Bucket:  b.Bucket,
		Prefix:  b.Prefix,
		CredRef: b.CredRef,
	})
	if err != nil {
		return "", fmt.Errorf("encode private_data: %w", err)
	}
	// Choose a JSON representation compatible with existing utf8mb3 DB
	// connections. Only supplementary runes need escaping; JSON decoding in
	// Attach/Destroy restores the exact original prefix. The core still stores
	// B opaquely, and the final representation must fit its byte limit.
	var escaped bytes.Buffer
	for _, r := range string(encoded) {
		if r > 0xffff {
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&escaped, `\u%04x\u%04x`, hi, lo)
		} else {
			escaped.WriteRune(r)
		}
	}
	encoded = escaped.Bytes()
	if len(encoded) > MaxPrivateDataLen {
		return "", fmt.Errorf("resolved private_data is %d bytes, over the %d-byte limit", len(encoded), MaxPrivateDataLen)
	}
	return string(encoded), nil
}

// DefaultPrefix is the legacy per-volume key prefix.
func DefaultPrefix(volumeID string) string {
	return DefaultPrefixRoot + volumeID + "/"
}

// NormalizePrefix validates a caller-supplied object prefix and returns it
// with one trailing slash. Leading slashes are stripped; "."/".." and empty
// segments are rejected so a later recursive delete can never escape the
// assigned prefix. Plugin-managed namespaces are refused.
func NormalizePrefix(raw string) (string, error) {
	return normalizePrefix(raw, false)
}

// NormalizeStatePrefix validates a prefix from persisted state B.
//
// A dynamic create that omits prefix falls back to the legacy layout
// volumes/<volume_id>/. That exact prefix must therefore round-trip through
// EncodeState and ParseState, although callers may not choose any other prefix
// in the plugin-managed volumes/ namespace.
func NormalizeStatePrefix(raw, volumeID string) (string, error) {
	prefix, err := NormalizePrefix(raw)
	if err == nil {
		return prefix, nil
	}
	if allowed, allowErr := normalizePrefix(raw, true); allowErr == nil && allowed == DefaultPrefix(volumeID) {
		return allowed, nil
	}
	return "", err
}

// normalizePrefix is the shared implementation. allowPluginNamespace is set
// only when validating the legacy plain-prefix state written by older plugin
// builds, which legitimately lives under volumes/.
func normalizePrefix(raw string, allowPluginNamespace bool) (string, error) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" {
		return "", errors.New("prefix is empty")
	}
	prefix = strings.TrimLeft(prefix, "/")
	if prefix == "" {
		return "", errors.New("prefix is empty")
	}
	if strings.Contains(prefix, "\\") {
		return "", errors.New("prefix must not contain a backslash")
	}
	for _, r := range prefix {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("prefix must not contain control characters")
		}
	}

	trimmed := strings.TrimSuffix(prefix, "/")
	if trimmed == "" {
		return "", errors.New("prefix is empty")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		switch segment {
		case "":
			return "", errors.New("prefix must not contain an empty path segment")
		case ".", "..":
			return "", fmt.Errorf("prefix must not contain %q", segment)
		}
	}

	prefix = trimmed + "/"
	if len(prefix) > maxPrefixLen {
		return "", fmt.Errorf("prefix is longer than %d bytes", maxPrefixLen)
	}
	if allowPluginNamespace {
		return prefix, nil
	}
	if strings.HasPrefix(prefix, ReservedPrefix) {
		return "", fmt.Errorf("prefix must not start with the reserved %q namespace", ReservedPrefix)
	}
	// volumes/ is the legacy layout managed by the plugin itself. Dynamic
	// prefixes stay out of it so a legacy volume and a dynamic volume can
	// never be nested inside each other.
	if strings.HasPrefix(prefix, DefaultPrefixRoot) {
		return "", fmt.Errorf("prefix must not start with the plugin-managed %q namespace", DefaultPrefixRoot)
	}
	return prefix, nil
}

// ValidateBucket rejects names that cannot safely be sent to an S3 endpoint.
// The backend still decides whether a custom naming convention is acceptable.
func ValidateBucket(bucket string) error {
	if bucket == "" {
		return errors.New("bucket is empty")
	}
	if len(bucket) > maxBucketLen {
		return fmt.Errorf("bucket is longer than %d bytes", maxBucketLen)
	}
	if len(bucket) < 3 {
		return errors.New("bucket must be at least 3 characters")
	}
	for _, r := range bucket {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("bucket %q contains an unsupported character", bucket)
		}
	}
	return nil
}

// ValidateCredRef accepts only a single safe path component: the reference is
// resolved to a file inside the credentials directory.
func ValidateCredRef(ref string) error {
	if ref == "" || len(ref) > 64 {
		return errors.New("credential reference must be 1 to 64 characters")
	}
	if ref == "." || ref == ".." || strings.Contains(ref, "..") {
		return errors.New("credential reference must not contain ..")
	}
	for i, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case (r == '.' || r == '-' || r == '_') && i > 0:
		default:
			return fmt.Errorf("credential reference %q contains an unsupported character", ref)
		}
	}
	return nil
}

// ValidateVolumeID keeps the id safe for file names and mount paths.
func ValidateVolumeID(id string) error {
	if id == "" {
		return errors.New("volume id is empty")
	}
	if len(id) > maxVolumeID {
		return fmt.Errorf("volume id is longer than %d bytes", maxVolumeID)
	}
	if id == "." || id == ".." {
		return errors.New("volume id must not be . or ..")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("volume id %q contains an unsupported character", id)
		}
	}
	return nil
}

func validateRaw(raw string) error {
	if len(raw) > MaxPrivateDataLen {
		return fmt.Errorf("private_data is %d bytes, over the %d-byte limit", len(raw), MaxPrivateDataLen)
	}
	if !utf8.ValidString(raw) {
		return errors.New("private_data is not valid UTF-8")
	}
	if strings.ContainsRune(raw, 0) {
		return errors.New("private_data must not contain NUL bytes")
	}
	return nil
}
