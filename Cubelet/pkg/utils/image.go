// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

const (
	truncatedIDLen = 13
)

func NormalizeRepoDigest(repoDigests []string) (repo, digest string) {
	if len(repoDigests) == 0 {
		return "<none>", "<none>"
	}

	repoDigestPair := strings.Split(repoDigests[0], "@")
	if len(repoDigestPair) != 2 {
		return "errorName", "errorRepoDigest"
	}

	return repoDigestPair[0], repoDigestPair[1]
}

func ParseImageDigestAndTag(name string) (repoTag string, repoDigest string, err error) {
	refed, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse image reference %q: %w", name, err)
	}

	if named, ok := refed.(reference.NamedTagged); ok {
		repoTag = named.Name()
		if named.Tag() != "" {
			repoTag = fmt.Sprintf("%s:%s", repoTag, named.Tag())
		}
	} else {
		repoTag = refed.String()
	}
	if canon, ok := refed.(reference.Digested); ok {
		repoDigest = canon.Digest().String()
	}

	return
}

func NormalizeRepoTagPair(repoTags []string, imageName string) (repoTagPairs [][]string) {
	const none = "<none>"
	if len(repoTags) == 0 {
		repoTagPairs = append(repoTagPairs, []string{imageName, none})

		return
	}

	for _, repoTag := range repoTags {
		idx := strings.LastIndex(repoTag, ":")
		if idx == -1 {
			repoTagPairs = append(repoTagPairs, []string{"errorRepoTag", "errorRepoTag"})

			continue
		}

		name := repoTag[:idx]
		if name == none {
			name = imageName
		}

		repoTagPairs = append(repoTagPairs, []string{name, repoTag[idx+1:]})
	}

	return
}

func GetTruncatedID(id, prefix string) string {
	id = strings.TrimPrefix(id, prefix)
	if len(id) > truncatedIDLen {
		id = id[:truncatedIDLen]
	}

	return id
}
