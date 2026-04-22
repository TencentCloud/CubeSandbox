// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package netfile

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
)

var _testingLock = &sync.Mutex{}

func loadNetfileTestConfig(t *testing.T, content string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err)

	_, err = config.Init(path, false)
	require.NoError(t, err)
}

func BenchmarkNetfileHash(b *testing.B) {
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			{
				Name: "test",
				HostAliases: []*cubebox.HostAlias{
					{
						Ip:        "192.168.1.1",
						Hostnames: []string{"example.com", "example.org"},
					},
					{
						Ip:        "192.168.1.2",
						Hostnames: []string{"example.net"},
					},
				},
				DnsConfig: &cubebox.DNSConfig{
					Servers: []string{"8.8.8.8", "9.9.9.9"},
				},
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cn := &CubeboxNetfile{
			Hostname: "test-hostname",
		}
		err := cn.CreateNetfiles(req)
		assert.NoError(b, err)
	}
}

func TestCubeboxNetfile_CreateNetfiles(t *testing.T) {
	cn := &CubeboxNetfile{
		Hostname: "test-hostname",
	}

	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			{
				Name: "container1",
				HostAliases: []*cubebox.HostAlias{
					{
						Ip:        "192.168.1.1",
						Hostnames: []string{"example.com"},
					},
				},
				DnsConfig: &cubebox.DNSConfig{
					Servers: []string{"8.8.8.8"},
				},
			},
			{
				Name: "container2",
				DnsConfig: &cubebox.DNSConfig{
					Servers: []string{"9.9.9.9"},
				},
			},
		},
	}

	err := cn.CreateNetfiles(req)
	assert.NoError(t, err)
	assert.Len(t, cn.ContainerNetfiles, 2)

	container1Files, exists := cn.ContainerNetfiles["container1"]
	assert.True(t, exists)
	assert.Len(t, container1Files.Files, 3)

	hostsContent := container1Files.Files["/etc/hosts"].Content
	assert.Contains(t, string(hostsContent), "127.0.0.1 localhost test-hostname")
	assert.Contains(t, string(hostsContent), "192.168.1.1 example.com")

	resolvContent := container1Files.Files["/etc/resolv.conf"].Content
	assert.Contains(t, string(resolvContent), "nameserver 8.8.8.8")

	hostnameContent := container1Files.Files["/etc/hostname"].Content
	assert.Equal(t, "test-hostname", string(hostnameContent))

	container2Files, exists := cn.ContainerNetfiles["container2"]
	assert.True(t, exists)
	assert.Len(t, container2Files.Files, 3)

	hostsContent2 := container2Files.Files["/etc/hosts"].Content
	assert.Contains(t, string(hostsContent2), "127.0.0.1 localhost test-hostname")
	assert.NotContains(t, string(hostsContent2), "192.168.1.1")
}

func TestCubeboxNetfile_WriteToHost(t *testing.T) {
	_testingLock.Lock()
	defer _testingLock.Unlock()

	tmpDir := t.TempDir()
	cn := &CubeboxNetfile{
		RootPath: tmpDir,
		ContainerNetfiles: map[string]ContainerNetfile{
			"test-container": {
				Files: map[string]FileContent{
					"/etc/hosts": {
						Path:    "/etc/hosts",
						Content: []byte("127.0.0.1 localhost\n"),
					},
					"/etc/resolv.conf": {
						Path:    "/etc/resolv.conf",
						Content: []byte("nameserver 8.8.8.8\n"),
					},
				},
			},
		},
	}

	err := cn.WriteToHost()
	assert.NoError(t, err)

	hostsPath := filepath.Join(tmpDir, "test-container", "etc", "hosts")
	resolvPath := filepath.Join(tmpDir, "test-container", "etc", "resolv.conf")

	assert.FileExists(t, hostsPath)
	assert.FileExists(t, resolvPath)

	hostsContent, err := os.ReadFile(hostsPath)
	assert.NoError(t, err)
	assert.Equal(t, "127.0.0.1 localhost\n", string(hostsContent))

	resolvContent, err := os.ReadFile(resolvPath)
	assert.NoError(t, err)
	assert.Equal(t, "nameserver 8.8.8.8\n", string(resolvContent))
}

