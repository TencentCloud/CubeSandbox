package resourcemetrics

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type HostSandboxSamplerConfig struct {
	CollectionInterval    time.Duration
	RequestTimeout        time.Duration
	MaxConcurrentRequests int
	StaleAfter            time.Duration
}

func (c HostSandboxSamplerConfig) validate() error {
	if c.CollectionInterval <= 0 {
		return fmt.Errorf("host sandbox collection interval must be positive")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("host sandbox request timeout must be positive")
	}
	if c.MaxConcurrentRequests <= 0 {
		return fmt.Errorf("host sandbox max concurrent requests must be positive")
	}
	if c.StaleAfter < c.CollectionInterval {
		return fmt.Errorf("host sandbox stale after must not be less than collection interval")
	}
	return nil
}

type hostSandboxUsageReader interface {
	UsageSnapshot(context.Context, string) (handle.UsageSnapshot, error)
}

type HostSandboxAvailability string

const (
	HostSandboxAvailable   HostSandboxAvailability = "available"
	HostSandboxStale       HostSandboxAvailability = "stale"
	HostSandboxUnavailable HostSandboxAvailability = "unavailable"
)

type HostSandboxSnapshot struct {
	Timestamp                time.Time
	SandboxID                string
	CGroupPath               string
	CPUUsageTotalNS          uint64
	CPUUserTotalNS           uint64
	CPUSystemTotalNS         uint64
	CPUThrottledTotalNS      uint64
	CPUPeriodsTotal          uint64
	CPUThrottledPeriodsTotal uint64
	CPULimitQuotaUS          uint64
	CPULimitPeriodUS         uint64
	CPULimitUnlimited        bool
	MemoryCurrentBytes       uint64
	MemoryLimitBytes         uint64
	MemoryLimitUnlimited     bool
	MemoryFailuresTotal      uint64
}

type HostSandboxLatest struct {
	SandboxID    string
	CGroupPath   string
	CollectedAt  time.Time
	Availability HostSandboxAvailability
	Snapshot     *HostSandboxSnapshot
	LastError    string
}

type HostSandboxSampler struct {
	config HostSandboxSamplerConfig
	store  guestWorkloadCubeboxStore
	reader hostSandboxUsageReader
	now    func() time.Time
	jitter func(time.Duration) time.Duration

	mu        sync.RWMutex
	latest    map[string]HostSandboxLatest
	inFlight  map[string]uint64
	blocked   map[string]bool
	tokens    map[string]uint64
	nextToken uint64
	requests  chan struct{}
	completed chan struct{}

	scheduleMu          sync.Mutex
	cycle               []*cubeboxstore.CubeBox
	cycleNeedsCleanup   bool
	dispatchInterval    time.Duration
	hasDispatchInterval bool
	nextIndex           int
}

func NewHostSandboxSampler(config HostSandboxSamplerConfig, store guestWorkloadCubeboxStore, reader hostSandboxUsageReader) (*HostSandboxSampler, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("host sandbox cubebox store is required")
	}
	if reader == nil {
		return nil, fmt.Errorf("host sandbox usage reader is required")
	}
	return &HostSandboxSampler{
		config:    config,
		store:     store,
		reader:    reader,
		now:       time.Now,
		jitter:    jitterHostSandboxDispatch,
		latest:    make(map[string]HostSandboxLatest),
		inFlight:  make(map[string]uint64),
		blocked:   make(map[string]bool),
		tokens:    make(map[string]uint64),
		requests:  make(chan struct{}, config.MaxConcurrentRequests),
		completed: make(chan struct{}, 1),
	}, nil
}

func (s *HostSandboxSampler) Run(ctx context.Context) {
	for {
		saturated := s.CollectOnce(ctx)
		var completed <-chan struct{}
		if saturated {
			completed = s.completed
		}
		if !waitForSamplerDispatch(ctx.Done(), s.jitter(s.nextDispatchInterval()), completed) {
			return
		}
	}
}

func (s *HostSandboxSampler) nextDispatchInterval() time.Duration {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	if !s.hasDispatchInterval {
		s.ensureCollectionCycleLocked()
	}
	return s.dispatchInterval
}

