// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"errors"
	"fmt"
)

type Files struct {
	reader fileReader
	writer fileWriter
	filer  fileFiler
}

type fileReader interface {
	readFile(context.Context, string) (string, error)
}

type fileWriter interface {
	writeFile(context.Context, string, []byte) error
}

type fileFiler interface {
	listDir(context.Context, string) ([]FileEntry, error)
	statFile(context.Context, string) (*FileEntry, error)
	removeFile(context.Context, string) error
	moveFile(context.Context, string, string) (*FileEntry, error)
	makeDirFile(context.Context, string) (*FileEntry, error)
	watchDir(context.Context, string) (*Watcher, error)
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

// WriteFiles uploads multiple files. It stops at the first error and returns
// the number of files successfully written.
func (f *Files) WriteFiles(ctx context.Context, entries []WriteEntry) (int, error) {
	if f == nil || f.writer == nil {
		return 0, fmt.Errorf("files is not attached to a sandbox")
	}
	for i, e := range entries {
		if err := f.Write(ctx, e.Path, e.Data); err != nil {
			return i, fmt.Errorf("write %s: %w", e.Path, err)
		}
	}
	return len(entries), nil
}

// List returns the entries in a directory.
func (f *Files) List(ctx context.Context, path string) ([]FileEntry, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.listDir(ctx, path)
}

// Stat returns metadata for a single file or directory.
func (f *Files) Stat(ctx context.Context, path string) (*FileEntry, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.statFile(ctx, path)
}

// Exists returns true if the path exists inside the sandbox.
func (f *Files) Exists(ctx context.Context, path string) (bool, error) {
	_, err := f.Stat(ctx, path)
	if err == nil {
		return true, nil
	}
	var nfe *NotFoundError
	if errors.As(err, &nfe) {
		return false, nil
	}
	return false, err
}

// Remove deletes a file or directory inside the sandbox.
func (f *Files) Remove(ctx context.Context, path string) error {
	if f == nil || f.filer == nil {
		return fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.removeFile(ctx, path)
}

// Rename moves or renames a file or directory inside the sandbox.
func (f *Files) Rename(ctx context.Context, oldPath, newPath string) (*FileEntry, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.moveFile(ctx, oldPath, newPath)
}

// MakeDir creates a directory inside the sandbox.
func (f *Files) MakeDir(ctx context.Context, path string) (*FileEntry, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.makeDirFile(ctx, path)
}

// WatchDir watches a directory for filesystem changes. The returned Watcher
// delivers events on its Events channel. Call Watcher.Close to stop.
func (f *Files) WatchDir(ctx context.Context, path string) (*Watcher, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.watchDir(ctx, path)
}
