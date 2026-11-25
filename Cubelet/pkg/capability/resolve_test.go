// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeDoc writes a document into dir and returns the file path.
func writeDoc(t *testing.T, dir, name string, doc Document) string {
	t.Helper()
	body, err := json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, body, 0o644))
	return path
}

func fullDocument() Document {
	return Document{
		Schema:    SchemaVersion,
		Component: "cube-shim",
		Version:   "v0.8.0",
		Capabilities: map[string]uint32{
			CapUpdateSnapshotCapture:       LevelInitial,
			CapUpdateSnapshotCaptureFSOnly: LevelInitial,
			CapErrorsCoded:                 LevelInitial,
		},
		Runtime: map[string]uint32{CapGuestFSFreeze: 1},
	}
}

func TestReadDocumentAcceptsARegularFile(t *testing.T) {
	dir := t.TempDir()
	path := writeDoc(t, dir, ArtifactFileName, fullDocument())

	doc, err := ReadDocument(path)
	require.NoError(t, err)
	require.Equal(t, "v0.8.0", doc.Version)
	require.True(t, Set(doc.Capabilities).Has(CapUpdateSnapshotCaptureFSOnly, 1))
	require.Equal(t, uint32(1), doc.Runtime[CapGuestFSFreeze])
}

func TestReadDocumentRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	real := writeDoc(t, dir, "real.json", fullDocument())
	link := filepath.Join(dir, ArtifactFileName)
	require.NoError(t, os.Symlink(real, link))

	_, err := ReadDocument(link)
	require.ErrorContains(t, err, "symlink")
}

func TestReadDocumentRefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ArtifactFileName)
	require.NoError(t, os.Mkdir(path, 0o755))

	_, err := ReadDocument(path)
	require.ErrorContains(t, err, "not a regular file")
}

func TestReadDocumentRefusesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ArtifactFileName)
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", maxDocumentBytes+1)), 0o644))

	_, err := ReadDocument(path)
	require.ErrorContains(t, err, "exceeds")
}

func TestReadDocumentRefusesAnUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	doc := fullDocument()
	doc.Schema = SchemaVersion + 1
	path := writeDoc(t, dir, ArtifactFileName, doc)

	_, err := ReadDocument(path)
	require.ErrorContains(t, err, "schema")
}

func TestReadDocumentRefusesAMalformedName(t *testing.T) {
	dir := t.TempDir()
	doc := fullDocument()
	doc.Capabilities = map[string]uint32{"Update.Bad": 1}
	path := writeDoc(t, dir, ArtifactFileName, doc)

	_, err := ReadDocument(path)
	require.ErrorContains(t, err, "capabilities")
}

func TestReadDocumentRefusesAnEmptyPath(t *testing.T) {
	_, err := ReadDocument("")
	require.Error(t, err)
}

func TestResolvePrefersTheRunningShimOverEverythingElse(t *testing.T) {
	bundle := t.TempDir()
	artifact := t.TempDir()

	// The bundle describes the process that will answer: fs-only present.
	writeDoc(t, bundle, RuntimeFileName, Document{
		Schema: SchemaVersion, Component: "cube-shim", Version: "v0.8.0",
		Capabilities: map[string]uint32{CapUpdateSnapshotCapture: 1, CapUpdateSnapshotCaptureFSOnly: 1},
		Runtime:      map[string]uint32{CapGuestFSFreeze: 0},
	})
	// The artifact of the pinned (older) version would say otherwise.
	writeDoc(t, artifact, ArtifactFileName, Document{
		Schema: SchemaVersion, Component: "cube-shim", Version: "v0.7.2",
		Capabilities: map[string]uint32{CapUpdateSnapshotCapture: 1},
	})

	resolved := Resolve(bundle, artifact, "v0.7.2", nil)
	require.Equal(t, SourceRuntime, resolved.Source)
	require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1),
		"the running shim wins over the pinned version")
	require.True(t, resolved.Runtime.KnownUnsupported(CapGuestFSFreeze, 1),
		"a refused guest answer is carried through")
}

func TestResolveFallsBackToTheArtifactWhenTheBundleHasNoFile(t *testing.T) {
	bundle := t.TempDir()
	artifact := t.TempDir()
	writeDoc(t, artifact, ArtifactFileName, fullDocument())

	var reported []Source
	resolved := Resolve(bundle, artifact, "v0.8.0", func(s Source, _ error) { reported = append(reported, s) })
	require.Equal(t, SourceArtifact, resolved.Source)
	require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1))
	require.Empty(t, reported, "a missing runtime file is the ordinary case, not an error")
}

func TestResolveReportsAnUnusableFileAndKeepsFallingThrough(t *testing.T) {
	bundle := t.TempDir()
	artifact := t.TempDir()
	// A corrupt runtime file must not stop the chain.
	require.NoError(t, os.WriteFile(filepath.Join(bundle, RuntimeFileName), []byte("{oops"), 0o644))
	writeDoc(t, artifact, ArtifactFileName, fullDocument())

	var reported []Source
	resolved := Resolve(bundle, artifact, "v0.8.0", func(s Source, _ error) { reported = append(reported, s) })
	require.Equal(t, SourceArtifact, resolved.Source)
	require.Equal(t, []Source{SourceRuntime}, reported)
}

func TestResolveFallsBackToTheLegacyTable(t *testing.T) {
	resolved := Resolve(t.TempDir(), t.TempDir(), "v0.7.3", nil)
	require.Equal(t, SourceLegacyTable, resolved.Source)
	require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCapture, 1))
	require.False(t, resolved.Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1))
}

func TestResolveWithNothingKnownStaysEmpty(t *testing.T) {
	resolved := Resolve(t.TempDir(), t.TempDir(), "", nil)
	require.Equal(t, SourceNone, resolved.Source)
	require.True(t, resolved.Empty())
	require.Empty(t, resolved.Capabilities)
}

func TestArtifactDirForClimbsOutOfTheBinDirectory(t *testing.T) {
	root := "/data/cubelet/root/component_versions/cube-shim/v0.8.0"
	require.Equal(t, root, ArtifactDirFor(filepath.Join(root, "bin", "containerd-shim-cube-rs")))
	// A path that is already the component root is returned unchanged.
	dir := t.TempDir()
	require.Equal(t, filepath.Clean(dir), ArtifactDirFor(dir))
	// The live toolbox layout uses the same shape.
	require.Equal(t,
		"/usr/local/services/cubetoolbox/cube-shim",
		ArtifactDirFor("/usr/local/services/cubetoolbox/cube-shim/bin/containerd-shim-cube-rs"))
}
