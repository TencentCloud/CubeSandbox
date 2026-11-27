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
// templateDefinitionCache and is handed to concurrent creates. Every
// reference-typed field is therefore rebuilt explicitly instead of copied by
// `*src`, which also keeps new fields visible at review time.
//
// deepcopy_test.go pins the output against the JSON round trip this replaced:
// add a field to any type below without copying it here and that test fails
// instead of leaving two requests aliasing one slice.

// DeepCopy returns a request that shares no mutable state with r. The result
// marshals to exactly the same JSON as r, which is what the previous
// json.Marshal/json.Unmarshal clone guaranteed for code that persists or
// fingerprints a request.
func (r *CreateCubeSandboxReq) DeepCopy() *CreateCubeSandboxReq {
	if r == nil {
		return nil
	}
	// Value copy covers every scalar field; each reference field below
	// overwrites the pointer/slice/map it inherited so no alias survives.
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
	return &Volume{Name: v.Name, VolumeSource: v.VolumeSource.DeepCopy()}
}

// DeepCopy returns a volume source that shares no mutable state with s.
func (s *VolumeSource) DeepCopy() *VolumeSource {
	if s == nil {
		return nil
	}
	out := &VolumeSource{
		EmptyDir:             s.EmptyDir.DeepCopy(),
		SandboxPath:          s.SandboxPath.DeepCopy(),
		HostDirVolumeSources: s.HostDirVolumeSources.DeepCopy(),
		PluginVolume:         s.PluginVolume.DeepCopy(),
	}
	if s.Image != nil {
		out.Image = proto.Clone(s.Image).(*imagev1.ImageVolumeSource)
	}
	return out
}

// DeepCopy returns a host directory source list that shares no mutable state
// with s.
func (s *HostDirVolumeSources) DeepCopy() *HostDirVolumeSources {
	if s == nil {
		return nil
	}
	return &HostDirVolumeSources{
		VolumeSources: copyPtrSlice(s.VolumeSources, (*HostDirSource).DeepCopy),
	}
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
	return &PluginVolumeSource{Driver: s.Driver, Options: maps.Clone(s.Options)}
}

// DeepCopy returns a container that shares no mutable state with c.
func (c *Container) DeepCopy() *Container {
	if c == nil {
		return nil
	}
	out := &Container{
		Name:            c.Name,
		Image:           c.Image.DeepCopy(),
		Command:         slices.Clone(c.Command),
		Args:            slices.Clone(c.Args),
		WorkingDir:      c.WorkingDir,
		Envs:            copyPtrSlice(c.Envs, (*KeyValue).DeepCopy),
		RLimit:          c.RLimit.DeepCopy(),
		Resources:       c.Resources.DeepCopy(),
		SecurityContext: c.SecurityContext.DeepCopy(),
		Probe:           c.Probe.DeepCopy(),
		Sysctls:         maps.Clone(c.Sysctls),
		Syscalls:        copyPtrSlice(c.Syscalls, (*SysCall).DeepCopy),
		DnsConfig:       c.DnsConfig.DeepCopy(),
		Annotations:     maps.Clone(c.Annotations),
		HostAliases:     copyPtrSlice(c.HostAliases, (*HostAlias).DeepCopy),
		Prestop:         c.Prestop.DeepCopy(),
		Poststop:        c.Poststop.DeepCopy(),
		Id:              c.Id,
		Hooks:           c.Hooks.DeepCopy(),
	}
	if c.VolumeMounts != nil {
		out.VolumeMounts = make([]*cubeboxv1.VolumeMounts, len(c.VolumeMounts))
		for i, mount := range c.VolumeMounts {
			if mount != nil {
				out.VolumeMounts[i] = proto.Clone(mount).(*cubeboxv1.VolumeMounts)
			}
		}
	}
	if c.LivenessProbe != nil {
		out.LivenessProbe = proto.Clone(c.LivenessProbe).(*cubeboxv1.LivenessProbe)
	}
	return out
}

// DeepCopy returns an image spec that shares no mutable state with s.
func (s *ImageSpec) DeepCopy() *ImageSpec {
	if s == nil {
		return nil
	}
	return &ImageSpec{
		Image:             s.Image,
		Name:              s.Name,
		Token:             s.Token,
		Annotations:       maps.Clone(s.Annotations),
		StorageMedia:      s.StorageMedia,
		WritableLayerSize: s.WritableLayerSize,
	}
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
	return &Resource{Cpu: r.Cpu, Mem: r.Mem, Limit: r.Limit.DeepCopy()}
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
	return &ContainerSecurityContext{
		Capabilities:   s.Capabilities.DeepCopy(),
		Privileged:     s.Privileged,
		RunAsUser:      s.RunAsUser.DeepCopy(),
		NoNewPrivs:     s.NoNewPrivs,
		RunAsGroup:     s.RunAsGroup.DeepCopy(),
		RunAsUsername:  s.RunAsUsername,
		ReadonlyRootfs: s.ReadonlyRootfs,
	}
}