func TestCubeboxNetfile_WriteToHost_EmptyRootPath(t *testing.T) {
	cn := &CubeboxNetfile{
		RootPath: "",
		ContainerNetfiles: map[string]ContainerNetfile{
			"test-container": {
				Files: map[string]FileContent{
					"/etc/hosts": {
						Path:    "/etc/hosts",
						Content: []byte("test content"),
					},
				},
			},
		},
	}

	err := cn.WriteToHost()
	assert.NoError(t, err)
}

func TestCubeboxNetfile_ContainerVirtiofsDirMapping(t *testing.T) {
	tmpDir := t.TempDir()
	cn := &CubeboxNetfile{
		RootPath: tmpDir,
		ContainerNetfiles: map[string]ContainerNetfile{
			"container1": {
				HostDirPath: filepath.Join(tmpDir, "container1"),
				Files:       map[string]FileContent{},
			},
		},
	}

	mapping := cn.ContainerVirtiofsDirMapping("container1")
	require.NotNil(t, mapping)
	assert.Equal(t, tmpDir, mapping.SharePath)
	assert.Equal(t, filepath.Join(filepath.Base(tmpDir), "container1"), mapping.MountPath)

	mapping = cn.ContainerVirtiofsDirMapping("nonexistent")
	assert.Nil(t, mapping)

	cn.RootPath = ""
	mapping = cn.ContainerVirtiofsDirMapping("container1")
	assert.Nil(t, mapping)
}

func TestCubeboxNetfile_ContainerVirtiofsMounts(t *testing.T) {
	tmpDir := t.TempDir()
	cn := &CubeboxNetfile{
		RootPath: tmpDir,
		ContainerNetfiles: map[string]ContainerNetfile{
			"container1": {
				HostDirPath: filepath.Join(tmpDir, "container1"),
				Files: map[string]FileContent{
					"/etc/hosts": {
						Path:    "/etc/hosts",
						Content: []byte("test"),
					},
					"/etc/resolv.conf": {
						Path:    "/etc/resolv.conf",
						Content: []byte("test"),
					},
				},
			},
		},
	}

	mounts := cn.ContainerVirtiofsMounts("container1")
	require.Len(t, mounts, 2)

	for _, mount := range mounts {
		assert.Contains(t, []string{"/etc/hosts", "/etc/resolv.conf"}, mount.ContainerDest)
		assert.Equal(t, "bind", mount.Type)
		assert.Contains(t, mount.Options, "rbind")
		assert.Contains(t, mount.Options, "ro")
	}

	mounts = cn.ContainerVirtiofsMounts("nonexistent")
	assert.Nil(t, mounts)

	cn.RootPath = ""
	mounts = cn.ContainerVirtiofsMounts("container1")
	assert.Nil(t, mounts)
}

func TestCubeboxNetfile_OciContainerNetfileSpec(t *testing.T) {
	ctx := context.Background()
	cn := &CubeboxNetfile{
		ContainerNetfiles: map[string]ContainerNetfile{
			"container1": {
				Files: map[string]FileContent{
					"/etc/hosts": {
						Path:    "/etc/hosts",
						Content: []byte("test content"),
					},
				},
			},
		},
	}

	specOpts := cn.OciContainerNetfileSpec(ctx, "container1")
	assert.NotNil(t, specOpts)

	specOpts = cn.OciContainerNetfileSpec(ctx, "nonexistent")
	assert.Nil(t, specOpts)

	cn.ContainerNetfiles["empty-container"] = ContainerNetfile{
		Files: map[string]FileContent{},
	}
	specOpts = cn.OciContainerNetfileSpec(ctx, "empty-container")
	assert.Nil(t, specOpts)
}

