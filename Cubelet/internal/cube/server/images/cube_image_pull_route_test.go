// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package images

import (
	"context"
	"testing"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
)

func TestCubeImageService_PullImage(t *testing.T) {
	_ = context.Background()
	name := "registry.example.com/public/test-image:v0.2@sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6"
	namedRef, err := reference.ParseDockerRef(name)
	assert.NoError(t, err, "failed to parse name")
	assert.Equal(t, "registry.example.com/public/test-image", namedRef.Name(), "name")
	assert.Equal(t, "registry.example.com/public/test-image@sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6", namedRef.String(), "name")

	refDigest := digest.Digest("sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6")
	repoDigest, repoTag := util.GetRepoDigestAndTag(namedRef, refDigest)
	assert.Equal(t, "registry.example.com/public/test-image@sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6", repoDigest, "repo digest")

	named, err := reference.ParseNormalizedNamed(name)
	assert.NoError(t, err, "failed to parse name")
	if taged, ok := named.(reference.NamedTagged); ok {
		repoTag = taged.Tag()
	}
	if canonical, ok := named.(reference.Digested); ok {
		repoDigest = canonical.Digest().String()
	}
	assert.Equal(t, "v0.2", repoTag, "repo tag")
	assert.Equal(t, "sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6", repoDigest, "repo digest")
}

func TestImageNameTest(t *testing.T) {
	name := "registry.example.com/public/test-image:v0.2@sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6"
	namedRef, err := reference.ParseDockerRef(name)
	assert.NoError(t, err, "failed to parse name")
	assert.Equal(t, "registry.example.com/public/test-image", namedRef.Name(), "name")
	assert.Equal(t, "registry.example.com/public/test-image@sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6", namedRef.String(), "named ref")
	canon, ok := namedRef.(reference.Canonical)
	assert.True(t, ok, "canonical")
	assert.Equal(t, "registry.example.com/public/test-image@sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6", canon.String(), "named ref")
	assert.Equal(t, "sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6", canon.Digest().String(), "reference.Digest")

	_, isNamedTagged := namedRef.(reference.NamedTagged)
	assert.False(t, isNamedTagged, "ParseDockerRef result should NOT implement NamedTagged when both tag and digest exist")

	named, err := reference.ParseNormalizedNamed(name)
	reference.Parse(name)
	assert.NoError(t, err, "failed to parse normalized named")
	tagged, ok := named.(reference.NamedTagged)
	assert.True(t, ok, "ParseNormalizedNamed result should implement NamedTagged")
	assert.Equal(t, "registry.example.com/public/test-image", tagged.Name(), "named")
	assert.Equal(t, "v0.2", tagged.Tag(), "tag")
	digest := named.(reference.Digested)
	assert.Equal(t, "sha256:f4c6858dac7216a8416bdea5708a037dec4b1cf242aa03d4f2155e37c51479e6", digest.Digest().String(), "digest")

}
