package resourcemetrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSandboxResourceCacheProjectsIndependentScopes(t *testing.T) {
	guest := &GuestWorkloadSampler{
		config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]GuestWorkloadLatest{
			"sandbox/sandbox": {
				Workload:            WorkloadRef{SandboxID: "sandbox", ContainerID: "sandbox"},
				CollectedAt:         time.Unix(10, 0),
				Availability:        GuestWorkloadAvailable,
				CumulativeAvailable: true,
				MemoryCurrentBytes:  11,
			},
		},
	}
	host := &HostSandboxSampler{
		config: HostSandboxSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]HostSandboxLatest{
			"sandbox": {
				SandboxID:    "sandbox",
				CollectedAt:  time.Unix(20, 0),
				Availability: HostSandboxAvailable,
				Snapshot:     &HostSandboxSnapshot{SandboxID: "sandbox", CPUUsageTotalNS: 22},
			},
		},
	}
	cache, err := NewSandboxResourceCache(guest, host)
	require.NoError(t, err)

	guestView, ok, err := cache.Latest("sandbox", ResourceScopeGuestWorkload, time.Unix(21, 0))
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, guestView.GuestWorkload)
	require.Nil(t, guestView.HostSandbox)
	require.Equal(t, time.Unix(10, 0), guestView.GuestWorkload.CollectedAt)

	hostView, ok, err := cache.Latest("sandbox", ResourceScopeHostSandbox, time.Unix(21, 0))
	require.NoError(t, err)
	require.True(t, ok)
	require.Nil(t, hostView.GuestWorkload)
	require.NotNil(t, hostView.HostSandbox)
	require.Equal(t, time.Unix(20, 0), hostView.HostSandbox.CollectedAt)

	all, ok, err := cache.Latest("sandbox", ResourceScopeAll, time.Unix(21, 0))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, time.Unix(21, 0), all.QueriedAt)
	require.Equal(t, time.Unix(10, 0), all.GuestWorkload.CollectedAt)
	require.Equal(t, time.Unix(20, 0), all.HostSandbox.CollectedAt)
}

func TestSandboxResourceCacheDoesNotSuppressHealthyScope(t *testing.T) {
	guest := &GuestWorkloadSampler{
		config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]GuestWorkloadLatest{
			"sandbox/sandbox": {
				Workload:     WorkloadRef{SandboxID: "sandbox", ContainerID: "sandbox"},
				Availability: GuestWorkloadUnavailable,
				LastError:    "guest unavailable",
			},
		},
	}
	host := &HostSandboxSampler{
		config: HostSandboxSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]HostSandboxLatest{
			"sandbox": {
				SandboxID:    "sandbox",
				CollectedAt:  time.Unix(30, 0),
				Availability: HostSandboxAvailable,
				Snapshot:     &HostSandboxSnapshot{SandboxID: "sandbox", MemoryCurrentBytes: 44},
			},
		},
	}

	cache, err := NewSandboxResourceCache(guest, host)
	require.NoError(t, err)
	all, ok, err := cache.Latest("sandbox", "", time.Unix(31, 0))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, GuestWorkloadUnavailable, all.GuestWorkload.Availability)
	require.Equal(t, HostSandboxAvailable, all.HostSandbox.Availability)
	require.Equal(t, uint64(44), all.HostSandbox.Snapshot.MemoryCurrentBytes)
}

func TestSandboxResourceCacheRejectsUnknownScope(t *testing.T) {
	cache, err := NewSandboxResourceCache(&GuestWorkloadSampler{config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute}, latest: map[string]GuestWorkloadLatest{}}, &HostSandboxSampler{config: HostSandboxSamplerConfig{StaleAfter: time.Minute}, latest: map[string]HostSandboxLatest{}})
	require.NoError(t, err)
	_, _, err = cache.Latest("sandbox", ResourceScope("unexpected"), time.Now())
	require.Error(t, err)
}

func TestSandboxResourceCacheAppliesConfiguredExportScopes(t *testing.T) {
	guest := &GuestWorkloadSampler{
		config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]GuestWorkloadLatest{
			"sandbox/sandbox": {Workload: WorkloadRef{SandboxID: "sandbox", ContainerID: "sandbox"}, Availability: GuestWorkloadAvailable},
		},
	}
	host := &HostSandboxSampler{
		config: HostSandboxSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]HostSandboxLatest{
			"sandbox": {SandboxID: "sandbox", Availability: HostSandboxAvailable},
		},
	}
	cache, err := NewSandboxResourceCache(guest, host, ResourceScopeGuestWorkload)
	require.NoError(t, err)

	all, ok, err := cache.Latest("sandbox", ResourceScopeAll, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, all.GuestWorkload)
	require.Nil(t, all.HostSandbox)

	_, _, err = cache.Latest("sandbox", ResourceScopeHostSandbox, time.Now())
	require.Error(t, err)
}

func TestSandboxResourceCacheRejectsInvalidConfiguredExportScopes(t *testing.T) {
	_, err := NewSandboxResourceCache(nil, nil, ResourceScopeAll, ResourceScopeGuestWorkload)
	require.Error(t, err)
	_, err = NewSandboxResourceCache(nil, nil, ResourceScope("unexpected"))
	require.Error(t, err)
	_, err = NewSandboxResourceCache(nil, nil, ResourceScope(""))
	require.Error(t, err)
}
