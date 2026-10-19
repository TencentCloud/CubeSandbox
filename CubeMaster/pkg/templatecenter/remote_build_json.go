package templatecenter

import (
	"encoding/json"
	"strings"

	images "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// UnmarshalJSON shares the compatibility boundary between live callbacks and
// persisted reports replayed by the reconciler.
func (r *RemoteBuildResult) UnmarshalJSON(data []byte) error {
	type plain RemoteBuildResult
	var wire struct {
		plain
		LegacyPath      string `json:"ext4_path"`
		LegacySHA256    string `json:"ext4_sha256"`
		LegacySizeBytes int64  `json:"ext4_size_bytes"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	metadata, err := images.ResolveLegacyRootfsMetadata(
		images.RootfsMetadata{Format: wire.RootfsFormat, Path: strings.TrimSpace(wire.RootfsPath), SHA256: strings.TrimSpace(wire.RootfsSHA256), SizeBytes: wire.RootfsSizeBytes},
		images.RootfsMetadata{Path: strings.TrimSpace(wire.LegacyPath), SHA256: strings.TrimSpace(wire.LegacySHA256), SizeBytes: wire.LegacySizeBytes})
	if err != nil {
		return err
	}
	*r = RemoteBuildResult(wire.plain)
	r.RootfsFormat, r.RootfsPath, r.RootfsSHA256, r.RootfsSizeBytes = metadata.Format, metadata.Path, metadata.SHA256, metadata.SizeBytes
	r.ArtifactID = strings.TrimSpace(r.ArtifactID)
	r.TemplateSpecFingerprint = strings.TrimSpace(r.TemplateSpecFingerprint)
	r.SourceImageDigest = strings.TrimSpace(r.SourceImageDigest)
	r.ImageConfigJSON = strings.TrimSpace(r.ImageConfigJSON)
	r.MasterNodeIP = strings.TrimSpace(r.MasterNodeIP)
	r.ArtifactURL = strings.TrimSpace(r.ArtifactURL)
	r.StorageBackend = strings.TrimSpace(r.StorageBackend)
	r.ObjectKey = strings.TrimSpace(r.ObjectKey)
	r.CubeEgressCAFingerprint = strings.TrimSpace(r.CubeEgressCAFingerprint)
	return nil
}
