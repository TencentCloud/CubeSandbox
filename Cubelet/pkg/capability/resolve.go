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

// Document is the on-disk shape published by a component's artifact. Every
// component uses the same shape.
type Document struct {
	Schema       int               `json:"schema"`
	Component    string            `json:"component"`
	Version      string            `json:"version"`
	Capabilities map[string]uint32 `json:"capabilities"`
}

// SchemaVersion is the only document schema this build reads.
const SchemaVersion = 1

// ReadDocument reads and validates one capability document.
//
// The file is untrusted input, so symlinks and non-regular files are refused, the
// read is bounded, and every name and level is validated. An error means this
// source has nothing usable; it never means the component declares nothing.
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

	// Read one byte past the limit so an oversized file is detected rather than
	// silently truncated into a valid-looking document.
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
	return &doc, nil
}

// ArtifactDirFor resolves the component root that holds `capabilities.json` from
// a path to a file inside it: `<root>/bin/<binary>` or `<root>/<payload>`.
// An already-directory path is returned as is.
func ArtifactDirFor(path string) string {
	path = filepath.Clean(path)
	if path == "" || path == "." {
		return ""
	}
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return path
	}
	parent := filepath.Dir(path)
	if filepath.Base(parent) == "bin" {
		return filepath.Dir(parent)
	}
	return parent
}

// Resolved is the outcome of resolving one component's capabilities for one sandbox.
type Resolved struct {
	// Capabilities are the component's declarations, keyed by capability name.
	Capabilities Set
	// Source says which source produced the result.
	Source Source
	// Version is the version the declaration or table entry describes.
	Version string
}

// Empty reports that nothing is known, so every consumer must keep its
// pre-existing behaviour.
func (r Resolved) Empty() bool { return r.Source == SourceNone || len(r.Capabilities) == 0 }

// resolveFromArtifact reads artifactDir's declaration. The bool reports whether
// one was found and usable.
func resolveFromArtifact(artifactDir string, onError func(source Source, err error)) (Resolved, bool) {
	if artifactDir == "" {
		return Resolved{}, false
	}
	doc, err := ReadDocument(filepath.Join(artifactDir, ArtifactFileName))
	switch {
	case err == nil:
		return Resolved{
			Capabilities: Set(doc.Capabilities).Clone(),
			Source:       SourceArtifact,
			Version:      doc.Version,
		}, true
	case errors.Is(err, fs.ErrNotExist):
		return Resolved{}, false
	default:
		if onError != nil {
			onError(SourceArtifact, err)
		}
		return Resolved{}, false
	}
}

// Resolve finds what the shim backing one sandbox can do: the released artifact's
// declaration first, then the frozen table keyed by the pinned version.
//
// A missing or malformed file falls through rather than failing the caller, so an
// unreadable artifact degrades to the previous release's answer.
func Resolve(artifactDir, version string, onError func(source Source, err error)) Resolved {
	if resolved, ok := resolveFromArtifact(artifactDir, onError); ok {
		return resolved
	}
	return FromLegacyVersion(version)
}

// ResolveGuest is the same two-level rule applied to the guest agent.
func ResolveGuest(artifactDir, version string, onError func(source Source, err error)) Resolved {
	if resolved, ok := resolveFromArtifact(artifactDir, onError); ok {
		return resolved
	}
	return FromLegacyAgentVersion(version)
}
