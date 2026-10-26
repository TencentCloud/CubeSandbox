package resourcemetrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const nanosecondsPerSecond = 1_000_000_000
const maxConcurrentPrometheusScrapes = 2

var (
	guestLabels = []string{"sandbox_id", "container_id"}
	hostLabels  = []string{"sandbox_id"}

	guestCPUUsage            = prometheus.NewDesc("cubesandbox_guest_workload_cpu_usage_seconds_total", "Total CPU time consumed by the guest workload cgroup.", guestLabels, nil)
	guestCPUUser             = prometheus.NewDesc("cubesandbox_guest_workload_cpu_user_seconds_total", "CPU time consumed in user space by the guest workload cgroup.", guestLabels, nil)
	guestCPUSystem           = prometheus.NewDesc("cubesandbox_guest_workload_cpu_system_seconds_total", "CPU time consumed in system space by the guest workload cgroup.", guestLabels, nil)
	guestCPUThrottled        = prometheus.NewDesc("cubesandbox_guest_workload_cpu_throttled_seconds_total", "Total time the guest workload cgroup was throttled.", guestLabels, nil)
	guestCPUPeriods          = prometheus.NewDesc("cubesandbox_guest_workload_cpu_periods_total", "Total CPU scheduling periods for the guest workload cgroup.", guestLabels, nil)
	guestCPUThrottledPeriods = prometheus.NewDesc("cubesandbox_guest_workload_cpu_throttled_periods_total", "Total throttled CPU scheduling periods for the guest workload cgroup.", guestLabels, nil)
	guestCPULimit            = prometheus.NewDesc("cubesandbox_guest_workload_cpu_limit_cores", "Configured CPU limit for the guest workload cgroup in cores.", guestLabels, nil)
	guestMemoryCurrent       = prometheus.NewDesc("cubesandbox_guest_workload_memory_current_bytes", "Current memory charged to the guest workload cgroup.", guestLabels, nil)
	guestMemoryLimit         = prometheus.NewDesc("cubesandbox_guest_workload_memory_limit_bytes", "Configured memory limit for the guest workload cgroup.", guestLabels, nil)
	guestMemoryFailures      = prometheus.NewDesc("cubesandbox_guest_workload_memory_failures_total", "Total guest workload memory limit failures since the current metrics epoch baseline.", guestLabels, nil)
	guestEpoch               = prometheus.NewDesc("cubesandbox_guest_workload_metrics_epoch", "Current guest workload metrics epoch generation.", guestLabels, nil)
	guestEpochStarted        = prometheus.NewDesc("cubesandbox_guest_workload_metrics_epoch_start_time_seconds", "Start time of the current guest workload metrics epoch as Unix seconds.", guestLabels, nil)

	hostCPUUsage            = prometheus.NewDesc("cubesandbox_host_sandbox_cpu_usage_seconds_total", "CPU time consumed by the current sandbox since its host cgroup assignment baseline.", hostLabels, nil)
	hostCPUUser             = prometheus.NewDesc("cubesandbox_host_sandbox_cpu_user_seconds_total", "User-space CPU time consumed by the current sandbox since its host cgroup assignment baseline.", hostLabels, nil)
	hostCPUSystem           = prometheus.NewDesc("cubesandbox_host_sandbox_cpu_system_seconds_total", "System-space CPU time consumed by the current sandbox since its host cgroup assignment baseline.", hostLabels, nil)
	hostCPUThrottled        = prometheus.NewDesc("cubesandbox_host_sandbox_cpu_throttled_seconds_total", "Time the current sandbox was throttled since its host cgroup assignment baseline.", hostLabels, nil)
	hostCPUPeriods          = prometheus.NewDesc("cubesandbox_host_sandbox_cpu_periods_total", "CPU scheduling periods for the current sandbox since its host cgroup assignment baseline.", hostLabels, nil)
	hostCPUThrottledPeriods = prometheus.NewDesc("cubesandbox_host_sandbox_cpu_throttled_periods_total", "Throttled CPU periods for the current sandbox since its host cgroup assignment baseline.", hostLabels, nil)
	hostCPULimit            = prometheus.NewDesc("cubesandbox_host_sandbox_cpu_limit_cores", "Configured CPU limit for the host sandbox cgroup in cores.", hostLabels, nil)
	hostMemoryCurrent       = prometheus.NewDesc("cubesandbox_host_sandbox_memory_current_bytes", "Current memory charged to the host sandbox cgroup.", hostLabels, nil)
	hostMemoryLimit         = prometheus.NewDesc("cubesandbox_host_sandbox_memory_limit_bytes", "Configured memory limit for the host sandbox cgroup.", hostLabels, nil)
	hostMemoryFailures      = prometheus.NewDesc("cubesandbox_host_sandbox_memory_failures_total", "Host cgroup memory limit failures for the current sandbox since its assignment baseline.", hostLabels, nil)
)

type prometheusCollector struct {
	cache *SandboxResourceCache
	now   func() time.Time
}

func NewPrometheusHandler(cache *SandboxResourceCache) http.Handler {
	return newPrometheusHandler(cache, time.Now)
}

func newPrometheusHandler(cache *SandboxResourceCache, now func() time.Time) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(&prometheusCollector{cache: cache, now: now})
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{MaxRequestsInFlight: maxConcurrentPrometheusScrapes})
}

