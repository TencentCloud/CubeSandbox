// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"fmt"
)

type Files struct {
	reader fileReader
	writer fileWriter
}

type fileReader interface {
	readFile(context.Context, string) (string, error)
}

type fileWriter interface {
	writeFile(context.Context, string, []byte) error
}

func (f *Files) Read(ctx context.Context, path string) (string, error) {
	if f == nil || f.reader == nil {
		return "", fmt.Errorf("files is not attached to a sandbox")
	}
	return f.reader.readFile(ctx, path)
}

// Write uploads data to path through envd's HTTP file API.
func (f *Files) Write(ctx context.Context, path string, data []byte) error {
	if f == nil || f.writer == nil {
		return fmt.Errorf("files is not attached to a sandbox")
	}
	return f.writer.writeFile(ctx, path, data)
}
