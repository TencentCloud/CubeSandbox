package resourcemetrics

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"k8s.io/apimachinery/pkg/api/resource"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

type GuestWorkloadSamplerConfig struct {
	CollectionInterval    time.Duration
	RequestTimeout        time.Duration
	MaxConcurrentRequests int
	StaleAfter            time.Duration
}

func (c GuestWorkloadSamplerConfig) validate() error {
	if c.CollectionInterval <= 0 {
		return fmt.Errorf("guest workload collection interval must be positive")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("guest workload request timeout must be positive")
	}
	if c.MaxConcurrentRequests <= 0 {
		return fmt.Errorf("guest workload max concurrent requests must be positive")
	}
	if c.StaleAfter < c.CollectionInterval {
		return fmt.Errorf("guest workload stale after must not be less than collection interval")
	}
	return nil
}

type guestWorkloadCubeboxStore interface {
	List() []*cubeboxstore.CubeBox
	SyncByID(context.Context, string) error
}

type guestWorkloadMetricsReader interface {
	Metrics(context.Context, string) (*types.Metric, error)
}

type GuestWorkloadAvailability string

const (
	GuestWorkloadAvailable   GuestWorkloadAvailability = "available"
	GuestWorkloadStale       GuestWorkloadAvailability = "stale"
	GuestWorkloadUnavailable GuestWorkloadAvailability = "unavailable"
)

type GuestWorkloadLatest struct {
	Workload             WorkloadRef
	EpochGeneration      uint64
	EpochStartedAt       time.Time
	EpochReadyAt         *time.Time
	CollectedAt          time.Time
	Availability         GuestWorkloadAvailability
	CumulativeAvailable  bool
	CPULimitMillicores   *int64
	Snapshot             *GuestWorkloadSnapshot
	MemoryCurrentBytes   uint64
	MemoryLimitBytes     uint64
	MemoryLimitUnlimited bool
	LastError            string
}

// guestEpochID is the identity of one metrics epoch. Generation alone is not
// unique across a sandbox's life: a resumed sandbox starts a fresh incarnation
// that may reuse generation 1 with a newer StartedAt, so every comparison must
// include the start time.
type guestEpochID struct {
	Generation uint64
	StartedAt  time.Time
}

func guestEpochIDOf(epoch *cubeboxstore.GuestMetricsEpoch) guestEpochID {
	if epoch == nil {
		return guestEpochID{}
	}
	return guestEpochID{Generation: epoch.Generation, StartedAt: epoch.StartedAt}
}

func (latest GuestWorkloadLatest) epochID() guestEpochID {
	return guestEpochID{Generation: latest.EpochGeneration, StartedAt: latest.EpochStartedAt}
}

func (id guestEpochID) equal(other guestEpochID) bool {
	return id.Generation == other.Generation && id.StartedAt.Equal(other.StartedAt)
}

func (id guestEpochID) isZero() bool {
	return id.Generation == 0 && id.StartedAt.IsZero()
}

type GuestWorkloadSampler struct {
	config GuestWorkloadSamplerConfig
	store  guestWorkloadCubeboxStore
	reader guestWorkloadMetricsReader
	now    func() time.Time
	jitter func(time.Duration) time.Duration

	mu        sync.RWMutex
	latest    map[string]GuestWorkloadLatest
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

func (s *GuestWorkloadSampler) InvalidateEpoch(workload WorkloadRef, epoch *cubeboxstore.GuestMetricsEpoch) {
	s.invalidateIfEpochChanged(workload, epoch)
}

func (s *GuestWorkloadSampler) SetLifecycleUnavailable(workload WorkloadRef, unavailable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := guestWorkloadKey(workload)
	if !unavailable {
		delete(s.blocked, key)
		return
	}
	s.blocked[key] = true
	latest := s.latest[key]
	latest.Workload = workload
	latest.Availability = GuestWorkloadUnavailable
	latest.CumulativeAvailable = false
	s.latest[key] = latest
}

func NewGuestWorkloadSampler(
	config GuestWorkloadSamplerConfig,
	store guestWorkloadCubeboxStore,
	reader guestWorkloadMetricsReader,
) (*GuestWorkloadSampler, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("guest workload cubebox store is required")
	}
	if reader == nil {
		return nil, fmt.Errorf("guest workload metrics reader is required")
	}
	return &GuestWorkloadSampler{
		config:    config,
		store:     store,
		reader:    reader,
		now:       time.Now,
		jitter:    jitterGuestWorkloadDispatch,
		latest:    make(map[string]GuestWorkloadLatest),
		inFlight:  make(map[string]uint64),
		blocked:   make(map[string]bool),
		tokens:    make(map[string]uint64),
		requests:  make(chan struct{}, config.MaxConcurrentRequests),
		completed: make(chan struct{}, 1),
	}, nil
}

