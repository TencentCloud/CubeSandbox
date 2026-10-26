// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !windows

package config

import (
	"github.com/containerd/containerd/v2/defaults"
	"github.com/pelletier/go-toml/v2"
)

func DefaultImageConfig() ImageConfig {
	return ImageConfig{
		Snapshotter:                defaults.DefaultSnapshotter,
		DisableSnapshotAnnotations: false,
		MaxConcurrentDownloads:     10,
		ImageDecryption: ImageDecryption{
			KeyModel: KeyModelNode,
		},
		PinnedImages: map[string]string{
			"sandbox": DefaultSandboxImage,
		},
		ImagePullProgressTimeout: defaultImagePullProgressTimeoutDuration.String(),
		ImagePullWithSyncFs:      false,
		StatsCollectPeriod:       10,
	}
}

func DefaultRuntimeConfig() RuntimeConfig {
	defaultRuncV2Opts := `
	# NoNewKeyring disables new keyring for the container.
	NoNewKeyring = false

	# ShimCgroup places the shim in a cgroup.
	ShimCgroup = ""

	# IoUid sets the I/O's pipes uid.
	IoUid = 0

	# IoGid sets the I/O's pipes gid.
	IoGid = 0

	# BinaryName is the binary name of the runc binary.
	BinaryName = ""

	# Root is the runc root directory.
	Root = ""

	# CriuImagePath is the criu image path
	CriuImagePath = ""

	# CriuWorkPath is the criu work path.
	CriuWorkPath = ""
`
	var m map[string]interface{}
	toml.Unmarshal([]byte(defaultRuncV2Opts), &m)

	return RuntimeConfig{
		CniConfig: CniConfig{
			NetworkPluginBinDir:        "/opt/cni/bin",
			NetworkPluginConfDir:       "/etc/cni/net.d",
			NetworkPluginMaxConfNum:    1,
			NetworkPluginSetupSerially: false,
			NetworkPluginConfTemplate:  "",
			UseInternalLoopback:        false,
		},
		ContainerdConfig: ContainerdConfig{
			DefaultRuntimeName: "runc",
			Runtimes: map[string]Runtime{
				"runc": {
					Type:      "io.containerd.runc.v2",
					Options:   m,
					Sandboxer: string(ModePodSandbox),
				},
			},
		},
		EnableSelinux:                    false,
		SelinuxCategoryRange:             1024,
		MaxContainerLogLineSize:          16 * 1024,
		DisableProcMount:                 false,
		TolerateMissingHugetlbController: true,
		DisableHugetlbController:         true,
		IgnoreImageDefinedVolumes:        false,
		EnableCDI:                        true,
		CDISpecDirs:                      []string{"/etc/cdi", "/var/run/cdi"},
		DrainExecSyncIOTimeout:           "0s",
		EnableUnprivilegedPorts:          true,
		EnableUnprivilegedICMP:           true,
	}
}
