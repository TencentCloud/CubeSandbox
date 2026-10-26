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

	"github.com/containerd/containerd/v2/pkg/oci"
	jsoniter "github.com/json-iterator/go"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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

// EffectiveDNSConfig is the guest resolv.conf content after merging request
// dns_config with Cubelet defaults. If the request sets any servers, the whole
// config comes from the request (no default search/options mixed in); otherwise
// empty request searches/options fall back to Cubelet defaults.
type EffectiveDNSConfig struct {
	Servers  []string
	Searches []string
	Options  []string
}

// AnnotationLines returns entries for the cube.sandbox.dns annotation.
// Servers stay as bare IPs (shim prefixes nameserver); search/options are full lines.
func (c EffectiveDNSConfig) AnnotationLines() []string {
	lines := make([]string, 0, len(c.Servers)+2)
	if len(c.Searches) > 0 {
		lines = append(lines, "search "+strings.Join(c.Searches, " "))
	}
	lines = append(lines, c.Servers...)
	if len(c.Options) > 0 {
		lines = append(lines, "options "+strings.Join(c.Options, " "))
	}
	return lines
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
	dnsCfg, err := ResolveEffectiveDNSConfig(req)
	if err != nil {
		return fmt.Errorf("failed to resolve effective dns config: %w", err)
	}
	var netfiles = make(map[string]ContainerNetfile)
	for _, c := range req.GetContainers() {
		hosts, err := genHostsFileWithHostName(cn.Hostname, c.GetHostAliases())
		if err != nil {
			return fmt.Errorf("failed to gen hosts file for container %s", c.Name)
		}
		dns, err := genResolvContent(dnsCfg)
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

		d, err := jsoniter.MarshalToString(files)
		if err != nil {
			log.G(ctx).Errorf("container %s marshal netfile files to string failed:%v", containerName, err)
			return nil
		} else {
			log.G(ctx).Infof("container %s use netfile files: %s", containerName, d)
		}
		return oci.WithAnnotations(map[string]string{
			constants.AnnotationShimCustomFile: d,
		})
	}
	return nil
}

// ResolveEffectiveDNSConfig merges request dns_config with Cubelet defaults.
//
// If the request sets any dns_config.servers, the whole DNS config is taken from
// the request (servers plus any request searches/options). Defaults for
// searches/options are not mixed in — explicit nameservers mean the caller owns
// DNS end-to-end.
//
// If the request does not set servers, Cubelet defaults apply: empty request
// searches/options fall back to default_dns_searches / default_dns_options
// (e.g. followNodeDns).
func ResolveEffectiveDNSConfig(req *cubebox.RunCubeSandboxRequest) (EffectiveDNSConfig, error) {
	requestServers, err := requestDNSServers(req)
	if err != nil {
		return EffectiveDNSConfig{}, err
	}
	searches, err := requestDNSSearches(req)
	if err != nil {
		return EffectiveDNSConfig{}, err
	}
	options, err := requestDNSOptions(req)
	if err != nil {
		return EffectiveDNSConfig{}, err
	}

	servers := requestServers
	if len(servers) == 0 {
		servers = defaultDNSServers()
		if len(servers) == 0 {
			servers = []string{defaultDNSIP}
		}
		if len(searches) == 0 {
			searches = defaultDNSSearches()
		}
		if len(options) == 0 {
			options = defaultDNSOptions()
		}
	}

	sort.Slice(servers, func(i, j int) bool {
		return servers[i] < servers[j]
	})

	return EffectiveDNSConfig{
		Servers:  append([]string(nil), servers...),
		Searches: append([]string(nil), searches...),
		Options:  append([]string(nil), options...),
	}, nil
}

func ResolveEffectiveDNSServers(req *cubebox.RunCubeSandboxRequest) ([]string, error) {
	cfg, err := ResolveEffectiveDNSConfig(req)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), cfg.Servers...), nil
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

func genResolvContent(cfg EffectiveDNSConfig) ([]byte, error) {
	if len(cfg.Servers) == 0 {
		resolved, err := ResolveEffectiveDNSConfig(nil)
		if err != nil {
			return nil, err
		}
		// Fill missing fields from Cubelet defaults; keep any caller-provided
		// searches/options (e.g. when only servers were empty).
		cfg.Servers = resolved.Servers
		if len(cfg.Searches) == 0 {
			cfg.Searches = resolved.Searches
		}
		if len(cfg.Options) == 0 {
			cfg.Options = resolved.Options
		}
	}
	servers := append([]string(nil), cfg.Servers...)
	sort.Slice(servers, func(i, j int) bool {
		return servers[i] < servers[j]
	})
	var b bytes.Buffer
	if len(cfg.Searches) > 0 {
		if _, err := b.Write([]byte("search " + strings.Join(cfg.Searches, " ") + "\n")); err != nil {
			return nil, err
		}
	}
	for _, entry := range servers {
		if net.ParseIP(entry) == nil {
			return nil, fmt.Errorf("invalid dns %s", entry)
		}
		if _, err := b.Write([]byte("nameserver " + entry + "\n")); err != nil {
			return nil, err
		}
	}
	if len(cfg.Options) > 0 {
		if _, err := b.Write([]byte("options " + strings.Join(cfg.Options, " ") + "\n")); err != nil {
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

func requestDNSSearches(req *cubebox.RunCubeSandboxRequest) ([]string, error) {
	return requestDNSStringList(req, "search", func(cfg *cubebox.DNSConfig) []string {
		if cfg == nil {
			return nil
		}
		return cfg.GetSearches()
	})
}

func requestDNSOptions(req *cubebox.RunCubeSandboxRequest) ([]string, error) {
	return requestDNSStringList(req, "option", func(cfg *cubebox.DNSConfig) []string {
		if cfg == nil {
			return nil
		}
		return cfg.GetOptions()
	})
}

// validDNSListToken rejects whitespace/control chars and resolv.conf comment
// markers so a single request entry cannot inject extra nameserver lines.
func validDNSListToken(item string) bool {
	for _, r := range item {
		if r <= ' ' || r == 0x7f || r == '#' || r == ';' {
			return false
		}
	}
	return true
}

func requestDNSStringList(req *cubebox.RunCubeSandboxRequest, kind string, get func(*cubebox.DNSConfig) []string) ([]string, error) {
	if req == nil {
		return nil, nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, ctr := range req.GetContainers() {
		for _, item := range get(ctr.GetDnsConfig()) {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if !validDNSListToken(item) {
				return nil, fmt.Errorf("invalid dns %s %q", kind, item)
			}
			if _, ok := seen[item]; ok {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out, nil
}

func defaultDNSServers() []string {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Common == nil || len(cfg.Common.DefaultDNSServers) == 0 {
		return nil
	}
	return append([]string(nil), cfg.Common.DefaultDNSServers...)
}

func defaultDNSSearches() []string {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Common == nil || len(cfg.Common.DefaultDNSSearches) == 0 {
		return nil
	}
	return append([]string(nil), cfg.Common.DefaultDNSSearches...)
}

func defaultDNSOptions() []string {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Common == nil || len(cfg.Common.DefaultDNSOptions) == 0 {
		return nil
	}
	return append([]string(nil), cfg.Common.DefaultDNSOptions...)
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