func (s *GuestWorkloadSampler) Run(ctx context.Context) {
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

func (s *GuestWorkloadSampler) nextDispatchInterval() time.Duration {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	if !s.hasDispatchInterval {
		s.ensureCollectionCycleLocked()
	}
	return s.dispatchInterval
}

func (s *GuestWorkloadSampler) ensureCollectionCycleLocked() {
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
		if cb == nil || cb.ID == "" {
			continue
		}
		status := cb.MainStatus()
		epoch := cb.GuestMetricsEpochCopy()
		if guestWorkloadSamplingUnavailable(status) || epoch == nil || epoch.State == cubeboxstore.GuestMetricsEpochPrepared {
			continue
		}
		count++
	}
	s.cycleNeedsCleanup = true
	s.dispatchInterval = batchDispatchInterval(s.config.CollectionInterval, s.config.MaxConcurrentRequests, count)
	s.hasDispatchInterval = true
	s.nextIndex = 0
}

func (s *GuestWorkloadSampler) finishCollectionCycleLocked() {
	s.cycle = nil
	s.cycleNeedsCleanup = false
	s.nextIndex = 0
}

func jitterGuestWorkloadDispatch(interval time.Duration) time.Duration {
	window := interval / 10
	if window <= 0 {
		return interval
	}
	return interval - window + time.Duration(rand.Int63n(int64(2*window)+1))
}

func (s *GuestWorkloadSampler) CollectOnce(ctx context.Context) bool {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	s.ensureCollectionCycleLocked()
	if s.cycleNeedsCleanup {
		present := make(map[string]struct{}, len(s.cycle))
		for _, cb := range s.cycle {
			if cb != nil && cb.ID != "" {
				present[guestWorkloadKey(WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID})] = struct{}{}
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
		workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}
		epoch := cb.GuestMetricsEpochCopy()
		s.invalidateIfEpochChanged(workload, epoch)
		if guestWorkloadSamplingUnavailable(cb.MainStatus()) {
			s.markUnavailable(workload)
			s.nextIndex++
			continue
		}
		s.SetLifecycleUnavailable(workload, false)
		if epoch == nil {
			var err error
			epoch, err = s.ensureFreshEpoch(ctx, cb)
			if err != nil {
				s.recordError(workload, guestEpochID{}, 0, err)
				s.nextIndex++
				continue
			}
			s.invalidateIfEpochChanged(workload, epoch)
		}
		if epoch.State == cubeboxstore.GuestMetricsEpochPrepared {
			s.markUnavailable(workload)
			s.nextIndex++
			continue
		}
		token, ok := s.start(workload)
		if !ok {
			s.nextIndex++
			continue
		}
		namespace := cb.Namespace
		if namespace == "" {
			namespace = namespaces.Default
		}
		epochID := guestEpochIDOf(epoch)
		cpuLimitMillicores, err := guestWorkloadCPULimitMillicores(cb)
		if err != nil {
			s.recordError(workload, epochID, token, err)
			s.finish(workload, token)
			s.nextIndex++
			continue
		}
		select {
		case s.requests <- struct{}{}:
			go s.collect(ctx, cb, workload, namespace, cpuLimitMillicores, epochID, token)
			s.nextIndex++
		default:
			s.finish(workload, token)
			return true
		}
	}
	s.finishCollectionCycleLocked()
	return false
}

func (s *GuestWorkloadSampler) ensureFreshEpoch(ctx context.Context, cb *cubeboxstore.CubeBox) (*cubeboxstore.GuestMetricsEpoch, error) {
	status := cb.MainStatus().Get()
	startedAt := time.Unix(0, status.StartedAt).UTC()
	created, err := cb.BeginFreshGuestMetricsEpochIfMissing(startedAt)
	if err != nil {
		return nil, fmt.Errorf("initialize missing fresh guest workload metrics epoch: %w", err)
	}
	if created {
		if err := s.store.SyncByID(ctx, cb.ID); err != nil {
			cb.RestoreGuestMetricsEpochIfCurrent(1, cubeboxstore.GuestMetricsEpochPending, nil)
			return nil, fmt.Errorf("persist missing fresh guest workload metrics epoch: %w", err)
		}
	}
	epoch := cb.GuestMetricsEpochCopy()
	if epoch == nil {
		return nil, fmt.Errorf("fresh guest workload metrics epoch is unavailable")
	}
	return epoch, nil
}

