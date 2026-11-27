// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodestatus

import (
	"context"
	"slices"
	"testing"

	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
)

func TestImagesTruncatesSortedNames(t *testing.T) {
	for _, refs := range [][]string{
		{"g", "f", "e", "d", "c", "b", "a"},
		{"a", "b", "c", "d", "e", "f", "g", "a"},
		{"b", "a"},
	} {
		t.Run(refs[0], func(t *testing.T) {
			set := Images(-1, func(context.Context) ([]imagestore.Image, error) {
				return []imagestore.Image{{ID: "a", References: refs}}, nil
			})
			want := []string{"a", "b", "c", "d", "e"}
			if len(refs) < MaxNamesPerImageInNodeStatus {
				want = []string{"a", "b"}
			}
			for range 32 {
				node := &cubeletnodemeta.Node{}
				if err := set(context.Background(), node); err != nil {
					t.Fatal(err)
				}
				if len(node.Status.CubeImages) != 1 || !slices.Equal(node.Status.CubeImages[0].Names, want) {
					t.Fatalf("images = %+v, want names %v", node.Status.CubeImages, want)
				}
			}
		})
	}
}
