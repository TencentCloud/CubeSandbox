// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// This file hand-writes the deep copy of CreateCubeSandboxReq. CubeMaster
// clones a template request on every cache hit, i.e. on every sandbox create,
// and the previous implementation round-tripped the whole request through
// encoding/json. That is a full serialize plus parse per request for a copy
// that never leaves the process; the CPU profile in the PR shows it at ~5.5%
// of create time on a 384-core master.
//
// The copy must be deep, not just a struct assignment: callers mutate the
// result (annotations, container spec, volumes) while the original stays in
// templateDefinitionCache and is handed to concurrent creates.
//
// Every DeepCopy below therefore starts from a value copy and then replaces
// each reference-typed field it inherited. Starting from the value copy (rather
// than from an explicit literal) is what makes the guard in deepcopy_test.go
// self-extending: a pointer, slice or map added to any of these types later is
// inherited by reference, so the reflection test fails until the copy line is
// written, instead of quietly handing out a request that aliases the cached
// one.

// DeepCopy returns a request that shares no mutable state with r. The result
// marshals to the same JSON as r, with a single exception: when r.Request is
// nil it stays nil, whereas the round trip this replaced always materialized
// &Request{} (UnmarshalJSON does that unconditionally) and so emitted
// `"requestID":""` for an input that has no requestID key at all. Nothing
// serializes a request with its embedded request dropped, so the difference is
// not reachable, but it is pinned by
// TestCreateCubeSandboxReqDeepCopyKeepsNilRequest so the claim stays honest.
func (r *CreateCubeSandboxReq) DeepCopy() *CreateCubeSandboxReq {
	if r == nil {
		return nil
	}
	out := *r
	if r.Request != nil {
		request := *r.Request
		out.Request = &request
	}
	out.Timeout = cloneIntPtr(r.Timeout)
	out.Volumes = copyPtrSlice(r.Volumes, (*Volume).DeepCopy)
	out.CubeNetworkConfig = r.CubeNetworkConfig.DeepCopy()
	out.Containers = copyPtrSlice(r.Containers, (*Container).DeepCopy)
	out.Annotations = maps.Clone(r.Annotations)
	out.Labels = maps.Clone(r.Labels)
	out.CreateTimeEnvVars = maps.Clone(r.CreateTimeEnvVars)
	out.DistributionScope = slices.Clone(r.DistributionScope)
	if r.RestartBackoff != nil {
		out.RestartBackoff = proto.Clone(r.RestartBackoff).(*cubeboxv1.RestartBackoffConfig)
	}
	if r.LivenessProbe != nil {
		out.LivenessProbe = proto.Clone(r.LivenessProbe).(*cubeboxv1.LivenessProbe)
	}
	return &out
}

// DeepCopy returns a volume that shares no mutable state with v.
func (v *Volume) DeepCopy() *Volume {
	if v == nil {
		return nil
	}
	out := *v
	out.VolumeSource = v.VolumeSource.DeepCopy()
	return &out
}

// DeepCopy returns a volume source that shares no mutable state with s.
func (s *VolumeSource) DeepCopy() *VolumeSource {
	if s == nil {
		return nil
	}
	out := *s
	out.EmptyDir = s.EmptyDir.DeepCopy()
	out.SandboxPath = s.SandboxPath.DeepCopy()
	out.HostDirVolumeSources = s.HostDirVolumeSources.DeepCopy()
	out.PluginVolume = s.PluginVolume.DeepCopy()
	if s.Image != nil {
		out.Image = proto.Clone(s.Image).(*imagev1.ImageVolumeSource)
	}
	return &out
}

// DeepCopy returns a host directory source list that shares no mutable state
// with s.
func (s *HostDirVolumeSources) DeepCopy() *HostDirVolumeSources {
	if s == nil {
		return nil
	}
	out := *s
	out.VolumeSources = copyPtrSlice(s.VolumeSources, (*HostDirSource).DeepCopy)
	return &out
}

// DeepCopy returns a host directory source that shares no mutable state with s.
func (s *HostDirSource) DeepCopy() *HostDirSource {
	if s == nil {
		return nil
	}
	out := *s
	return &out
}

// DeepCopy returns an empty-dir source that shares no mutable state with s.
func (s *EmptyDirVolumeSource) DeepCopy() *EmptyDirVolumeSource {
	if s == nil {
		return nil
	}
	out := *s
	return &out
}

// DeepCopy returns a sandbox-path source that shares no mutable state with s.
func (s *SandboxPathVolumeSource) DeepCopy() *SandboxPathVolumeSource {
	if s == nil {
		return nil
	}
	out := *s
	return &out
}