func (s *GuestWorkloadSampler) Latest(workload WorkloadRef, now time.Time) (GuestWorkloadLatest, bool) {
	s.mu.RLock()
	latest, ok := s.latest[guestWorkloadKey(workload)]
	s.mu.RUnlock()
	if !ok {
		return GuestWorkloadLatest{}, false
	}
	if latest.Availability == GuestWorkloadAvailable && now.Sub(latest.CollectedAt) > s.config.StaleAfter {
		latest.Availability = GuestWorkloadStale
	}
	return cloneGuestWorkloadLatest(latest), true
}

func (s *GuestWorkloadSampler) ListLatest(now time.Time) []GuestWorkloadLatest {
	s.mu.RLock()
	latest := make([]GuestWorkloadLatest, 0, len(s.latest))
	for _, item := range s.latest {
		copied := cloneGuestWorkloadLatest(item)
		if copied.Availability == GuestWorkloadAvailable && now.Sub(copied.CollectedAt) > s.config.StaleAfter {
			copied.Availability = GuestWorkloadStale
		}
		latest = append(latest, copied)
	}
	s.mu.RUnlock()
	sort.Slice(latest, func(i, j int) bool {
		return guestWorkloadKey(latest[i].Workload) < guestWorkloadKey(latest[j].Workload)
	})
	return latest
}

// invalidateIfEpochChanged resets the cached window whenever the store epoch
// identity differs, including a generation reset after pause/resume. Generation
// is not monotonic across a fresh incarnation, so the full identity decides.
func (s *GuestWorkloadSampler) invalidateIfEpochChanged(workload WorkloadRef, epoch *cubeboxstore.GuestMetricsEpoch) {
	if epoch == nil {
		return
	}
	id := guestEpochIDOf(epoch)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := guestWorkloadKey(workload)
	if current, found := s.latest[key]; found && current.epochID().equal(id) {
		return
	}
	s.latest[key] = GuestWorkloadLatest{
		Workload:        workload,
		EpochGeneration: id.Generation,
		EpochStartedAt:  id.StartedAt,
		EpochReadyAt:    epoch.ReadyAt,
		Availability:    GuestWorkloadUnavailable,
		LastError:       "guest workload metrics epoch changed; awaiting baseline",
	}
}

func (s *GuestWorkloadSampler) collect(
	parent context.Context,
	cb *cubeboxstore.CubeBox,
	workload WorkloadRef,
	namespace string,
	cpuLimitMillicores *int64,
	scheduledEpoch guestEpochID,
	token uint64,
) {
	defer func() {
		<-s.requests
		s.finish(workload, token)
		notifySamplerCompletion(s.completed)
	}()
	ctx, cancel := context.WithTimeout(
		namespaces.WithNamespace(parent, namespace),
		s.config.RequestTimeout,
	)
	defer cancel()
	metric, err := s.reader.Metrics(ctx, workload.SandboxID)
	if err != nil {
		if errdefs.IsFailedPrecondition(err) {
			s.recordFatalError(workload, scheduledEpoch, token, err)
		} else {
			s.recordError(workload, scheduledEpoch, token, err)
		}
		return
	}
	raw, err := DecodeGuestWorkloadMetric(metric, workload)
	if err != nil {
		s.recordError(workload, scheduledEpoch, token, err)
		return
	}
	raw.CPULimitMillicores = cloneInt64(cpuLimitMillicores)
	epoch := cb.GuestMetricsEpochCopy()
	if epoch == nil {
		s.delete(workload)
		return
	}
	if !guestEpochIDOf(epoch).equal(scheduledEpoch) {
		s.invalidateIfEpochChanged(workload, epoch)
		return
	}

	switch epoch.State {
	case cubeboxstore.GuestMetricsEpochPending:
		s.readyPendingEpoch(ctx, cb, workload, epoch, raw, token)
	case cubeboxstore.GuestMetricsEpochPrepared:
		s.recordUnavailable(workload, epoch, raw, token, "guest workload rollback epoch is prepared")
	case cubeboxstore.GuestMetricsEpochReady:
		s.recordReadyEpoch(ctx, cb, workload, epoch, raw, token)
	case cubeboxstore.GuestMetricsEpochDegraded:
		s.recordUnavailable(workload, epoch, raw, token, "guest workload epoch is degraded")
	default:
		s.recordError(workload, guestEpochIDOf(epoch), token, fmt.Errorf("guest workload epoch generation %d has unsupported state %q", epoch.Generation, epoch.State))
	}
}

