package resourcemetrics

import (
	"fmt"
	"sort"
	"time"
)

type ResourceScope string

const (
	ResourceScopeGuestWorkload ResourceScope = "guest_workload"
	ResourceScopeHostSandbox   ResourceScope = "host_sandbox"
	ResourceScopeAll           ResourceScope = "all"
)

type SandboxResourceSnapshot struct {
	SandboxID     string
	QueriedAt     time.Time
	GuestWorkload *GuestWorkloadLatest
	HostSandbox   *HostSandboxLatest
}

type SandboxResourceCache struct {
	guest        *GuestWorkloadSampler
	host         *HostSandboxSampler
	exportScopes map[ResourceScope]struct{}
}

func NewSandboxResourceCache(guest *GuestWorkloadSampler, host *HostSandboxSampler, scopes ...ResourceScope) (*SandboxResourceCache, error) {
	exportScopes, err := newResourceScopeSet(scopes)
	if err != nil {
		return nil, err
	}
	return &SandboxResourceCache{guest: guest, host: host, exportScopes: exportScopes}, nil
}

func (c *SandboxResourceCache) Latest(sandboxID string, scope ResourceScope, now time.Time) (SandboxResourceSnapshot, bool, error) {
	scope, err := normalizeResourceScope(scope)
	if err != nil {
		return SandboxResourceSnapshot{}, false, err
	}
	if sandboxID == "" {
		return SandboxResourceSnapshot{}, false, fmt.Errorf("sandbox ID is required")
	}
	snapshot := SandboxResourceSnapshot{SandboxID: sandboxID, QueriedAt: now}
	found := false
	if (scope == ResourceScopeGuestWorkload || scope == ResourceScopeAll) && c.scopeEnabled(ResourceScopeGuestWorkload) {
		if guest, ok := c.latestGuest(sandboxID, now); ok {
			snapshot.GuestWorkload = &guest
			found = true
		}
	}
	if (scope == ResourceScopeHostSandbox || scope == ResourceScopeAll) && c.scopeEnabled(ResourceScopeHostSandbox) {
		if c.host != nil {
			if host, ok := c.host.Latest(sandboxID, now); ok {
				snapshot.HostSandbox = &host
				found = true
			}
		}
	}
	if scope != ResourceScopeAll && !c.scopeEnabled(scope) {
		return SandboxResourceSnapshot{}, false, fmt.Errorf("resource metrics scope %q is disabled by export_scopes", scope)
	}
	return snapshot, found, nil
}

func (c *SandboxResourceCache) ListLatest(scope ResourceScope, now time.Time) ([]SandboxResourceSnapshot, error) {
	scope, err := normalizeResourceScope(scope)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{})
	if c.guest != nil && c.scopeEnabled(ResourceScopeGuestWorkload) && (scope == ResourceScopeGuestWorkload || scope == ResourceScopeAll) {
		for _, latest := range c.guest.ListLatest(now) {
			ids[latest.Workload.SandboxID] = struct{}{}
		}
	}
	if c.host != nil && c.scopeEnabled(ResourceScopeHostSandbox) && (scope == ResourceScopeHostSandbox || scope == ResourceScopeAll) {
		for _, latest := range c.host.ListLatest(now) {
			ids[latest.SandboxID] = struct{}{}
		}
	}
	sandboxIDs := make([]string, 0, len(ids))
	for sandboxID := range ids {
		sandboxIDs = append(sandboxIDs, sandboxID)
	}
	sort.Strings(sandboxIDs)
	result := make([]SandboxResourceSnapshot, 0, len(sandboxIDs))
	for _, sandboxID := range sandboxIDs {
		snapshot, ok, err := c.Latest(sandboxID, scope, now)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, snapshot)
		}
	}
	return result, nil
}

func (c *SandboxResourceCache) latestGuest(sandboxID string, now time.Time) (GuestWorkloadLatest, bool) {
	if c.guest == nil {
		return GuestWorkloadLatest{}, false
	}
	return c.guest.Latest(WorkloadRef{SandboxID: sandboxID, ContainerID: sandboxID}, now)
}

func normalizeResourceScope(scope ResourceScope) (ResourceScope, error) {
	if scope == "" {
		return ResourceScopeAll, nil
	}
	switch scope {
	case ResourceScopeGuestWorkload, ResourceScopeHostSandbox, ResourceScopeAll:
		return scope, nil
	default:
		return "", fmt.Errorf("unsupported resource metrics scope %q", scope)
	}
}

func newResourceScopeSet(scopes []ResourceScope) (map[ResourceScope]struct{}, error) {
	if len(scopes) == 0 {
		scopes = []ResourceScope{ResourceScopeAll}
	}
	set := make(map[ResourceScope]struct{}, 2)
	for _, scope := range scopes {
		if scope == "" {
			return nil, fmt.Errorf("resource metrics export scope cannot be empty")
		}
		normalized, err := normalizeResourceScope(scope)
		if err != nil {
			return nil, err
		}
		if normalized == ResourceScopeAll {
			if len(scopes) != 1 {
				return nil, fmt.Errorf("resource metrics scope %q cannot be combined with other export scopes", ResourceScopeAll)
			}
			set[ResourceScopeGuestWorkload] = struct{}{}
			set[ResourceScopeHostSandbox] = struct{}{}
			return set, nil
		}
		set[normalized] = struct{}{}
	}
	return set, nil
}

func (c *SandboxResourceCache) scopeEnabled(scope ResourceScope) bool {
	_, ok := c.exportScopes[scope]
	return ok
}
