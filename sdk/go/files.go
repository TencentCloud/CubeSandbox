// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
)

type Files struct {
	reader fileReader
	writer fileWriter
	filer  fileFiler
	user   string
}

type fileRequestOptions struct {
	user string
}

type fileRequestOption func(*fileRequestOptions)

func withUser(user string) fileRequestOption {
	return func(options *fileRequestOptions) {
		options.user = user
	}
}

func resolveFileRequestOptions(options ...fileRequestOption) fileRequestOptions {
	var resolved fileRequestOptions
	for _, option := range options {
		if option != nil {
			option(&resolved)
		}
	}
	return resolved
}

type fileReader interface {
	readFile(context.Context, string, ...fileRequestOption) (string, error)
	readFileBytes(context.Context, string, ...fileRequestOption) ([]byte, error)
	openFileStream(context.Context, string, ...fileRequestOption) (io.ReadCloser, error)
}

type fileWriter interface {
	writeFile(context.Context, string, []byte, ...fileRequestOption) error
}

type fileFiler interface {
	listDir(context.Context, string, ...fileRequestOption) ([]FileEntry, error)
	statFile(context.Context, string, ...fileRequestOption) (*FileEntry, error)
	removeFile(context.Context, string, ...fileRequestOption) error
	moveFile(context.Context, string, string, ...fileRequestOption) (*FileEntry, error)
	makeDirFile(context.Context, string, ...fileRequestOption) (*FileEntry, error)
	watchDir(context.Context, string, ...fileRequestOption) (*Watcher, error)
}

// ForUser returns an immutable filesystem view that executes every operation
// as user. The returned view shares the underlying sandbox connection with f.
// An empty user restores the unscoped behavior used by Sandbox.Files.
func (f *Files) ForUser(user string) *Files {
	if f == nil {
		return nil
	}
	scoped := *f
	scoped.user = user
	return &scoped
}

// Read downloads file content as text (UTF-8 string), matching the e2b
// files.read(..., format="text") default. Binary files should use ReadBytes.
func (f *Files) Read(ctx context.Context, path string) (string, error) {
	if f == nil || f.reader == nil {
		return "", fmt.Errorf("files is not attached to a sandbox")
	}
	return f.reader.readFile(ctx, path, withUser(f.user))
}

// ReadBytes downloads file content as raw bytes, matching e2b
// files.read(..., format="bytes"). Prefer this for any non-UTF-8 payload.
func (f *Files) ReadBytes(ctx context.Context, path string) ([]byte, error) {
	if f == nil || f.reader == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.reader.readFileBytes(ctx, path, withUser(f.user))
}

// ReadStream opens a streaming download of path, matching e2b
// files.read(..., format="stream"). The caller must Close the reader.
func (f *Files) ReadStream(ctx context.Context, path string) (io.ReadCloser, error) {
	if f == nil || f.reader == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.reader.openFileStream(ctx, path, withUser(f.user))
}

// Write uploads data to path through envd's HTTP file API.
func (f *Files) Write(ctx context.Context, path string, data []byte) error {
	if f == nil || f.writer == nil {
		return fmt.Errorf("files is not attached to a sandbox")
	}
	return f.writer.writeFile(ctx, path, data, withUser(f.user))
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
	return f.filer.listDir(ctx, path, withUser(f.user))
}

// Stat returns metadata for a single file or directory.
func (f *Files) Stat(ctx context.Context, path string) (*FileEntry, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.statFile(ctx, path, withUser(f.user))
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
	return f.filer.removeFile(ctx, path, withUser(f.user))
}

// Rename moves or renames a file or directory inside the sandbox.
func (f *Files) Rename(ctx context.Context, oldPath, newPath string) (*FileEntry, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.moveFile(ctx, oldPath, newPath, withUser(f.user))
}

// MakeDir creates a directory inside the sandbox.
func (f *Files) MakeDir(ctx context.Context, path string) (*FileEntry, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.makeDirFile(ctx, path, withUser(f.user))
}

// WatchDir watches a directory for filesystem changes. The returned Watcher
// delivers events on its Events channel. Call Watcher.Close to stop.
func (f *Files) WatchDir(ctx context.Context, path string) (*Watcher, error) {
	if f == nil || f.filer == nil {
		return nil, fmt.Errorf("files is not attached to a sandbox")
	}
	return f.filer.watchDir(ctx, path, withUser(f.user))
}