func (s *GuestWorkloadSampler) readyPendingEpoch(
	ctx context.Context,
	cb *cubeboxstore.CubeBox,
	workload WorkloadRef,
	epoch *cubeboxstore.GuestMetricsEpoch,
	raw GuestWorkloadRawSample,
	token uint64,
) {
	baseline := baselineFromRaw(raw)
	previous := epoch.DeepCopy()
	readyAt := s.now().UTC()
	if err := cb.ReadyGuestMetricsEpoch(epoch.Generation, baseline, readyAt); err != nil {
		s.recordError(workload, guestEpochIDOf(epoch), token, err)
		return
	}
	if err := s.store.SyncByID(ctx, cb.ID); err != nil {
		cb.RestoreGuestMetricsEpochIfCurrent(
			epoch.Generation,
			cubeboxstore.GuestMetricsEpochReady,
			previous,
		)
		s.recordPending(workload, epoch, raw, token, fmt.Errorf("persist guest workload epoch baseline: %w", err))
		return
	}
	ready := cb.GuestMetricsEpochCopy()
	if ready == nil || !guestEpochIDOf(ready).equal(guestEpochIDOf(epoch)) || ready.State != cubeboxstore.GuestMetricsEpochReady {
		s.invalidateIfEpochChanged(workload, ready)
		return
	}
	s.recordReadyEpoch(ctx, cb, workload, ready, raw, token)
}

func (s *GuestWorkloadSampler) recordReadyEpoch(
	ctx context.Context,
	cb *cubeboxstore.CubeBox,
	workload WorkloadRef,
	epoch *cubeboxstore.GuestMetricsEpoch,
	raw GuestWorkloadRawSample,
	token uint64,
) {
	if epoch.Baseline == nil || epoch.Baseline.ContainerID != workload.ContainerID {
		s.recordError(workload, guestEpochIDOf(epoch), token, fmt.Errorf("guest workload epoch generation %d has no baseline for %s", epoch.Generation, workload.ContainerID))
		return
	}
	snapshot, err := NormalizeGuestWorkloadMetrics(raw, baselineForEpoch(workload, *epoch.Baseline, epoch.Generation))
	if err != nil {
		previous := epoch.DeepCopy()
		if degradeErr := cb.DegradeGuestMetricsEpoch(epoch.Generation); degradeErr != nil {
			s.recordError(workload, guestEpochIDOf(epoch), token, fmt.Errorf("normalize guest workload metrics: %w", err))
			return
		}
		if persistErr := s.store.SyncByID(ctx, cb.ID); persistErr != nil {
			cb.RestoreGuestMetricsEpochIfCurrent(
				epoch.Generation,
				cubeboxstore.GuestMetricsEpochDegraded,
				previous,
			)
			s.recordError(workload, guestEpochIDOf(epoch), token, fmt.Errorf("persist degraded guest workload epoch: %w", persistErr))
			return
		}
		degraded := cb.GuestMetricsEpochCopy()
		if degraded == nil || !guestEpochIDOf(degraded).equal(guestEpochIDOf(epoch)) || degraded.State != cubeboxstore.GuestMetricsEpochDegraded {
			s.invalidateIfEpochChanged(workload, degraded)
			return
		}
		s.recordUnavailable(workload, degraded, raw, token, err.Error())
		return
	}
	current := cb.GuestMetricsEpochCopy()
	if current == nil || !guestEpochIDOf(current).equal(guestEpochIDOf(epoch)) || current.State != cubeboxstore.GuestMetricsEpochReady {
		s.invalidateIfEpochChanged(workload, current)
		return
	}
	s.record(token, GuestWorkloadLatest{
		Workload:             workload,
		EpochGeneration:      epoch.Generation,
		EpochStartedAt:       epoch.StartedAt,
		EpochReadyAt:         epoch.ReadyAt,
		CollectedAt:          raw.Timestamp,
		Availability:         GuestWorkloadAvailable,
		CumulativeAvailable:  true,
		CPULimitMillicores:   cloneInt64(raw.CPULimitMillicores),
		Snapshot:             &snapshot,
		MemoryCurrentBytes:   raw.MemoryCurrentBytes,
		MemoryLimitBytes:     raw.MemoryLimitBytes,
		MemoryLimitUnlimited: raw.MemoryLimitUnlimited,
	})
}

