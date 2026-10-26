// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package command

import (
	"context"
	"testing"

	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestContainerSpecCommand(t *testing.T) {
	for _, test := range []struct {
		desc            string
		criEntrypoint   []string
		criArgs         []string
		imageEntrypoint []string
		imageArgs       []string
		expected        []string
		expectErr       bool
	}{
		{
			desc:            "should use cri entrypoint if it's specified",
			criEntrypoint:   []string{"a", "b"},
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"a", "b"},
		},
		{
			desc:            "should use cri entrypoint if it's specified even if it's empty",
			criEntrypoint:   []string{},
			criArgs:         []string{"a", "b"},
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"a", "b"},
		},
		{
			desc:            "should use cri entrypoint and args if they are specified",
			criEntrypoint:   []string{"a", "b"},
			criArgs:         []string{"c", "d"},
			imageEntrypoint: []string{"e", "f"},
			imageArgs:       []string{"g", "h"},
			expected:        []string{"a", "b", "c", "d"},
		},
		{
			desc:            "should use image entrypoint if cri entrypoint is not specified",
			criArgs:         []string{"a", "b"},
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"c", "d", "a", "b"},
		},
		{
			desc:            "should use image args if both cri entrypoint and args are not specified",
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"c", "d", "e", "f"},
		},
		{
			desc:      "should return error if both entrypoint and args are empty",
			expectErr: true,
		},
	} {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			config := &cubebox.ContainerConfig{}
			config.Command = test.criEntrypoint
			config.Args = test.criArgs
			imageConfig := &imagespec.ImageConfig{}
			imageConfig.Entrypoint = test.imageEntrypoint
			imageConfig.Cmd = test.imageArgs

			var spec runtimespec.Spec
			err := WithProcessArgs(config, imageConfig)(context.Background(), nil, nil, &spec)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, test.expected, spec.Process.Args, test.desc)
		})
	}
}
