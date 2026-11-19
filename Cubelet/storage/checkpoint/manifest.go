// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// SchemaVersion is the candidate format, and the manifest format of a
	// full export.
	SchemaVersion = 1
	// SchemaVersionIncremental is the manifest format of an incremental
	// export: per-file chunk sizes, and chunks that may live in the
	// sandbox's lineage or its template's base instead of the session.
	SchemaVersionIncremental = 2

	// Kinds of volume in an incremental manifest.
	KindRootfs = "rootfs"
	KindMemory = "memory"

	// RoleVolume is a cubecow object (rootfs, memory) the package refers to
	// by name. RoleMeta is a file under the package home, by relative path.
	RoleVolume = "volume"
	RoleMeta   = "meta"

	// DefaultChunkSize bounds one object and one request.
	DefaultChunkSize int64 = 16 << 20
	minChunkSize     int64 = 1 << 20
	maxChunkSize     int64 = 256 << 20

	maxFiles       = 4096
	maxChunks      = 1 << 22
	maxFileSize    = int64(4) << 40
	maxMetaDepth   = 8
	maxManifestLen = 512 << 20
)

var (
	safeIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	safeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Candidate is the small object at the fixed key of a snapshot. Writing it
// is the last step of an export, and it pins one session and the digest of
// that session's manifest.
type Candidate struct {
	SchemaVersion  int    `json:"schema_version"`
	SnapshotID     string `json:"snapshot_id"`
	Session        string `json:"session"`
	ManifestKey    string `json:"manifest_key"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ManifestSize   int64  `json:"manifest_size"`
	CreatedAt      string `json:"created_at"`
}

// Manifest lists every file of one checkpoint. Bytes of a file not covered
// by a chunk are zero by definition; nothing else is implied.
type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	SnapshotID    string            `json:"snapshot_id"`
	Session       string            `json:"session"`
	CreatedAt     string            `json:"created_at"`
	ChunkSize     int64             `json:"chunk_size"`
	Attributes    map[string]string `json:"attributes,omitempty"`
	Files         []File            `json:"files"`
	// Version 2 only.
	SandboxID string `json:"sandbox_id,omitempty"`
	// TemplateID names the template whose base this manifest may reuse.
	TemplateID string `json:"template_id,omitempty"`
	// BaseManifestSHA256 is the manifest this one was built against, if any.
	BaseManifestSHA256 string `json:"base_manifest_sha256,omitempty"`
	// Generation is the lineage generation the export uploaded new chunks
	// into; its lineage chunks all sit in this generation or an older one.
	// Zero is a manifest from before generations, whose lineage chunks sit
	// directly under the lineage's c/.
	Generation int64 `json:"generation,omitempty"`
}

// File is one restored file.
type File struct {
	Role   string  `json:"role"`
	Name   string  `json:"name"`
	Size   int64   `json:"size"`
	Mode   uint32  `json:"mode,omitempty"`
	Chunks []Chunk `json:"chunks"`
	// Version 2 only: the volume's kind and its own chunk size.
	Kind      string `json:"kind,omitempty"`
	ChunkSize int64  `json:"chunk_size,omitempty"`
}

// fileChunkSize is the chunk size f was cut with.
func (m *Manifest) fileChunkSize(f *File) int64 {
	if f.ChunkSize > 0 {
		return f.ChunkSize
	}
	return m.ChunkSize
}

// Chunk is one non-zero range of a file and the object that holds it.
type Chunk struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
	Key    string `json:"key"`
}

// DataBytes is the number of bytes the manifest stores in objects.
func (m *Manifest) DataBytes() int64 {
	var n int64
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			n += c.Length
		}
	}
	return n
}

// LogicalBytes is the restored size of every file together.
func (m *Manifest) LogicalBytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// SnapshotPrefix is where every object of snapshotID lives.
func SnapshotPrefix(prefix, snapshotID string) string {
	return strings.Trim(prefix, "/") + "/" + snapshotID + "/"
}

// CandidateKey is the fixed key a reader discovers a checkpoint at. It is
// derived from the snapshot id alone, so nothing has to be told where to
// look once Master has assigned the id.
func CandidateKey(prefix, snapshotID string) string {
	return SnapshotPrefix(prefix, snapshotID) + "candidate.json"
}

// SessionPrefix holds the objects of one upload attempt.
func SessionPrefix(prefix, snapshotID, session string) string {
	return SnapshotPrefix(prefix, snapshotID) + "s-" + session + "/"
}

// ManifestKey is the manifest of one session.
func ManifestKey(prefix, snapshotID, session string) string {
	return SessionPrefix(prefix, snapshotID, session) + "manifest.json"
}

// EvidenceKey is the readback evidence of one session.
func EvidenceKey(prefix, snapshotID, session string) string {
	return SessionPrefix(prefix, snapshotID, session) + "verified.json"
}

// ChunkKey is content-addressed inside a session: a repeated chunk is
// uploaded once, and a reader can tell from the key alone which object it
// must get.
func ChunkKey(prefix, snapshotID, session, sum string) string {
	return SessionPrefix(prefix, snapshotID, session) + "c/" + sum
}

// LineagePrefix holds the chunks every checkpoint of one sandbox shares.
func LineagePrefix(prefix, sandboxID string) string {
	return strings.Trim(prefix, "/") + "/lineage/" + sandboxID + "/"
}

// A lineage is cut into generations: every export uploads its new chunks
// under a generation of its own, greater than the one of the checkpoint it
// was built from, and reuses only chunks that checkpoint names. The sweep
// after checkpoint N deletes only chunks of generation N or older that N
// does not name, so it can never delete a chunk a newer export uploaded or
// reused, wherever that export runs and however the two interleave.
const lineageGenDigits = 19

// LineageGenerationPrefix holds the chunks one export uploaded.
func LineageGenerationPrefix(prefix, sandboxID string, gen int64) string {
	return LineagePrefix(prefix, sandboxID) + fmt.Sprintf("g/%0*d/", lineageGenDigits, gen)
}

// LineageChunkKey is content-addressed within one generation of a sandbox's
// lineage.
func LineageChunkKey(prefix, sandboxID string, gen int64, sum string) string {
	return LineageGenerationPrefix(prefix, sandboxID, gen) + "c/" + sum
}

// parseLineageKey splits a key under lineage (a LineagePrefix) into its
// generation and digest. A chunk from before generations, lineage + "c/" +
// sum, is generation 0.
func parseLineageKey(lineage, key string) (gen int64, sum string, ok bool) {
	rest, found := strings.CutPrefix(key, lineage)
	if !found {
		return 0, "", false
	}
	if sum, found := strings.CutPrefix(rest, "c/"); found {
		return 0, sum, sha256RE.MatchString(sum)
	}
	rest, found = strings.CutPrefix(rest, "g/")
	if !found || len(rest) < lineageGenDigits+3 || rest[lineageGenDigits:lineageGenDigits+3] != "/c/" {
		return 0, "", false
	}
	gen, err := strconv.ParseInt(rest[:lineageGenDigits], 10, 64)
	sum = rest[lineageGenDigits+3:]
	if err != nil || gen <= 0 || !sha256RE.MatchString(sum) {
		return 0, "", false
	}
	return gen, sum, true
}

// ReusableChunks maps the digest of every volume chunk a later checkpoint of
// the same sandbox may name to its key: chunks of a lineage generation and
// of the template base. Chunks from before generations are left out; their
// keys are not protected by the sweep's generation rule.
func (m *Manifest) ReusableChunks(prefix string) map[string]string {
	out := map[string]string{}
	if m.SchemaVersion != SchemaVersionIncremental || m.Generation <= 0 {
		return out
	}
	lineage := LineagePrefix(prefix, m.SandboxID)
	tpl := ""
	if m.TemplateID != "" {
		tpl = TemplatePrefix(prefix, m.TemplateID) + "c/"
	}
	for _, f := range m.Files {
		if f.Role != RoleVolume {
			continue
		}
		for _, ch := range f.Chunks {
			gen, sum, ok := parseLineageKey(lineage, ch.Key)
			if (ok && gen > 0 && sum == ch.SHA256) || (tpl != "" && ch.Key == tpl+ch.SHA256) {
				if _, have := out[ch.SHA256]; !have {
					out[ch.SHA256] = ch.Key
				}
			}
		}
	}
	return out
}

// TemplatePrefix holds a template's base: chunks of its memory that every
// sandbox started from it may reuse.
func TemplatePrefix(prefix, templateID string) string {
	return strings.Trim(prefix, "/") + "/template/" + templateID + "/"
}

// TemplateChunkKey is content-addressed within a template's base.
func TemplateChunkKey(prefix, templateID, sum string) string {
	return TemplatePrefix(prefix, templateID) + "c/" + sum
}

// TemplateBaseKey is the manifest of a template's base.
func TemplateBaseKey(prefix, templateID string) string {
	return TemplatePrefix(prefix, templateID) + "base.json"
}

// ValidateID accepts snapshot ids and session ids.
func ValidateID(kind, id string) error {
	if !safeIDRE.MatchString(id) {
		return fmt.Errorf("invalid %s %q", kind, id)
	}
	return nil
}

// Validate checks the candidate before any object it names is read.
func (c *Candidate) Validate(prefix, snapshotID string) error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("candidate schema %d is not supported", c.SchemaVersion)
	}
	if c.SnapshotID != snapshotID {
		return fmt.Errorf("candidate belongs to %q, not %q", c.SnapshotID, snapshotID)
	}
	if err := ValidateID("session", c.Session); err != nil {
		return err
	}
	if c.ManifestKey != ManifestKey(prefix, snapshotID, c.Session) {
		return fmt.Errorf("candidate manifest key %q is outside its session", c.ManifestKey)
	}
	if !sha256RE.MatchString(c.ManifestSHA256) {
		return fmt.Errorf("candidate manifest digest %q is malformed", c.ManifestSHA256)
	}
	if c.ManifestSize <= 0 || c.ManifestSize > maxManifestLen {
		return fmt.Errorf("candidate manifest size %d is out of range", c.ManifestSize)
	}
	return nil
}

// ParseManifest checks raw against the candidate digest, decodes it and
// validates every field a restore will trust.
func ParseManifest(raw []byte, cand *Candidate, prefix string) (*Manifest, error) {
	if int64(len(raw)) != cand.ManifestSize {
		return nil, fmt.Errorf("manifest is %d bytes, candidate says %d", len(raw), cand.ManifestSize)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != cand.ManifestSHA256 {
		return nil, fmt.Errorf("manifest digest does not match the candidate")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Session != cand.Session {
		return nil, fmt.Errorf("manifest session %q differs from candidate %q", m.Session, cand.Session)
	}
	if err := m.Validate(prefix, cand.SnapshotID); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate rejects a manifest that could write outside the package, leave a
// range undefined, or name an object outside its own session.
func (m *Manifest) Validate(prefix, snapshotID string) error {
	if m.SchemaVersion != SchemaVersion && m.SchemaVersion != SchemaVersionIncremental {
		return fmt.Errorf("manifest schema %d is not supported", m.SchemaVersion)
	}
	if m.SchemaVersion == SchemaVersionIncremental {
		if err := ValidateID("sandbox id", m.SandboxID); err != nil {
			return err
		}
		if m.TemplateID != "" {
			if err := ValidateID("template id", m.TemplateID); err != nil {
				return err
			}
		}
	}
	if m.Generation < 0 || (m.Generation > 0 && m.SchemaVersion != SchemaVersionIncremental) {
		return fmt.Errorf("manifest generation %d is invalid", m.Generation)
	}
	if m.SnapshotID != snapshotID {
		return fmt.Errorf("manifest belongs to %q, not %q", m.SnapshotID, snapshotID)
	}
	if err := ValidateID("snapshot id", m.SnapshotID); err != nil {
		return err
	}
	if err := ValidateID("session", m.Session); err != nil {
		return err
	}
	if m.ChunkSize < minChunkSize || m.ChunkSize > maxChunkSize {
		return fmt.Errorf("chunk size %d is out of range", m.ChunkSize)
	}
	if len(m.Files) == 0 || len(m.Files) > maxFiles {
		return fmt.Errorf("manifest lists %d files", len(m.Files))
	}
	seen := map[string]bool{}
	chunks := 0
	keyPrefix := SessionPrefix(prefix, m.SnapshotID, m.Session) + "c/"
	// A version-2 chunk may also live in the sandbox's own lineage or its
	// template's base, never anywhere else. A manifest with a generation
	// names lineage chunks of that generation or older only; one without
	// names only chunks from before generations.
	lineage, tpl := "", ""
	if m.SchemaVersion == SchemaVersionIncremental {
		lineage = LineagePrefix(prefix, m.SandboxID)
		if m.TemplateID != "" {
			tpl = TemplatePrefix(prefix, m.TemplateID) + "c/"
		}
	}
	keyAllowed := func(key, sum string) bool {
		if key == keyPrefix+sum || (tpl != "" && key == tpl+sum) {
			return true
		}
		if lineage == "" {
			return false
		}
		gen, s, ok := parseLineageKey(lineage, key)
		if !ok || s != sum {
			return false
		}
		if m.Generation == 0 {
			return gen == 0
		}
		return gen > 0 && gen <= m.Generation
	}
	for i := range m.Files {
		f := &m.Files[i]
		switch f.Kind {
		case "", KindRootfs, KindMemory:
		default:
			return fmt.Errorf("file %q has unknown kind %q", f.Name, f.Kind)
		}
		switch f.Role {
		case RoleVolume:
			if !safeNameRE.MatchString(f.Name) {
				return fmt.Errorf("volume name %q is invalid", f.Name)
			}
		case RoleMeta:
			if err := validateMetaPath(f.Name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("file %q has unknown role %q", f.Name, f.Role)
		}
		id := f.Role + ":" + f.Name
		if seen[id] {
			return fmt.Errorf("file %s is listed twice", id)
		}
		seen[id] = true
		if f.Size < 0 || f.Size > maxFileSize {
			return fmt.Errorf("file %s size %d is out of range", id, f.Size)
		}
		if f.ChunkSize != 0 && (f.ChunkSize < minChunkSize || f.ChunkSize > maxChunkSize) {
			return fmt.Errorf("file %s chunk size %d is out of range", id, f.ChunkSize)
		}
		var end int64
		for j, c := range f.Chunks {
			if c.Offset < end {
				return fmt.Errorf("file %s chunk %d overlaps or is out of order", id, j)
			}
			if c.Length <= 0 || c.Length > m.fileChunkSize(f) {
				return fmt.Errorf("file %s chunk %d length %d is out of range", id, j, c.Length)
			}
			if c.Offset > f.Size-c.Length {
				return fmt.Errorf("file %s chunk %d ends past the file", id, j)
			}
			if !sha256RE.MatchString(c.SHA256) {
				return fmt.Errorf("file %s chunk %d digest is malformed", id, j)
			}
			if !keyAllowed(c.Key, c.SHA256) {
				return fmt.Errorf("file %s chunk %d key %q is outside its session", id, j, c.Key)
			}
			end = c.Offset + c.Length
		}
		chunks += len(f.Chunks)
		if chunks > maxChunks {
			return fmt.Errorf("manifest lists more than %d chunks", maxChunks)
		}
	}
	return nil
}

// FindFile returns the file with role and name.
func (m *Manifest) FindFile(role, name string) *File {
	for i := range m.Files {
		if m.Files[i].Role == role && m.Files[i].Name == name {
			return &m.Files[i]
		}
	}
	return nil
}

func validateMetaPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || path.Clean(p) != p {
		return fmt.Errorf("metadata path %q is not a clean relative path", p)
	}
	parts := strings.Split(p, "/")
	if len(parts) > maxMetaDepth {
		return fmt.Errorf("metadata path %q is too deep", p)
	}
	for _, part := range parts {
		if part == "." || part == ".." || !safeNameRE.MatchString(part) {
			return fmt.Errorf("metadata path %q has an invalid component %q", p, part)
		}
	}
	return nil
}

func sortChunks(chunks []Chunk) {
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Offset < chunks[j].Offset })
}