func (s *GuestWorkloadSampler) recordPending(workload WorkloadRef, epoch *cubeboxstore.GuestMetricsEpoch, raw GuestWorkloadRawSample, token uint64, err error) {
	s.record(token, GuestWorkloadLatest{
		Workload:             workload,
		EpochGeneration:      epoch.Generation,
		EpochStartedAt:       epoch.StartedAt,
		CollectedAt:          raw.Timestamp,
		Availability:         GuestWorkloadAvailable,
		CumulativeAvailable:  false,
		CPULimitMillicores:   cloneInt64(raw.CPULimitMillicores),
		MemoryCurrentBytes:   raw.MemoryCurrentBytes,
		MemoryLimitBytes:     raw.MemoryLimitBytes,
		MemoryLimitUnlimited: raw.MemoryLimitUnlimited,
		LastError:            err.Error(),
	})
}

func (s *GuestWorkloadSampler) recordUnavailable(workload WorkloadRef, epoch *cubeboxstore.GuestMetricsEpoch, raw GuestWorkloadRawSample, token uint64, reason string) {
	latest := GuestWorkloadLatest{
		Workload:             workload,
		CollectedAt:          raw.Timestamp,
		Availability:         GuestWorkloadUnavailable,
		MemoryCurrentBytes:   raw.MemoryCurrentBytes,
		MemoryLimitBytes:     raw.MemoryLimitBytes,
		MemoryLimitUnlimited: raw.MemoryLimitUnlimited,
		CPULimitMillicores:   cloneInt64(raw.CPULimitMillicores),
		LastError:            reason,
	}
	if epoch != nil {
		latest.EpochGeneration = epoch.Generation
		latest.EpochStartedAt = epoch.StartedAt
		latest.EpochReadyAt = epoch.ReadyAt
	}
	s.record(token, latest)
}

// recordError keeps the cached window when epoch is zero, which means the
// epoch could not be determined (for example, ensureFreshEpoch failed).
func (s *GuestWorkloadSampler) recordError(workload WorkloadRef, epoch guestEpochID, token uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := guestWorkloadKey(workload)
	if token != 0 && s.tokens[key] != token {
		return
	}
	latest, found := s.latest[key]
	if found && !epoch.isZero() && !latest.epochID().equal(epoch) {
		return
	}
	retainLastSuccess := found &&
		latest.Availability == GuestWorkloadAvailable &&
		!latest.CollectedAt.IsZero() &&
		(epoch.isZero() || latest.epochID().equal(epoch))
	latest.Workload = workload
	if !epoch.isZero() {
		latest.EpochGeneration = epoch.Generation
		latest.EpochStartedAt = epoch.StartedAt
	}
	if !retainLastSuccess || s.blocked[key] {
		latest.Availability = GuestWorkloadUnavailable
		latest.CumulativeAvailable = false
	}
	latest.LastError = err.Error()
	s.latest[key] = latest
}

func (s *GuestWorkloadSampler) recordFatalError(workload WorkloadRef, epoch guestEpochID, token uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := guestWorkloadKey(workload)
	if token != 0 && s.tokens[key] != token {
		return
	}
	latest, found := s.latest[key]
	if found && !epoch.isZero() && !latest.epochID().equal(epoch) {
		return
	}
	latest.Workload = workload
	if !epoch.isZero() {
		latest.EpochGeneration = epoch.Generation
		latest.EpochStartedAt = epoch.StartedAt
	}
	latest.Availability = GuestWorkloadUnavailable
	latest.CumulativeAvailable = false
	latest.LastError = err.Error()
	s.latest[key] = latest
}

func (s *GuestWorkloadSampler) markUnavailable(workload WorkloadRef) {
	s.SetLifecycleUnavailable(workload, true)
}

func (s *GuestWorkloadSampler) record(token uint64, latest GuestWorkloadLatest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := guestWorkloadKey(latest.Workload)
	if token != 0 && s.tokens[key] != token {
		return
	}
	if current, ok := s.latest[key]; ok && !current.epochID().equal(latest.epochID()) {
		return
	}
	if s.blocked[key] {
		latest.Availability = GuestWorkloadUnavailable
		latest.CumulativeAvailable = false
	}
	s.latest[key] = cloneGuestWorkloadLatest(latest)
}

func (s *GuestWorkloadSampler) delete(workload WorkloadRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.latest, guestWorkloadKey(workload))
	delete(s.blocked, guestWorkloadKey(workload))
}

