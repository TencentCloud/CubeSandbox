// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Document is the on-disk shape published by CubeShim, used by both the artifact
// file and the running shim's bundle file.
type Document struct {
	Schema       int               `json:"schema"`
	Component    string            `json:"component"`
	Version      string            `json:"version"`
	Capabilities map[string]uint32 `json:"capabilities"`
	Runtime      map[string]uint32 `json:"runtime"`
}

// SchemaVersion is the only document schema this build reads.
const SchemaVersion = 1

// ReadDocument reads and validates one capability document as untrusted input.
// An error means this source has nothing usable; callers fall through to the next.
func ReadDocument(path string) (*Document, error) {
	if path == "" {
		return nil, errors.New("capability path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink, refusing to follow it", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	// Read one byte past the limit so an oversized file is detected.
	body, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(body) > maxDocumentBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxDocumentBytes)
	}

	var doc Document
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Schema != SchemaVersion {
		return nil, fmt.Errorf("%s has schema %d, this build reads %d", path, doc.Schema, SchemaVersion)
	}
	if err := Validate(Set(doc.Capabilities)); err != nil {
		return nil, fmt.Errorf("%s capabilities: %w", path, err)
	}
	if err := Validate(Set(doc.Runtime)); err != nil {
		return nil, fmt.Errorf("%s runtime capabilities: %w", path, err)
	}
	return &doc, nil
}

// ArtifactDirFor resolves the component root holding capabilities.json from a
// binary path inside it. An already-directory path is returned as is.
func ArtifactDirFor(path string) string {
	path = filepath.Clean(path)
	if path == "" || path == "." {
		return ""
	}
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return path
	}
	// <root>/bin/<binary> -> <root>.
	parent := filepath.Dir(path)
	if filepath.Base(parent) == "bin" {
		return filepath.Dir(parent)
	}
	return parent
}

// Resolved is the outcome of resolving one sandbox's shim capabilities.
type Resolved struct {
	// Capabilities are the shim's own declarations (`update.*`, `errors.*`).
	Capabilities Set
	// Runtime is what the live guest agent reported (`guest.*`).
	Runtime Set
	// Source says which of the three sources produced the result.
	Source Source
	// Version is the version recorded in the document, when one was read.
	Version string
}

// Empty reports that nothing is known.
func (r Resolved) Empty() bool { return r.Source == SourceNone || len(r.Capabilities) == 0 }

// Resolve finds the capabilities of the shim backing one sandbox, trying the
// bundle runtime file, the artifact declaration, then the frozen historical
// table, in that order. onError reports sources that could not be used.
func Resolve(bundleDir, artifactDir, version string, onError func(source Source, err error)) Resolved {
	report := func(source Source, err error) {
		if onError != nil && err != nil {
			onError(source, err)
		}
	}

	if bundleDir != "" {
		doc, err := ReadDocument(filepath.Join(bundleDir, RuntimeFileName))
		switch {
		case err == nil:
			return Resolved{
				Capabilities: Set(doc.Capabilities).Clone(),
				Runtime:      Set(doc.Runtime).Clone(),
				Source:       SourceRuntime,
				Version:      doc.Version,
			}
		case !errors.Is(err, fs.ErrNotExist):
			// A missing file is the ordinary case; other errors are worth reporting.
			report(SourceRuntime, err)
		}
	}

	if artifactDir != "" {
		doc, err := ReadDocument(filepath.Join(artifactDir, ArtifactFileName))
		switch {
		case err == nil:
			return Resolved{
				Capabilities: Set(doc.Capabilities).Clone(),
				Runtime:      Set(doc.Runtime).Clone(),
				Source:       SourceArtifact,
				Version:      doc.Version,
			}
		case !errors.Is(err, fs.ErrNotExist):
			report(SourceArtifact, err)
		}
	}

	return FromLegacyVersion(version)
}