// DeepCopy returns a plugin volume that shares no mutable state with s.
func (s *PluginVolumeSource) DeepCopy() *PluginVolumeSource {
	if s == nil {
		return nil
	}
	out := *s
	out.Options = maps.Clone(s.Options)
	return &out
}

// DeepCopy returns a container that shares no mutable state with c.
func (c *Container) DeepCopy() *Container {
	if c == nil {
		return nil
	}
	out := *c
	out.Image = c.Image.DeepCopy()
	out.Command = slices.Clone(c.Command)
	out.Args = slices.Clone(c.Args)
	out.Envs = copyPtrSlice(c.Envs, (*KeyValue).DeepCopy)
	if c.VolumeMounts != nil {
		out.VolumeMounts = make([]*cubeboxv1.VolumeMounts, len(c.VolumeMounts))
		for i, mount := range c.VolumeMounts {
			if mount != nil {
				out.VolumeMounts[i] = proto.Clone(mount).(*cubeboxv1.VolumeMounts)
			}
		}
	}
	out.RLimit = c.RLimit.DeepCopy()
	out.Resources = c.Resources.DeepCopy()
	out.SecurityContext = c.SecurityContext.DeepCopy()
	out.Probe = c.Probe.DeepCopy()
	if c.LivenessProbe != nil {
		out.LivenessProbe = proto.Clone(c.LivenessProbe).(*cubeboxv1.LivenessProbe)
	}
	out.Sysctls = maps.Clone(c.Sysctls)
	out.Syscalls = copyPtrSlice(c.Syscalls, (*SysCall).DeepCopy)
	out.DnsConfig = c.DnsConfig.DeepCopy()
	out.Annotations = maps.Clone(c.Annotations)
	out.HostAliases = copyPtrSlice(c.HostAliases, (*HostAlias).DeepCopy)
	out.Prestop = c.Prestop.DeepCopy()
	out.Poststop = c.Poststop.DeepCopy()
	out.Hooks = c.Hooks.DeepCopy()
	return &out
}

// DeepCopy returns an image spec that shares no mutable state with s.
func (s *ImageSpec) DeepCopy() *ImageSpec {
	if s == nil {
		return nil
	}
	out := *s
	out.Annotations = maps.Clone(s.Annotations)
	return &out
}

// DeepCopy returns a key/value pair that shares no mutable state with k.
func (k *KeyValue) DeepCopy() *KeyValue {
	if k == nil {
		return nil
	}
	out := *k
	return &out
}

// DeepCopy returns a resource request that shares no mutable state with r.
func (r *Resource) DeepCopy() *Resource {
	if r == nil {
		return nil
	}
	out := *r
	out.Limit = r.Limit.DeepCopy()
	return &out
}

// DeepCopy returns a resource limit that shares no mutable state with l.
func (l *RequestLimit) DeepCopy() *RequestLimit {
	if l == nil {
		return nil
	}
	out := *l
	return &out
}

// DeepCopy returns an rlimit that shares no mutable state with l.
func (l *RLimit) DeepCopy() *RLimit {
	if l == nil {
		return nil
	}
	out := *l
	return &out
}

// DeepCopy returns a security context that shares no mutable state with s.
func (s *ContainerSecurityContext) DeepCopy() *ContainerSecurityContext {
	if s == nil {
		return nil
	}
	out := *s
	out.Capabilities = s.Capabilities.DeepCopy()
	out.RunAsUser = s.RunAsUser.DeepCopy()
	out.RunAsGroup = s.RunAsGroup.DeepCopy()
	return &out
}

// DeepCopy returns a capability set that shares no mutable state with c.
func (c *Capability) DeepCopy() *Capability {
	if c == nil {
		return nil
	}
	out := *c
	out.AddCapabilities = slices.Clone(c.AddCapabilities)
	out.DropCapabilities = slices.Clone(c.DropCapabilities)
	out.AddAmbientCapabilities = slices.Clone(c.AddAmbientCapabilities)
	return &out
}