func (s *GuestWorkloadSampler) deleteMissing(present map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.tokens {
		if _, ok := present[key]; !ok {
			delete(s.latest, key)
			delete(s.blocked, key)
			delete(s.inFlight, key)
			delete(s.tokens, key)
		}
	}
	for key := range s.latest {
		if _, ok := present[key]; !ok {
			delete(s.latest, key)
			delete(s.blocked, key)
		}
	}
}

func (s *GuestWorkloadSampler) start(workload WorkloadRef) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := guestWorkloadKey(workload)
	if _, ok := s.inFlight[key]; ok {
		return 0, false
	}
	s.nextToken++
	token := s.nextToken
	s.inFlight[key] = token
	s.tokens[key] = token
	return token, true
}

func (s *GuestWorkloadSampler) finish(workload WorkloadRef, token uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := guestWorkloadKey(workload)
	if s.inFlight[key] == token {
		delete(s.inFlight, key)
	}
}

func guestWorkloadKey(workload WorkloadRef) string {
	return workload.SandboxID + "/" + workload.ContainerID
}

func baselineFromRaw(raw GuestWorkloadRawSample) cubeboxstore.GuestMetricsEpochBaseline {
	return cubeboxstore.GuestMetricsEpochBaseline{
		ContainerID:              raw.Workload.ContainerID,
		CPUUsageTotalNS:          raw.Counters.CPUUsageTotalNS,
		CPUUserTotalNS:           raw.Counters.CPUUserTotalNS,
		CPUSystemTotalNS:         raw.Counters.CPUSystemTotalNS,
		CPUThrottledTotalNS:      raw.Counters.CPUThrottledTotalNS,
		CPUPeriodsTotal:          raw.Counters.CPUPeriodsTotal,
		CPUThrottledPeriodsTotal: raw.Counters.CPUThrottledPeriodsTotal,
		MemoryFailuresTotal:      raw.Counters.MemoryFailuresTotal,
	}
}

func baselineForEpoch(workload WorkloadRef, baseline cubeboxstore.GuestMetricsEpochBaseline, generation uint64) GuestWorkloadCounterBaseline {
	return GuestWorkloadCounterBaseline{
		ID:       fmt.Sprintf("%s/%s/%d", workload.SandboxID, workload.ContainerID, generation),
		Workload: workload,
		Counters: GuestWorkloadCounters{
			CPUUsageTotalNS:          baseline.CPUUsageTotalNS,
			CPUUserTotalNS:           baseline.CPUUserTotalNS,
			CPUSystemTotalNS:         baseline.CPUSystemTotalNS,
			CPUThrottledTotalNS:      baseline.CPUThrottledTotalNS,
			CPUPeriodsTotal:          baseline.CPUPeriodsTotal,
			CPUThrottledPeriodsTotal: baseline.CPUThrottledPeriodsTotal,
			MemoryFailuresTotal:      baseline.MemoryFailuresTotal,
		},
	}
}

func cloneGuestWorkloadLatest(latest GuestWorkloadLatest) GuestWorkloadLatest {
	copied := latest
	if latest.EpochReadyAt != nil {
		readyAt := *latest.EpochReadyAt
		copied.EpochReadyAt = &readyAt
	}
	if latest.Snapshot != nil {
		snapshot := *latest.Snapshot
		snapshot.CPULimitMillicores = cloneInt64(latest.Snapshot.CPULimitMillicores)
		copied.Snapshot = &snapshot
	}
	copied.CPULimitMillicores = cloneInt64(latest.CPULimitMillicores)
	return copied
}

func guestWorkloadCPULimitMillicores(cb *cubeboxstore.CubeBox) (*int64, error) {
	if cb == nil {
		return nil, nil
	}
	container := cb.FirstContainer()
	if container == nil || container.Config == nil || container.Config.GetResources() == nil {
		return nil, nil
	}
	cpu := container.Config.GetResources().GetCpuLimit()
	if cpu == "" {
		cpu = container.Config.GetResources().GetCpu()
	}
	if cpu == "" {
		return nil, nil
	}
	quantity, err := resource.ParseQuantity(cpu)
	if err != nil {
		return nil, fmt.Errorf("parse guest workload CPU limit %q: %w", cpu, err)
	}
	millicores := quantity.MilliValue()
	if millicores <= 0 {
		return nil, fmt.Errorf("guest workload CPU limit must be positive, got %q", cpu)
	}
	return &millicores, nil
}
