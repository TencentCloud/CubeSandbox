// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// imageRefAllowedPattern is the strict character whitelist for image
// references. It permits exactly the characters that appear in legitimate
// registry/repository[:tag][@algo:hexdigest] references.
var imageRefAllowedPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

// ValidateImageRef guards every external image consumer against argument
// injection and rejects syntactically invalid Docker/OCI references.
//
// The optional docker:// transport is accepted for compatibility with skopeo,
// but it is not passed to the semantic parser.
func ValidateImageRef(imageRef string) error {
	rawRef := strings.TrimPrefix(imageRef, "docker://")
	if rawRef == "" {
		return errors.New("empty image reference")
	}
	if strings.HasPrefix(rawRef, "docker://") {
		return fmt.Errorf("invalid image reference: %s", imageRef)
	}
	if strings.HasPrefix(rawRef, "-") || !imageRefAllowedPattern.MatchString(rawRef) {
		return fmt.Errorf("invalid image reference: %s", imageRef)
	}
	if _, err := name.ParseReference(rawRef); err != nil {
		return fmt.Errorf("invalid image reference %q: %w", imageRef, err)
	}
	return nil
}

func skopeoDockerImageRef(imageRef string) string {
	if strings.HasPrefix(imageRef, "docker://") {
		return imageRef
	}
	return "docker://" + imageRef
}

func ociLayoutImageRef(ociDir, imageRef string) string {
	tag := imageTagFromRef(imageRef)
	if tag == "" {
		return ociDir
	}
	return ociDir + ":" + tag
}

// splitImageRef strips the docker:// transport prefix and any @digest suffix,
// then splits the remainder into the repository name and the tag. The final
// ":" is only treated as a tag separator when it appears after the last "/", so
// a registry port (e.g. registry:5000/image) is not mistaken for a tag. When
// the reference carries no tag, name is the whole remainder and tag is empty.
func splitImageRef(imageRef string) (name, tag string) {
	imageRef = strings.TrimPrefix(imageRef, "docker://")
	if digestIndex := strings.LastIndex(imageRef, "@"); digestIndex >= 0 {
		imageRef = imageRef[:digestIndex]
	}
	lastSlash := strings.LastIndex(imageRef, "/")
	lastColon := strings.LastIndex(imageRef, ":")
	if lastColon > lastSlash {
		return imageRef[:lastColon], imageRef[lastColon+1:]
	}
	return imageRef, ""
}

func imageTagFromRef(imageRef string) string {
	_, tag := splitImageRef(imageRef)
	return tag
}

func imageNameWithoutTagDigest(imageRef string) string {
	name, _ := splitImageRef(imageRef)
	return name
}

func registryHostFromImageRef(imageRef string) string {
	imageRef = strings.TrimPrefix(imageRef, "docker://")
	parts := strings.Split(imageRef, "/")
	if len(parts) == 0 {
		return "docker.io"
	}
	first := parts[0]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first
	}
	return "docker.io"
}

func NormalizeBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed
	}
	return "http://" + trimmed
}
