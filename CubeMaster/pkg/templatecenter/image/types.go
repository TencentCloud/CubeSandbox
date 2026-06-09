// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import "context"

type SourceSpec struct {
	ImageRef         string
	RegistryUsername string
	RegistryPassword string
	DownloadBaseURL  string
}

type BuildOptions struct {
	ArtifactID string
}

type BuildResult struct {
	Ext4Path  string
	SHA256    string
	SizeBytes int64
}

type dockerInspectImage struct {
	ID          string            `json:"Id"`
	RepoDigests []string          `json:"RepoDigests"`
	Config      DockerImageConfig `json:"Config"`
}

type skopeoInspectImage struct {
	Name       string               `json:"Name"`
	Digest     string               `json:"Digest"`
	LayersData []skopeoInspectLayer `json:"LayersData"`
}

// skopeoInspectLayer mirrors a single entry of the LayersData array returned by
// `skopeo inspect`. Size is the compressed (on-registry) size of the layer blob
// in bytes.
type skopeoInspectLayer struct {
	Size int64 `json:"Size"`
}

type skopeoInspectConfig struct {
	Config DockerImageConfig `json:"config"`
}

type DockerImageConfig struct {
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	WorkingDir string   `json:"WorkingDir"`
	User       string   `json:"User"`
}

type PreparedSource struct {
	LocalRef       string
	Digest         string
	Config         DockerImageConfig
	ConfigJSON     string
	MasterNodeIP   string
	UseDockerless  bool
	SkopeoAuthFile string
	// compressedSizeBytes is the sum of the compressed layer blob sizes reported
	// by `skopeo inspect` (LayersData[].Size). It is only populated on the
	// dockerless path and lets the disk-space pre-check estimate the image size
	// without invoking the docker daemon. Zero means "unknown".
	CompressedSizeBytes int64
	Cleanup             func(context.Context)
}