func (s *HostSandboxSampler) ensureCollectionCycleLocked() {
	if s.cycle != nil {
		return
	}
	boxes := s.store.List()
	s.cycle = make([]*cubeboxstore.CubeBox, len(boxes))
	copy(s.cycle, boxes)
	sort.Slice(s.cycle, func(i, j int) bool {
		if s.cycle[i] == nil {
			return false
		}
		if s.cycle[j] == nil {
			return true
		}
		return s.cycle[i].ID < s.cycle[j].ID
	})
	count := 0
	for _, cb := range s.cycle {
		if cb == nil || cb.ID == "" || cb.CGroupPath == "" {
			continue
		}
		status := cb.MainStatus()
		if hostSandboxSamplingUnavailable(status) {
			continue
		}
		count++
	}
	s.cycleNeedsCleanup = true
	s.dispatchInterval = batchDispatchInterval(s.config.CollectionInterval, s.config.MaxConcurrentRequests, count)
	s.hasDispatchInterval = true
	s.nextIndex = 0
}

func (s *HostSandboxSampler) finishCollectionCycleLocked() {
	s.cycle = nil
	s.cycleNeedsCleanup = false
	s.nextIndex = 0
}

func jitterHostSandboxDispatch(interval time.Duration) time.Duration {
	window := interval / 10
	if window <= 0 {
		return interval
	}
	return interval - window + time.Duration(rand.Int63n(int64(2*window)+1))
}

func (s *HostSandboxSampler) CollectOnce(ctx context.Context) bool {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	s.ensureCollectionCycleLocked()
	if s.cycleNeedsCleanup {
		present := make(map[string]struct{}, len(s.cycle))
		for _, cb := range s.cycle {
			if cb != nil && cb.ID != "" {
				present[cb.ID] = struct{}{}
			}
		}
		s.deleteMissing(present)
		s.cycleNeedsCleanup = false
	}
	if len(s.cycle) == 0 {
		s.finishCollectionCycleLocked()
		return false
	}
	for s.nextIndex < len(s.cycle) {
		cb := s.cycle[s.nextIndex]
		if cb == nil || cb.ID == "" {
			s.nextIndex++
			continue
		}
		if hostSandboxSamplingUnavailable(cb.MainStatus()) {
			s.SetLifecycleUnavailable(cb.ID, true)
			s.nextIndex++
			continue
		}
		s.SetLifecycleUnavailable(cb.ID, false)
		if cb.CGroupPath == "" {
			s.recordError(cb.ID, 0, fmt.Errorf("host cgroup path is unavailable"))
			s.nextIndex++
			continue
		}
		token, ok := s.start(cb.ID)
		if !ok {
			s.nextIndex++
			continue
		}
		select {
		case s.requests <- struct{}{}:
			go s.collect(ctx, cb, token)
			s.nextIndex++
		default:
			s.finish(cb.ID, token)
			return true
		}
	}
	s.finishCollectionCycleLocked()
	return false
}

func (s *HostSandboxSampler) collect(parent context.Context, cb *cubeboxstore.CubeBox, token uint64) {
	defer func() {
		<-s.requests
		s.finish(cb.ID, token)
		notifySamplerCompletion(s.completed)
	}()
	ctx, cancel := context.WithTimeout(parent, s.config.RequestTimeout)
	defer cancel()
	cgroupPath := cb.CGroupPath
	if cb.HostMetricsBaselineMissingAtAssignment {
		s.recordError(cb.ID, token, fmt.Errorf("host cgroup assignment baseline was not captured; recreate sandbox %s to restore host metrics", cb.ID))
		return
	}
	usage, err := s.reader.UsageSnapshot(ctx, cgroupPath)
	if err != nil {
		s.recordError(cb.ID, token, err)
		return
	}
	baseline := cb.HostMetricsBaselineCopy()
	if baseline == nil || baseline.CGroupPath != cgroupPath {
		previous := baseline
		candidate := hostMetricsBaselineFromUsage(cgroupPath, usage)
		cb.RestoreHostMetricsBaseline(&candidate)
		if err := s.store.SyncByID(ctx, cb.ID); err != nil {
			cb.RestoreHostMetricsBaselineIfCurrent(candidate, previous)
			s.recordError(cb.ID, token, fmt.Errorf("persist host sandbox counter baseline: %w", err))
			return
		}
		baseline = &candidate
	}
	normalized, err := normalizeHostSandboxUsage(usage, *baseline)
	if err != nil {
		s.recordError(cb.ID, token, err)
		return
	}
	collectedAt := s.now().UTC()
	s.record(token, HostSandboxLatest{
		SandboxID:    cb.ID,
		CGroupPath:   cgroupPath,
		CollectedAt:  collectedAt,
		Availability: HostSandboxAvailable,
		Snapshot:     newHostSandboxSnapshot(cb.ID, cgroupPath, collectedAt, normalized),
	})
}

