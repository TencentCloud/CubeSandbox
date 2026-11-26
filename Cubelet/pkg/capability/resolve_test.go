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
	}
}

func agentDocument() Document {
	return Document{
		Schema:       SchemaVersion,
		Component:    "cube-agent",
		Version:      "v0.8.0",
		Capabilities: map[string]uint32{CapGuestFSFreeze: LevelInitial},
	}
}

func TestReadDocumentAcceptsARegularFile(t *testing.T) {
	dir := t.TempDir()
	path := writeDoc(t, dir, ArtifactFileName, fullDocument())

	doc, err := ReadDocument(path)
	require.NoError(t, err)
	require.Equal(t, "v0.8.0", doc.Version)
	require.True(t, Set(doc.Capabilities).Has(CapUpdateSnapshotCaptureFSOnly, 1))
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

// The artifact declaration is the only per-version source: a running process
// publishes nothing, so this is what a decision must be based on.
func TestResolveUsesTheArtifactDeclaration(t *testing.T) {
	artifact := t.TempDir()
	writeDoc(t, artifact, ArtifactFileName, fullDocument())

	resolved := Resolve(artifact, "v0.8.0", nil)
	require.Equal(t, SourceArtifact, resolved.Source)
	require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCapture, 1))
	require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1))
}

// A missing declaration must fall through to the frozen table rather than erroring.
func TestResolveFallsBackToTheFrozenTable(t *testing.T) {
	resolved := Resolve(t.TempDir(), "v0.7.3", nil)
	require.Equal(t, SourceLegacyTable, resolved.Source)
	require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCapture, 1))
	require.False(t, resolved.Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1))
}

// A corrupt declaration is reported and then falls through: the caller gets the
// same answer the previous release would have given, and the problem is visible.
func TestResolveReportsAnUnusableDeclarationAndKeepsFallingThrough(t *testing.T) {
	artifact := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(artifact, ArtifactFileName), []byte("{oops"), 0o644))

	var reported []Source
	resolved := Resolve(artifact, "v0.8.0", func(s Source, _ error) { reported = append(reported, s) })
	require.Equal(t, SourceLegacyTable, resolved.Source)
	require.Equal(t, []Source{SourceArtifact}, reported)
	require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1),
		"the frozen table answers for a known version even when the artifact is unusable")
}

func TestResolveWithNothingKnownStaysEmpty(t *testing.T) {
	resolved := Resolve(t.TempDir(), "", nil)
	require.Equal(t, SourceNone, resolved.Source)
	require.True(t, resolved.Empty())
}

// The guest agent uses the same two-level rule, with its own frozen table.
func TestResolveGuestUsesTheAgentArtifact(t *testing.T) {
	artifact := t.TempDir()
	writeDoc(t, artifact, ArtifactFileName, agentDocument())

	resolved := ResolveGuest(artifact, "v0.8.0", nil)
	require.Equal(t, SourceArtifact, resolved.Source)
	require.Equal(t, "cube-agent", agentDocument().Component)
	require.True(t, resolved.Capabilities.Has(CapGuestFSFreeze, 1))
}

func TestResolveGuestFallsBackToTheAgentTable(t *testing.T) {
	resolved := ResolveGuest(t.TempDir(), "v0.7.3", nil)
	require.Equal(t, SourceLegacyTable, resolved.Source)
	require.False(t, resolved.Capabilities.Has(CapGuestFSFreeze, 1),
		"an agent older than the freeze RPCs cannot freeze")

	resolved = ResolveGuest(t.TempDir(), "v0.8.0", nil)
	require.Equal(t, SourceLegacyTable, resolved.Source)
	require.True(t, resolved.Capabilities.Has(CapGuestFSFreeze, 1))
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
	// The agent artifact is a flat directory: the payload sits directly in it.
	require.Equal(t,
		"/data/cubelet/root/component_versions/cube-agent/v0.8.0",
		ArtifactDirFor("/data/cubelet/root/component_versions/cube-agent/v0.8.0/cube-agent.ext4"))
}
