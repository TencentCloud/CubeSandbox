// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"cmp"
	"slices"
)

// CanonicalLocalTemplates returns a copy ordered by every persisted field.
// Older Cubelets build this list from a map, so normalize at the receiver too.
func CanonicalLocalTemplates(templates []LocalTemplate) []LocalTemplate {
	out := slices.Clone(templates)
	slices.SortFunc(out, func(a, b LocalTemplate) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.TemplateID, b.TemplateID), cmp.Compare(a.ID, b.ID),
			cmp.Compare(a.Media, b.Media), cmp.Compare(a.Path, b.Path))
	})
	return out
}

// CanonicalImages normalizes both the image list and each image's references
// without changing the request's slices, which may also be used by callers.
func CanonicalImages(images []ContainerImage) []ContainerImage {
	out := slices.Clone(images)
	for i := range out {
		out[i].Names = slices.Clone(out[i].Names)
		slices.Sort(out[i].Names)
	}
	slices.SortFunc(out, func(a, b ContainerImage) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace),
			slices.Compare(a.Names, b.Names), cmp.Compare(a.MediaType, b.MediaType),
			cmp.Compare(a.SizeBytes, b.SizeBytes))
	})
	return out
}