func TestGenHostsFileWithHostName(t *testing.T) {
	hostname := "test-host"
	hostAliases := []*cubebox.HostAlias{
		{
			Ip:        "192.168.1.1",
			Hostnames: []string{"example.com", "example.org"},
		},
		{
			Ip:        "192.168.1.2",
			Hostnames: []string{"example.net"},
		},
	}

	content, err := genHostsFileWithHostName(hostname, hostAliases)
	assert.NoError(t, err)

	expected := `127.0.0.1 localhost test-host
192.168.1.1 example.com
192.168.1.1 example.org
192.168.1.2 example.net
`
	assert.Equal(t, expected, string(content))

	content, err = genHostsFileWithHostName(hostname, nil)
	assert.NoError(t, err)
	assert.Equal(t, "127.0.0.1 localhost test-host\n", string(content))
}

func TestGenResolvContent(t *testing.T) {

	loadNetfileTestConfig(t, "common:\n  default_dns_servers:\n    - 119.29.29.29\n")
	dnsServers := []string{"8.8.8.8", "9.9.9.9"}
	content, err := genResolvContent(dnsServers)
	assert.NoError(t, err)
	assert.Equal(t, "nameserver 8.8.8.8\nnameserver 9.9.9.9\n", string(content))

	loadNetfileTestConfig(t, "common:\n  default_dns_servers:\n    - 1.1.1.1\n    - 119.29.29.29\n")
	content, err = genResolvContent(nil)
	assert.NoError(t, err)
	assert.Equal(t, "nameserver 1.1.1.1\nnameserver 119.29.29.29\n", string(content))

	loadNetfileTestConfig(t, "common: {}\n")
	content, err = genResolvContent(nil)
	assert.NoError(t, err)
	assert.Equal(t, "nameserver 119.29.29.29\n", string(content))

	_, err = genResolvContent([]string{"invalid-ip"})
	assert.Error(t, err)
}

func TestResolveEffectiveDNSServers(t *testing.T) {
	tests := []struct {
		name    string
		cfgYAML string
		req     *cubebox.RunCubeSandboxRequest
		want    []string
		wantErr bool
	}{
		{
			name:    "prefer request dns servers",
			cfgYAML: "common:\n  default_dns_servers:\n    - 119.29.29.29\n",
			req: &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					{DnsConfig: &cubebox.DNSConfig{Servers: []string{"8.8.8.8", " 1.1.1.1 "}}},
					{DnsConfig: &cubebox.DNSConfig{Servers: []string{"1.1.1.1"}}},
				},
			},
			want: []string{"1.1.1.1", "8.8.8.8"},
		},
		{
			name:    "fallback to cubelet default dns servers",
			cfgYAML: "common:\n  default_dns_servers:\n    - 9.9.9.9\n    - 1.1.1.1\n",
			req:     &cubebox.RunCubeSandboxRequest{},
			want:    []string{"1.1.1.1", "9.9.9.9"},
		},
		{
			name:    "fallback to hardcoded dns server",
			cfgYAML: "common: {}\n",
			req:     &cubebox.RunCubeSandboxRequest{},
			want:    []string{"119.29.29.29"},
		},
		{
			name:    "reject invalid request dns server",
			cfgYAML: "common: {}\n",
			req: &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					{DnsConfig: &cubebox.DNSConfig{Servers: []string{"invalid-ip"}}},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loadNetfileTestConfig(t, tt.cfgYAML)
			got, err := ResolveEffectiveDNSServers(tt.req)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFileContent_EmptyContent(t *testing.T) {
	cn := &CubeboxNetfile{
		ContainerNetfiles: map[string]ContainerNetfile{
			"container1": {
				Files: map[string]FileContent{
					"/etc/hosts": {
						Path:    "/etc/hosts",
						Content: []byte{},
					},
				},
			},
		},
	}

	ctx := context.Background()
	specOpts := cn.OciContainerNetfileSpec(ctx, "container1")
	assert.NotNil(t, specOpts)
}
