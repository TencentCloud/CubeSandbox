// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package blobstore

import (
	"path"
	"strings"
	"unicode"
)

// ValidateKey rejects keys that could escape a filesystem mapping. Allowed
// characters are ASCII letters, digits, '.', '_', '-' and '/'. Segments
// cannot be empty, cannot be "." / "..", and cannot start with '.'.
func ValidateKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return ErrInvalidKey
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return ErrInvalidKey
	}
	return validateSegments(key)
}

// ValidatePrefix is ValidateKey plus the empty prefix and a possibly
// incomplete last segment (S3-style string prefixes: "amd" matches "amd64").
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasPrefix(prefix, "/") {
		return ErrInvalidKey
	}
	if strings.HasSuffix(prefix, "/") {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix == "" {
			return ErrInvalidKey
		}
	}
	return validateSegments(prefix)
}

func validateSegments(key string) error {
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidKey
		}
		if strings.HasPrefix(seg, ".") {
			return ErrInvalidKey
		}
		if !segmentCharsOK(seg) {
			return ErrInvalidKey
		}
	}
	return nil
}

func segmentCharsOK(seg string) bool {
	for _, r := range seg {
		if r > unicode.MaxASCII {
			return false
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '.', '_', '-':
			continue
		default:
			return false
		}
	}
	return true
}

// JoinKey concatenates prefix and key with a single slash. Either side may
// be empty. It does not validate the result.
func JoinKey(prefix, key string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	key = strings.Trim(strings.TrimSpace(key), "/")
	switch {
	case prefix == "":
		return key
	case key == "":
		return prefix
	default:
		return prefix + "/" + key
	}
}

// ArtifactExt4Key is the object key CubeTemplateCenter and CubeMaster share
// for a rootfs artifact: [prefix/]<artifactID>.ext4.
func ArtifactExt4Key(prefix, artifactID string) string {
	id := strings.TrimSpace(artifactID)
	if id == "" {
		return ""
	}
	return JoinKey(prefix, id+".ext4")
}

const (
	// LocatorScheme is the URL scheme for a store-relative object locator
	// (blobstore:<driver>:<fullKey>) used when SignedGetURL is unsupported.
	LocatorScheme = "blobstore"

	locatorPrefix = LocatorScheme + ":"
)

// ObjectLocator builds blobstore:<driver>:<fullKey>. Empty inputs yield "".
func ObjectLocator(driver, fullKey string) string {
	driver = strings.TrimSpace(driver)
	fullKey = strings.TrimSpace(fullKey)
	if driver == "" || fullKey == "" {
		return ""
	}
	return locatorPrefix + driver + ":" + fullKey
}

// IsObjectLocator reports whether raw is a blobstore: locator, not an HTTP URL.
func IsObjectLocator(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), locatorPrefix)
}

// BaseName returns the last path element of a key.
func BaseName(key string) string {
	return path.Base(key)
}