func (s *HostSandboxSampler) Latest(sandboxID string, now time.Time) (HostSandboxLatest, bool) {
	s.mu.RLock()
	latest, ok := s.latest[sandboxID]
	s.mu.RUnlock()
	if !ok {
		return HostSandboxLatest{}, false
	}
	if latest.Availability == HostSandboxAvailable && now.Sub(latest.CollectedAt) > s.config.StaleAfter {
		latest.Availability = HostSandboxStale
	}
	return cloneHostSandboxLatest(latest), true
}

func (s *HostSandboxSampler) ListLatest(now time.Time) []HostSandboxLatest {
	s.mu.RLock()
	latest := make([]HostSandboxLatest, 0, len(s.latest))
	for _, item := range s.latest {
		copied := cloneHostSandboxLatest(item)
		if copied.Availability == HostSandboxAvailable && now.Sub(copied.CollectedAt) > s.config.StaleAfter {
			copied.Availability = HostSandboxStale
		}
		latest = append(latest, copied)
	}
	s.mu.RUnlock()
	sort.Slice(latest, func(i, j int) bool { return latest[i].SandboxID < latest[j].SandboxID })
	return latest
}

func (s *HostSandboxSampler) SetLifecycleUnavailable(sandboxID string, unavailable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !unavailable {
		delete(s.blocked, sandboxID)
		return
	}
	s.blocked[sandboxID] = true
	latest := s.latest[sandboxID]
	latest.SandboxID = sandboxID
	latest.Availability = HostSandboxUnavailable
	s.latest[sandboxID] = latest
}

func (s *HostSandboxSampler) start(sandboxID string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inFlight[sandboxID]; ok {
		return 0, false
	}
	s.nextToken++
	token := s.nextToken
	s.inFlight[sandboxID] = token
	s.tokens[sandboxID] = token
	return token, true
}

func (s *HostSandboxSampler) finish(sandboxID string, token uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[sandboxID] == token {
		delete(s.inFlight, sandboxID)
	}
}

func (s *HostSandboxSampler) record(token uint64, latest HostSandboxLatest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[latest.SandboxID] != token {
		return
	}
	if s.blocked[latest.SandboxID] {
		latest.Availability = HostSandboxUnavailable
	}
	s.latest[latest.SandboxID] = cloneHostSandboxLatest(latest)
}

func (s *HostSandboxSampler) recordError(sandboxID string, token uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token != 0 && s.tokens[sandboxID] != token {
		return
	}
	latest := s.latest[sandboxID]
	latest.SandboxID = sandboxID
	if latest.Snapshot == nil || s.blocked[sandboxID] {
		latest.Availability = HostSandboxUnavailable
	}
	latest.LastError = err.Error()
	s.latest[sandboxID] = latest
}

func (s *HostSandboxSampler) deleteMissing(present map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sandboxID := range s.tokens {
		if _, ok := present[sandboxID]; ok {
			continue
		}
		delete(s.latest, sandboxID)
		delete(s.blocked, sandboxID)
		delete(s.inFlight, sandboxID)
		delete(s.tokens, sandboxID)
	}
	for sandboxID := range s.latest {
		if _, ok := present[sandboxID]; !ok {
			delete(s.latest, sandboxID)
			delete(s.blocked, sandboxID)
		}
	}
}

