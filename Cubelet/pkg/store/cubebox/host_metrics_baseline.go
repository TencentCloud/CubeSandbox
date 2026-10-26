package cubebox

type HostMetricsBaseline struct {
	CGroupPath               string `json:"cgroup_path"`
	CPUUsageTotalNS          uint64 `json:"cpu_usage_total_ns"`
	CPUUserTotalNS           uint64 `json:"cpu_user_total_ns"`
	CPUSystemTotalNS         uint64 `json:"cpu_system_total_ns"`
	CPUThrottledTotalNS      uint64 `json:"cpu_throttled_total_ns"`
	CPUPeriodsTotal          uint64 `json:"cpu_periods_total"`
	CPUThrottledPeriodsTotal uint64 `json:"cpu_throttled_periods_total"`
	MemoryFailuresTotal      uint64 `json:"memory_failures_total"`
}

func (b *HostMetricsBaseline) DeepCopy() *HostMetricsBaseline {
	if b == nil {
		return nil
	}
	copied := *b
	return &copied
}

func (cb *CubeBox) HostMetricsBaselineCopy() *HostMetricsBaseline {
	if cb == nil {
		return nil
	}
	cb.hostMetricsBaselineLock.RLock()
	defer cb.hostMetricsBaselineLock.RUnlock()
	return cb.HostMetricsBaseline.DeepCopy()
}

func (cb *CubeBox) RestoreHostMetricsBaseline(baseline *HostMetricsBaseline) {
	if cb == nil {
		return
	}
	cb.hostMetricsBaselineLock.Lock()
	defer cb.hostMetricsBaselineLock.Unlock()
	cb.HostMetricsBaseline = baseline.DeepCopy()
}

func (cb *CubeBox) RestoreHostMetricsBaselineIfCurrent(expected HostMetricsBaseline, baseline *HostMetricsBaseline) bool {
	if cb == nil {
		return false
	}
	cb.hostMetricsBaselineLock.Lock()
	defer cb.hostMetricsBaselineLock.Unlock()
	if cb.HostMetricsBaseline == nil || *cb.HostMetricsBaseline != expected {
		return false
	}
	cb.HostMetricsBaseline = baseline.DeepCopy()
	return true
}
