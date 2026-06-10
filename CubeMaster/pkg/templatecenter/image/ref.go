// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"strings"
)

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
