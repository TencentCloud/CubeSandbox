package resourcemetrics

import (
	"context"
	"testing"
	"time"

	srvconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

func TestPluginConfigDecodesDurationStrings(t *testing.T) {
	pluginID := constants.InternalPlugin.String() + "." + PluginID
	serverConfig := &srvconfig.Config{Plugins: map[string]interface{}{
		pluginID: map[string]interface{}{
			"enabled":                 true,
			"collection_interval":     "5s",
			"request_timeout":         "2s",
			"max_concurrent_requests": int64(8),
			"stale_after":             "15s",
			"export_scopes":           []string{"host_sandbox"},
		},
	}}

	decoded, err := serverConfig.Decode(context.Background(), pluginID, defaultConfig())
	require.NoError(t, err)
	config := decoded.(*Config)
	require.Equal(t, 5*time.Second, time.Duration(config.CollectionInterval))
	require.Equal(t, 2*time.Second, time.Duration(config.RequestTimeout))
	require.Equal(t, 15*time.Second, time.Duration(config.StaleAfter))
	require.Equal(t, []string{"host_sandbox"}, config.ExportScopes)
}

func TestDefaultConfigSelectsHostSandbox(t *testing.T) {
	require.Equal(t, []string{"host_sandbox"}, defaultConfig().ExportScopes)
}

func TestExportScopesSelectBackgroundSamplers(t *testing.T) {
	for _, test := range []struct {
		name      string
		scopes    []string
		wantGuest bool
		wantHost  bool
	}{
		{name: "host only", scopes: []string{"host_sandbox"}, wantHost: true},
		{name: "guest only", scopes: []string{"guest_workload"}, wantGuest: true},
		{name: "all", scopes: []string{"all"}, wantGuest: true, wantHost: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection, err := samplerSelectionForExportScopes(test.scopes)
			require.NoError(t, err)
			require.Equal(t, test.wantGuest, selection.GuestWorkload)
			require.Equal(t, test.wantHost, selection.HostSandbox)
		})
	}
}

func TestMetricsEventListenerSupportsOneSelectedSampler(t *testing.T) {
	for _, test := range []struct {
		name  string
		guest bool
	}{
		{name: "guest only", guest: true},
		{name: "host only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
			cb.CGroupPath = "/cube_sandbox/sandbox/1"
			store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
			listener := cubeboxMetricsEpochListener{}
			if test.guest {
				listener.guest = testGuestWorkloadSampler(t, store, &fakeGuestWorkloadReader{})
			} else {
				listener.host = testHostSandboxSampler(t, store, &fakeHostSandboxReader{})
			}

			require.NoError(t, listener.OnCubeboxEvent(context.Background(), &cubes.CubeboxEvent{Cubebox: cb}))
		})
	}
}