func (c *prometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		guestCPUUsage, guestCPUUser, guestCPUSystem, guestCPUThrottled,
		guestCPUPeriods, guestCPUThrottledPeriods, guestCPULimit,
		guestMemoryCurrent, guestMemoryLimit, guestMemoryFailures,
		guestEpoch, guestEpochStarted,
		hostCPUUsage, hostCPUUser, hostCPUSystem, hostCPUThrottled,
		hostCPUPeriods, hostCPUThrottledPeriods, hostCPULimit,
		hostMemoryCurrent, hostMemoryLimit, hostMemoryFailures,
	} {
		ch <- desc
	}
}

func (c *prometheusCollector) Collect(ch chan<- prometheus.Metric) {
	if c.cache == nil {
		return
	}
	latest, err := c.cache.ListLatest(ResourceScopeAll, c.now())
	if err != nil {
		return
	}
	for _, sandbox := range latest {
		if sandbox.GuestWorkload != nil {
			collectGuestPrometheus(ch, *sandbox.GuestWorkload)
		}
		if sandbox.HostSandbox != nil {
			collectHostPrometheus(ch, *sandbox.HostSandbox)
		}
	}
}

func collectGuestPrometheus(ch chan<- prometheus.Metric, latest GuestWorkloadLatest) {
	if latest.Availability != GuestWorkloadAvailable {
		return
	}
	labels := []string{latest.Workload.SandboxID, latest.Workload.ContainerID}
	if latest.EpochGeneration > 0 {
		ch <- prometheus.MustNewConstMetric(guestEpoch, prometheus.GaugeValue, float64(latest.EpochGeneration), labels...)
	}
	if !latest.EpochStartedAt.IsZero() {
		ch <- prometheus.MustNewConstMetric(guestEpochStarted, prometheus.GaugeValue, float64(latest.EpochStartedAt.UnixNano())/nanosecondsPerSecond, labels...)
	}
	if latest.CPULimitMillicores != nil {
		ch <- prometheus.MustNewConstMetric(guestCPULimit, prometheus.GaugeValue, float64(*latest.CPULimitMillicores)/1000, labels...)
	}
	ch <- prometheus.MustNewConstMetric(guestMemoryCurrent, prometheus.GaugeValue, float64(latest.MemoryCurrentBytes), labels...)
	if !latest.MemoryLimitUnlimited {
		ch <- prometheus.MustNewConstMetric(guestMemoryLimit, prometheus.GaugeValue, float64(latest.MemoryLimitBytes), labels...)
	}
	if !latest.CumulativeAvailable || latest.Snapshot == nil {
		return
	}
	counters := latest.Snapshot.Counters
	ch <- prometheus.MustNewConstMetric(guestCPUUsage, prometheus.CounterValue, seconds(counters.CPUUsageTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(guestCPUUser, prometheus.CounterValue, seconds(counters.CPUUserTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(guestCPUSystem, prometheus.CounterValue, seconds(counters.CPUSystemTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(guestCPUThrottled, prometheus.CounterValue, seconds(counters.CPUThrottledTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(guestCPUPeriods, prometheus.CounterValue, float64(counters.CPUPeriodsTotal), labels...)
	ch <- prometheus.MustNewConstMetric(guestCPUThrottledPeriods, prometheus.CounterValue, float64(counters.CPUThrottledPeriodsTotal), labels...)
	ch <- prometheus.MustNewConstMetric(guestMemoryFailures, prometheus.CounterValue, float64(counters.MemoryFailuresTotal), labels...)
}

func collectHostPrometheus(ch chan<- prometheus.Metric, latest HostSandboxLatest) {
	if latest.Availability != HostSandboxAvailable || latest.Snapshot == nil {
		return
	}
	labels := []string{latest.SandboxID}
	snapshot := latest.Snapshot
	ch <- prometheus.MustNewConstMetric(hostCPUUsage, prometheus.CounterValue, seconds(snapshot.CPUUsageTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(hostCPUUser, prometheus.CounterValue, seconds(snapshot.CPUUserTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(hostCPUSystem, prometheus.CounterValue, seconds(snapshot.CPUSystemTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(hostCPUThrottled, prometheus.CounterValue, seconds(snapshot.CPUThrottledTotalNS), labels...)
	ch <- prometheus.MustNewConstMetric(hostCPUPeriods, prometheus.CounterValue, float64(snapshot.CPUPeriodsTotal), labels...)
	ch <- prometheus.MustNewConstMetric(hostCPUThrottledPeriods, prometheus.CounterValue, float64(snapshot.CPUThrottledPeriodsTotal), labels...)
	if !snapshot.CPULimitUnlimited && snapshot.CPULimitPeriodUS > 0 {
		ch <- prometheus.MustNewConstMetric(hostCPULimit, prometheus.GaugeValue, float64(snapshot.CPULimitQuotaUS)/float64(snapshot.CPULimitPeriodUS), labels...)
	}
	ch <- prometheus.MustNewConstMetric(hostMemoryCurrent, prometheus.GaugeValue, float64(snapshot.MemoryCurrentBytes), labels...)
	if !snapshot.MemoryLimitUnlimited {
		ch <- prometheus.MustNewConstMetric(hostMemoryLimit, prometheus.GaugeValue, float64(snapshot.MemoryLimitBytes), labels...)
	}
	ch <- prometheus.MustNewConstMetric(hostMemoryFailures, prometheus.CounterValue, float64(snapshot.MemoryFailuresTotal), labels...)
}

func seconds(nanoseconds uint64) float64 {
	return float64(nanoseconds) / nanosecondsPerSecond
}
