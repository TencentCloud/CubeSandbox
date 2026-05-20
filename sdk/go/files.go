// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"fmt"
)

type Files struct {
	reader fileReader
}

type fileReader interface {
	readFile(context.Context, string) (string, error)
}

func (f *Files) Read(ctx context.Context, path string) (string, error) {
	if f == nil || f.reader == nil {
		return "", fmt.Errorf("files is not attached to a sandbox")
	}
	return f.reader.readFile(ctx, path)
}
