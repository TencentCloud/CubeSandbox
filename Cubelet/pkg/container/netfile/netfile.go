// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package netfile

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"encoding/json"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

var (
	oldNetfilePath string
)

const (
	netfilePathResolv = "/etc/resolv.conf"
	netfilePathHosts  = "/etc/hosts"
	hostnameFilePath  = "/etc/hostname"
	defaultDNSIP      = "119.29.29.29"
)

func Init(oldDir string) error {
	oldNetfilePath = oldDir
	return nil
}

type FileContent struct {
	Path    string `json:"path,omitempty"`
	Content []byte `json:"content,omitempty"`
}

type ContainerNetfile struct {
	HostDirPath string
	Files       map[string]FileContent
}

type CubeboxNetfile struct {
	RootPath string
	Hostname string

	ContainerNetfiles map[string]ContainerNetfile
}

func (c *CubeboxNetfile) WriteToHost() error {
	for key := range c.ContainerNetfiles {
		cnf := c.ContainerNetfiles[key]
		cnf.HostDirPath = path.Join(c.RootPath, key)
		for _, cf := range cnf.Files {
			filePath := path.Clean(path.Join(cnf.HostDirPath, cf.Path))
			dirname := path.Dir(filePath)
			if err := os.MkdirAll(dirname, os.ModeDir|0755); err != nil {
				return fmt.Errorf("create local netfile dir %s failed, %s", dirname, err.Error())
			}
			if err := os.WriteFile(filePath, cf.Content, 0644); err != nil {
				return fmt.Errorf("write local netfile %s failed, %s", filePath, err.Error())
			}
		}

		c.ContainerNetfiles[key] = cnf
	}
	return nil
}

func (cn *CubeboxNetfile) CreateNetfiles(req *cubebox.RunCubeSandboxRequest) error {
	dnsServers, err := ResolveEffectiveDNSServers(req)
	if err != nil {
		return fmt.Errorf("failed to resolve effective dns servers: %w", err)
	}
	var netfiles = make(map[string]ContainerNetfile)
	for _, c := range req.GetContainers() {
		hosts, err := genHostsFileWithHostName(cn.Hostname, c.GetHostAliases())
		if err != nil {
			return fmt.Errorf("failed to gen hosts file for container %s", c.Name)
		}
		dns, err := genResolvContent(dnsServers)
		if err != nil {
			return fmt.Errorf("failed to gen resolv.conf for container %s", c.Name)
		}
		netfiles[c.Name] = ContainerNetfile{
			Files: map[string]FileContent{
				netfilePathHosts: {
					Path:    netfilePathHosts,
					Content: hosts,
				},
				netfilePathResolv: {
					Path:    netfilePathResolv,
					Content: dns,
				},
				hostnameFilePath: {
					Path:    hostnameFilePath,
					Content: []byte(cn.Hostname),
				},
			},
		}
	}
	cn.ContainerNetfiles = netfiles
	return nil
}

func (cn *CubeboxNetfile) ContainerVirtiofsDirMaping(containerName string) *virtiofs.ShareDirMapping {
	if cn.RootPath == "" {
		return nil
	}
	containerFiles, ok := cn.ContainerNetfiles[containerName]
	if !ok {
		return nil
	}
	if containerFiles.HostDirPath == "" {
		return nil
	}

	return &virtiofs.ShareDirMapping{

		SharePath: cn.RootPath,

		MountPath: path.Join(path.Base(cn.RootPath), containerName),
	}
}

func (cn *CubeboxNetfile) ContainerVirtiofsMounts(containerName string) []virtiofs.CubeRootfsMount {
	if cn.RootPath == "" {
		return nil
	}
	var (
		containerFiles, ok = cn.ContainerNetfiles[containerName]
		mounts             []virtiofs.CubeRootfsMount
	)
	if !ok {
		return nil
	}
	for _, cf := range containerFiles.Files {
		mounts = append(mounts, virtiofs.CubeRootfsMount{
			HostSource:     filepath.Clean(filepath.Join(containerFiles.HostDirPath, cf.Path)),
			VirtiofsSource: filepath.Clean(filepath.Join(path.Base(cn.RootPath), containerName, cf.Path)),
			ContainerDest:  cf.Path,
			Type:           constants.MountTypeBind,
			Options:        []string{constants.MountOptBindRO, constants.MountOptReadOnly},
		})
	}

	return mounts
}