// DeepCopy returns an int64 wrapper that shares no mutable state with v.
func (v *Int64Value) DeepCopy() *Int64Value {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

// DeepCopy returns a probe that shares no mutable state with p.
func (p *Probe) DeepCopy() *Probe {
	if p == nil {
		return nil
	}
	out := *p
	out.ProbeHandler = p.ProbeHandler.DeepCopy()
	return &out
}

// DeepCopy returns a probe handler that shares no mutable state with h.
func (h *ProbeHandler) DeepCopy() *ProbeHandler {
	if h == nil {
		return nil
	}
	out := *h
	out.TCPSocket = h.TCPSocket.DeepCopy()
	out.Ping = h.Ping.DeepCopy()
	out.HttpGet = h.HttpGet.DeepCopy()
	return &out
}

// DeepCopy returns an HTTP probe action that shares no mutable state with a.
func (a *HTTPGetAction) DeepCopy() *HTTPGetAction {
	if a == nil {
		return nil
	}
	out := *a
	out.Path = cloneStringPtr(a.Path)
	out.Host = cloneStringPtr(a.Host)
	out.HttpHeaders = copyPtrSlice(a.HttpHeaders, (*HTTPHeader).DeepCopy)
	return &out
}

// DeepCopy returns an HTTP header that shares no mutable state with h.
func (h *HTTPHeader) DeepCopy() *HTTPHeader {
	if h == nil {
		return nil
	}
	out := *h
	out.Name = cloneStringPtr(h.Name)
	out.Value = cloneStringPtr(h.Value)
	return &out
}

// DeepCopy returns a TCP socket probe that shares no mutable state with a.
func (a *TCPSocketAction) DeepCopy() *TCPSocketAction {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// DeepCopy returns a ping probe that shares no mutable state with a.
func (a *PingAction) DeepCopy() *PingAction {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// DeepCopy returns a seccomp syscall rule that shares no mutable state with s.
func (s *SysCall) DeepCopy() *SysCall {
	if s == nil {
		return nil
	}
	out := *s
	out.Names = slices.Clone(s.Names)
	out.Args = copyPtrSlice(s.Args, (*LinuxSeccompArg).DeepCopy)
	return &out
}

// DeepCopy returns a seccomp argument that shares no mutable state with a.
func (a *LinuxSeccompArg) DeepCopy() *LinuxSeccompArg {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// DeepCopy returns a DNS config that shares no mutable state with c.
func (c *DNSConfig) DeepCopy() *DNSConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.Servers = slices.Clone(c.Servers)
	out.Searches = slices.Clone(c.Searches)
	out.Options = slices.Clone(c.Options)
	return &out
}

// DeepCopy returns an /etc/hosts alias that shares no mutable state with a.
func (a *HostAlias) DeepCopy() *HostAlias {
	if a == nil {
		return nil
	}
	out := *a
	out.Hostnames = slices.Clone(a.Hostnames)
	return &out
}

// DeepCopy returns a pre-stop hook that shares no mutable state with p.
func (p *PreStop) DeepCopy() *PreStop {
	if p == nil {
		return nil
	}
	out := *p
	out.LifecyleHandler = p.LifecyleHandler.DeepCopy()
	return &out
}

// DeepCopy returns a post-stop hook that shares no mutable state with p.
func (p *PostStop) DeepCopy() *PostStop {
	if p == nil {
		return nil
	}
	out := *p
	out.LifecyleHandler = p.LifecyleHandler.DeepCopy()
	return &out
}

// DeepCopy returns a lifecycle handler that shares no mutable state with h.
func (h *LifecycleHandler) DeepCopy() *LifecycleHandler {
	if h == nil {
		return nil
	}
	out := *h
	out.HttpGet = h.HttpGet.DeepCopy()
	return &out
}

// DeepCopy returns a hook set that shares no mutable state with h.
func (h *Hooks) DeepCopy() *Hooks {
	if h == nil {
		return nil
	}
	out := *h
	out.Prestart = copyPtrSlice(h.Prestart, (*Hook).DeepCopy)
	return &out
}

// DeepCopy returns a prestart hook that shares no mutable state with h.
func (h *Hook) DeepCopy() *Hook {
	if h == nil {
		return nil
	}
	out := *h
	out.Args = slices.Clone(h.Args)
	out.Env = slices.Clone(h.Env)
	out.Timeout = cloneInt32Ptr(h.Timeout)
	return &out
}

// copyPtrSlice clones a slice of pointers with the element's own DeepCopy. A
// nil slice stays nil so the copy marshals exactly like the original.
func copyPtrSlice[T any](in []*T, deepCopy func(*T) *T) []*T {
	if in == nil {
		return nil
	}
	out := make([]*T, len(in))
	for i, item := range in {
		out[i] = deepCopy(item)
	}
	return out
}

// cloneInt32Ptr mirrors cloneIntPtr for the *int32 fields on Hook.
func cloneInt32Ptr(value *int32) *int32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