func newHostSandboxSnapshot(sandboxID, cgroupPath string, timestamp time.Time, usage handle.UsageSnapshot) *HostSandboxSnapshot {
	return &HostSandboxSnapshot{
		Timestamp:                timestamp,
		SandboxID:                sandboxID,
		CGroupPath:               cgroupPath,
		CPUUsageTotalNS:          usage.CPUUsageTotalNS,
		CPUUserTotalNS:           usage.CPUUserTotalNS,
		CPUSystemTotalNS:         usage.CPUSystemTotalNS,
		CPUThrottledTotalNS:      usage.CPUThrottledTotalNS,
		CPUPeriodsTotal:          usage.CPUPeriodsTotal,
		CPUThrottledPeriodsTotal: usage.CPUThrottledPeriodsTotal,
		CPULimitQuotaUS:          usage.CPULimit.QuotaUS,
		CPULimitPeriodUS:         usage.CPULimit.PeriodUS,
		CPULimitUnlimited:        usage.CPULimit.Unlimited,
		MemoryCurrentBytes:       usage.MemoryCurrentBytes,
		MemoryLimitBytes:         usage.MemoryLimit.Value,
		MemoryLimitUnlimited:     usage.MemoryLimit.Unlimited,
		MemoryFailuresTotal:      usage.MemoryFailuresTotal,
	}
}

func hostMetricsBaselineFromUsage(cgroupPath string, usage handle.UsageSnapshot) cubeboxstore.HostMetricsBaseline {
	return cubeboxstore.HostMetricsBaseline{
		CGroupPath:               cgroupPath,
		CPUUsageTotalNS:          usage.CPUUsageTotalNS,
		CPUUserTotalNS:           usage.CPUUserTotalNS,
		CPUSystemTotalNS:         usage.CPUSystemTotalNS,
		CPUThrottledTotalNS:      usage.CPUThrottledTotalNS,
		CPUPeriodsTotal:          usage.CPUPeriodsTotal,
		CPUThrottledPeriodsTotal: usage.CPUThrottledPeriodsTotal,
		MemoryFailuresTotal:      usage.MemoryFailuresTotal,
	}
}

func normalizeHostSandboxUsage(usage handle.UsageSnapshot, baseline cubeboxstore.HostMetricsBaseline) (handle.UsageSnapshot, error) {
	if baseline.CGroupPath == "" {
		return handle.UsageSnapshot{}, fmt.Errorf("host sandbox counter baseline cgroup path is required")
	}
	result := usage
	counters := []struct {
		name     string
		current  uint64
		baseline uint64
		target   *uint64
	}{
		{name: "CPU usage", current: usage.CPUUsageTotalNS, baseline: baseline.CPUUsageTotalNS, target: &result.CPUUsageTotalNS},
		{name: "CPU user", current: usage.CPUUserTotalNS, baseline: baseline.CPUUserTotalNS, target: &result.CPUUserTotalNS},
		{name: "CPU system", current: usage.CPUSystemTotalNS, baseline: baseline.CPUSystemTotalNS, target: &result.CPUSystemTotalNS},
		{name: "CPU throttled time", current: usage.CPUThrottledTotalNS, baseline: baseline.CPUThrottledTotalNS, target: &result.CPUThrottledTotalNS},
		{name: "CPU periods", current: usage.CPUPeriodsTotal, baseline: baseline.CPUPeriodsTotal, target: &result.CPUPeriodsTotal},
		{name: "CPU throttled periods", current: usage.CPUThrottledPeriodsTotal, baseline: baseline.CPUThrottledPeriodsTotal, target: &result.CPUThrottledPeriodsTotal},
		{name: "memory failures", current: usage.MemoryFailuresTotal, baseline: baseline.MemoryFailuresTotal, target: &result.MemoryFailuresTotal},
	}
	for _, counter := range counters {
		if counter.current < counter.baseline {
			return handle.UsageSnapshot{}, fmt.Errorf("host sandbox %s counter regressed below persisted baseline", counter.name)
		}
		*counter.target = counter.current - counter.baseline
	}
	return result, nil
}

func cloneHostSandboxLatest(latest HostSandboxLatest) HostSandboxLatest {
	copied := latest
	if latest.Snapshot != nil {
		snapshot := *latest.Snapshot
		copied.Snapshot = &snapshot
	}
	return copied
}