func (cn *CubeboxNetfile) OciContainerNetfileSpec(ctx context.Context, containerName string) oci.SpecOpts {
	if cn.RootPath != "" {
		return nil
	}
	if cf, ok := cn.ContainerNetfiles[containerName]; ok {
		var files []FileContent
		if len(cf.Files) == 0 {
			log.G(ctx).Errorf("container %s none netfile files", containerName)
			return nil
		}
		for _, f := range cf.Files {
			files = append(files, f)
		}

		d, err := json.Marshal(files)
		dStr := string(d)
		if err != nil {
			log.G(ctx).Errorf("container %s marshal netfile files to string failed:%v", containerName, err)
			return nil
		} else {
			log.G(ctx).Infof("container %s use netfile files: %s", containerName, dStr)
		}
		return oci.WithAnnotations(map[string]string{
			constants.AnnotationShimCustomFile: dStr,
		})
	}
	return nil
}

func ResolveEffectiveDNSServers(req *cubebox.RunCubeSandboxRequest) ([]string, error) {
	dns, err := requestDNSServers(req)
	if err != nil {
		return nil, err
	}
	if len(dns) == 0 {
		dns = defaultDNSServers()
	}
	if len(dns) == 0 {
		dns = []string{defaultDNSIP}
	}
	sort.Slice(dns, func(i, j int) bool {
		return dns[i] < dns[j]
	})
	return append([]string(nil), dns...), nil
}

func genHostsFileWithHostName(hostname string, hosts []*cubebox.HostAlias) ([]byte, error) {
	var b bytes.Buffer
	if _, err := b.Write([]byte(fmt.Sprintf("127.0.0.1 localhost %s\n", hostname))); err != nil {
		return nil, err
	}

	for _, h := range hosts {
		sort.Slice(h.Hostnames, func(i, j int) bool {
			return h.Hostnames[i] < h.Hostnames[j]
		})
	}
	sort.Slice(hosts, func(i, j int) bool {
		return hosts[i].Ip < hosts[j].Ip
	})

	for _, h := range hosts {
		hostnames := h.GetHostnames()

		for _, l := range hostnames {
			line := fmt.Sprintf("%s %s\n", h.GetIp(), l)
			if _, err := b.Write([]byte(line)); err != nil {
				return nil, err
			}
		}
	}

	return b.Bytes(), nil
}

func genResolvContent(dns []string) ([]byte, error) {
	var err error
	if len(dns) == 0 {
		dns, err = ResolveEffectiveDNSServers(nil)
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(dns, func(i, j int) bool {
		return dns[i] < dns[j]
	})
	var b bytes.Buffer
	for _, entry := range dns {
		if net.ParseIP(entry) == nil {
			return nil, fmt.Errorf("invalid dns %s", entry)
		}
		if _, err := b.Write([]byte("nameserver " + entry + "\n")); err != nil {
			return nil, err
		}
	}

	return b.Bytes(), nil
}

func requestDNSServers(req *cubebox.RunCubeSandboxRequest) ([]string, error) {
	if req == nil {
		return nil, nil
	}
	seen := make(map[string]struct{})
	dnsServers := make([]string, 0)
	for _, ctr := range req.GetContainers() {
		for _, server := range ctr.GetDnsConfig().GetServers() {
			server = strings.TrimSpace(server)
			if server == "" {
				continue
			}
			if net.ParseIP(server) == nil {
				return nil, fmt.Errorf("invalid dns %s", server)
			}
			if _, ok := seen[server]; ok {
				continue
			}
			seen[server] = struct{}{}
			dnsServers = append(dnsServers, server)
		}
	}
	return dnsServers, nil
}

func defaultDNSServers() []string {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Common == nil || len(cfg.Common.DefaultDNSServers) == 0 {
		return nil
	}
	return append([]string(nil), cfg.Common.DefaultDNSServers...)
}

func Clean(ctx context.Context, containerID string) error {
	// 1.2.1 路径穿越防护：校验 containerID 不含路径穿越字符
	dir, err := utils.SafeJoinPath(oldNetfilePath, containerID)
	if err != nil {
		return fmt.Errorf("Clean netfile: %w", err)
	}
	if ok, err := utils.DenExist(dir); err == nil && ok {
		return os.RemoveAll(dir)
	}
	return nil
}