// DeepCopy returns a capability set that shares no mutable state with c.
func (c *Capability) DeepCopy() *Capability {
	if c == nil {
		return nil
	}
	return &Capability{
		AddCapabilities:        slices.Clone(c.AddCapabilities),
		DropCapabilities:       slices.Clone(c.DropCapabilities),
		AddAmbientCapabilities: slices.Clone(c.AddAmbientCapabilities),
	}
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
	return &ProbeHandler{
		TCPSocket: h.TCPSocket.DeepCopy(),
		Ping:      h.Ping.DeepCopy(),
		HttpGet:   h.HttpGet.DeepCopy(),
	}
}

// DeepCopy returns an HTTP probe action that shares no mutable state with a.
func (a *HTTPGetAction) DeepCopy() *HTTPGetAction {
	if a == nil {
		return nil
	}
	return &HTTPGetAction{
		Path:        cloneStringPtr(a.Path),
		Port:        a.Port,
		Host:        cloneStringPtr(a.Host),
		HttpHeaders: copyPtrSlice(a.HttpHeaders, (*HTTPHeader).DeepCopy),
	}
}

// DeepCopy returns an HTTP header that shares no mutable state with h.
func (h *HTTPHeader) DeepCopy() *HTTPHeader {
	if h == nil {
		return nil
	}
	return &HTTPHeader{Name: cloneStringPtr(h.Name), Value: cloneStringPtr(h.Value)}
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
	return &SysCall{
		Names:  slices.Clone(s.Names),
		Action: s.Action,
		Errno:  s.Errno,
		Args:   copyPtrSlice(s.Args, (*LinuxSeccompArg).DeepCopy),
	}
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
	return &DNSConfig{
		Servers:  slices.Clone(c.Servers),
		Searches: slices.Clone(c.Searches),
		Options:  slices.Clone(c.Options),
	}
}

// DeepCopy returns an /etc/hosts alias that shares no mutable state with a.
func (a *HostAlias) DeepCopy() *HostAlias {
	if a == nil {
		return nil
	}
	return &HostAlias{Hostnames: slices.Clone(a.Hostnames), Ip: a.Ip}
}

// DeepCopy returns a pre-stop hook that shares no mutable state with p.
func (p *PreStop) DeepCopy() *PreStop {
	if p == nil {
		return nil
	}
	return &PreStop{
		TerminationGracePeriodMs: p.TerminationGracePeriodMs,
		LifecyleHandler:          p.LifecyleHandler.DeepCopy(),
	}
}

// DeepCopy returns a post-stop hook that shares no mutable state with p.
func (p *PostStop) DeepCopy() *PostStop {
	if p == nil {
		return nil
	}
	return &PostStop{
		TimeoutMs:       p.TimeoutMs,
		LifecyleHandler: p.LifecyleHandler.DeepCopy(),
	}
}

// DeepCopy returns a lifecycle handler that shares no mutable state with h.
func (h *LifecycleHandler) DeepCopy() *LifecycleHandler {
	if h == nil {
		return nil
	}
	return &LifecycleHandler{HttpGet: h.HttpGet.DeepCopy()}
}

// DeepCopy returns a hook set that shares no mutable state with h.
func (h *Hooks) DeepCopy() *Hooks {
	if h == nil {
		return nil
	}
	return &Hooks{Prestart: copyPtrSlice(h.Prestart, (*Hook).DeepCopy)}
}

// DeepCopy returns a prestart hook that shares no mutable state with h.
func (h *Hook) DeepCopy() *Hook {
	if h == nil {
		return nil
	}
	return &Hook{
		Path:    h.Path,
		Args:    slices.Clone(h.Args),
		Env:     slices.Clone(h.Env),
		Timeout: cloneInt32Ptr(h.Timeout),
	}
}

// copyPtrSlice clones a slice of pointers with the element's own DeepCopy. A
// nil slice stays nil so the copy marshals exactly like the original (an
// empty non-nil slice would still be dropped by omitempty, but callers also
// compare against nil).
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
