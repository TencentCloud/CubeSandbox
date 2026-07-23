package resourcemetrics

import (
	"context"
	"fmt"
	"net/http"
	"time"

	containerdsandbox "github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

const PluginID = "resource-metrics"

type Service struct {
	*SandboxResourceCache
	handler http.Handler
}

func NewService(cache *SandboxResourceCache) *Service {
	return &Service{SandboxResourceCache: cache, handler: NewPrometheusHandler(cache)}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

type Config struct {
	Enabled               bool             `toml:"enabled"`
	CollectionInterval    tomlext.Duration `toml:"collection_interval"`
	RequestTimeout        tomlext.Duration `toml:"request_timeout"`
	MaxConcurrentRequests int              `toml:"max_concurrent_requests"`
	StaleAfter            tomlext.Duration `toml:"stale_after"`
	ExportScopes          []string         `toml:"export_scopes"`
}

type samplerSelection struct {
	GuestWorkload bool
	HostSandbox   bool
}

func samplerSelectionForExportScopes(scopes []string) (samplerSelection, error) {
	resourceScopes := make([]ResourceScope, len(scopes))
	for i, scope := range scopes {
		resourceScopes[i] = ResourceScope(scope)
	}
	enabled, err := newResourceScopeSet(resourceScopes)
	if err != nil {
		return samplerSelection{}, err
	}
	_, guestWorkload := enabled[ResourceScopeGuestWorkload]
	_, hostSandbox := enabled[ResourceScopeHostSandbox]
	return samplerSelection{GuestWorkload: guestWorkload, HostSandbox: hostSandbox}, nil
}

func defaultConfig() *Config {
	return &Config{
		Enabled:               true,
		CollectionInterval:    tomlext.FromStdTime(5 * time.Second),
		RequestTimeout:        tomlext.FromStdTime(2 * time.Second),
		MaxConcurrentRequests: 8,
		StaleAfter:            tomlext.FromStdTime(15 * time.Second),
		ExportScopes:          []string{"host_sandbox"},
	}
}

func init() {
	registry.Register(&plugin.Registration{
		Type: constants.InternalPlugin,
		ID:   PluginID,
		Requires: []plugin.Type{
			constants.CubeStorePlugin,
			constants.InternalPlugin,
			plugins.SandboxControllerPlugin,
		},
		Config: defaultConfig(),
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			config := ic.Config.(*Config)
			selection, err := samplerSelectionForExportScopes(config.ExportScopes)
			if err != nil {
				return nil, fmt.Errorf("configure resource metrics export scopes: %w", err)
			}
			storePlugin, err := ic.GetByID(constants.CubeStorePlugin, constants.CubeboxID.ID())
			if err != nil {
				return nil, fmt.Errorf("load cubebox store for resource metrics: %w", err)
			}
			store, ok := storePlugin.(cubes.CubeboxAPI)
			if !ok {
				return nil, fmt.Errorf("cubebox store does not expose resource metrics state")
			}
			storeAdapter := cubeboxStoreAdapter{store: store}
			var sampler *GuestWorkloadSampler
			if selection.GuestWorkload {
				controllerPlugin, err := ic.GetByID(plugins.SandboxControllerPlugin, "cube")
				if err != nil {
					return nil, fmt.Errorf("load cube sandbox controller for resource metrics: %w", err)
				}
				controller, ok := controllerPlugin.(containerdsandbox.Controller)
				if !ok {
					return nil, fmt.Errorf("cube sandbox controller does not expose task metrics")
				}
				sampler, err = NewGuestWorkloadSampler(GuestWorkloadSamplerConfig{
					CollectionInterval:    tomlext.ToStdTime(config.CollectionInterval),
					RequestTimeout:        tomlext.ToStdTime(config.RequestTimeout),
					MaxConcurrentRequests: config.MaxConcurrentRequests,
					StaleAfter:            tomlext.ToStdTime(config.StaleAfter),
				}, storeAdapter, controller)
				if err != nil {
					return nil, err
				}
			}
			var hostSampler *HostSandboxSampler
			if selection.HostSandbox {
				cgroupPlugin, err := ic.GetByID(constants.InternalPlugin, constants.CgroupID.ID())
				if err != nil {
					return nil, fmt.Errorf("load cube cgroup reader for resource metrics: %w", err)
				}
				hostReader, ok := cgroupPlugin.(hostSandboxUsageReader)
				if !ok {
					return nil, fmt.Errorf("cube cgroup plugin does not expose host usage snapshots")
				}
				hostSampler, err = NewHostSandboxSampler(HostSandboxSamplerConfig{
					CollectionInterval:    tomlext.ToStdTime(config.CollectionInterval),
					RequestTimeout:        tomlext.ToStdTime(config.RequestTimeout),
					MaxConcurrentRequests: config.MaxConcurrentRequests,
					StaleAfter:            tomlext.ToStdTime(config.StaleAfter),
				}, storeAdapter, hostReader)
				if err != nil {
					return nil, err
				}
			}
			exportScopes := make([]ResourceScope, len(config.ExportScopes))
			for i, scope := range config.ExportScopes {
				exportScopes[i] = ResourceScope(scope)
			}
			cache, err := NewSandboxResourceCache(sampler, hostSampler, exportScopes...)
			if err != nil {
				return nil, fmt.Errorf("configure resource metrics export scopes: %w", err)
			}
			if eventRegistry, ok := storePlugin.(cubes.CubeboxEventListenerRegistry); ok {
				eventRegistry.Register(cubeboxMetricsEpochListener{guest: sampler, host: hostSampler})
			}
			if config.Enabled {
				if sampler != nil {
					go sampler.Run(ic.Context)
				}
				if hostSampler != nil {
					go hostSampler.Run(ic.Context)
				}
			}
			return NewService(cache), nil
		},
	})
}

type cubeboxStoreAdapter struct {
	store cubes.CubeboxAPI
}

type cubeboxMetricsEpochListener struct {
	guest *GuestWorkloadSampler
	host  *HostSandboxSampler
}

func (l cubeboxMetricsEpochListener) OnCubeboxEvent(_ context.Context, event *cubes.CubeboxEvent) error {
	if event == nil || event.Cubebox == nil {
		return nil
	}
	workload := WorkloadRef{SandboxID: event.Cubebox.ID, ContainerID: event.Cubebox.ID}
	status := event.Cubebox.MainStatus()
	if l.guest != nil {
		l.guest.SetLifecycleUnavailable(workload, guestWorkloadSamplingUnavailable(status))
		l.guest.InvalidateEpoch(workload, event.Cubebox.GuestMetricsEpochCopy())
	}
	if l.host != nil {
		l.host.SetLifecycleUnavailable(event.Cubebox.ID, hostSandboxSamplingUnavailable(status))
	}
	return nil
}

func (a cubeboxStoreAdapter) List() []*cubeboxstore.CubeBox {
	return a.store.List()
}

func (a cubeboxStoreAdapter) SyncByID(ctx context.Context, id string) error {
	return a.store.SyncByID(ctx, id)
}
