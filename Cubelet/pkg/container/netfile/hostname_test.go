// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package netfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTrimHostName(t *testing.T) {
	t.Parallel()

	fullSandboxID := "711be366a1b2c3d4e5f60718293a4b5c"
	templateID := "tpl-cd1c0123456789abcdef0123"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "short", in: "abc", want: "abc"},
		{name: "exactly twelve", in: "711be366a1b2", want: "711be366a1b2"},
		{name: "full sandbox id", in: fullSandboxID, want: "711be366a1b2"},
		{name: "template id", in: templateID, want: "tpl-cd1c0123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, TrimHostName(tc.in))
		})
	}
}
