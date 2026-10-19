package tcclient

import (
	"encoding/json"
	images "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func (r *UploadArtifactResponse) UnmarshalJSON(data []byte) error {
	type plain UploadArtifactResponse
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	metadata, err := images.DecodeRootfsMetadata(data)
	if err != nil {
		return err
	}
	r.RootfsFormat, r.RootfsPath, r.RootfsSHA256, r.RootfsSizeBytes = metadata.Format, metadata.Path, metadata.SHA256, metadata.SizeBytes
	return nil
}

func (r UploadArtifactResponse) MarshalJSON() ([]byte, error) {
	type plain UploadArtifactResponse
	legacy := images.RootfsMetadata{}
	if r.RootfsFormat == "" || r.RootfsFormat == "ext4" {
		legacy = images.RootfsMetadata{Path: r.RootfsPath, SHA256: r.RootfsSHA256, SizeBytes: r.RootfsSizeBytes}
	}
	return json.Marshal(struct {
		plain
		LegacyPath      string `json:"ext4_path,omitempty"`
		LegacySHA256    string `json:"ext4_sha256,omitempty"`
		LegacySizeBytes int64  `json:"ext4_size_bytes,omitempty"`
	}{plain(r), legacy.Path, legacy.SHA256, legacy.SizeBytes})
}
