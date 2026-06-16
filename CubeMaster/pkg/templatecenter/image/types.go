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
	// OnPullProgress, when non-nil, receives best-effort source-image pull
	// progress updates while the image is fetched (docker pull on the docker
	// path, skopeo copy on the dockerless path). A nil value disables progress
	// streaming and preserves the original buffered-exec behaviour.
	OnPullProgress ProgressFunc
}

type BuildOptions struct {
	ArtifactID string
	// PostRootfsExport is invoked after the image rootfs has been exported to
	// the working directory but before mkfs.ext4 runs, so callers can mutate
	// the rootfs (e.g. bake the CubeEgress root CA into the trust store). The
	// callback receives the rootfs directory path that mkfs.ext4 will consume.
	// A non-nil error aborts the build.
	PostRootfsExport func(ctx context.Context, rootfsDir string) error
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
	// OnPullProgress is propagated from SourceSpec so that the export phase
	// (skopeo copy on the dockerless path) can stream pull progress even
	// though it runs after PrepareSource has returned.
	OnPullProgress ProgressFunc
}
