package cubebox

import (
	"fmt"
	"time"
)

type GuestMetricsEpochState string

const (
	GuestMetricsEpochPrepared GuestMetricsEpochState = "prepared"
	GuestMetricsEpochPending  GuestMetricsEpochState = "pending"
	GuestMetricsEpochReady    GuestMetricsEpochState = "ready"
	GuestMetricsEpochDegraded GuestMetricsEpochState = "degraded"
)

type GuestMetricsEpochReason string

const (
	GuestMetricsEpochFreshCreate GuestMetricsEpochReason = "fresh_create"
	GuestMetricsEpochRollback    GuestMetricsEpochReason = "rollback"
)

type GuestMetricsEpoch struct {
	Generation uint64                     `json:"generation"`
	StartedAt  time.Time                  `json:"started_at"`
	ReadyAt    *time.Time                 `json:"ready_at,omitempty"`
	Reason     GuestMetricsEpochReason    `json:"reason"`
	State      GuestMetricsEpochState     `json:"state"`
	Baseline   *GuestMetricsEpochBaseline `json:"baseline,omitempty"`
}

type GuestMetricsEpochBaseline struct {
	ContainerID              string `json:"container_id"`
	CPUUsageTotalNS          uint64 `json:"cpu_usage_total_ns"`
	CPUUserTotalNS           uint64 `json:"cpu_user_total_ns"`
	CPUSystemTotalNS         uint64 `json:"cpu_system_total_ns"`
	CPUThrottledTotalNS      uint64 `json:"cpu_throttled_total_ns"`
	CPUPeriodsTotal          uint64 `json:"cpu_periods_total"`
	CPUThrottledPeriodsTotal uint64 `json:"cpu_throttled_periods_total"`
	MemoryFailuresTotal      uint64 `json:"memory_failures_total"`
}

func (e *GuestMetricsEpoch) DeepCopy() *GuestMetricsEpoch {
	if e == nil {
		return nil
	}
	copied := *e
	if e.ReadyAt != nil {
		readyAt := *e.ReadyAt
		copied.ReadyAt = &readyAt
	}
	if e.Baseline != nil {
		baseline := *e.Baseline
		copied.Baseline = &baseline
	}
	return &copied
}

func (cb *CubeBox) BeginGuestMetricsEpoch(reason GuestMetricsEpochReason, startedAt time.Time) error {
	return cb.beginGuestMetricsEpoch(reason, GuestMetricsEpochPending, startedAt)
}

func (cb *CubeBox) BeginFreshGuestMetricsEpochIfMissing(startedAt time.Time) (bool, error) {
	if cb == nil {
		return false, fmt.Errorf("cubebox is required")
	}
	cb.guestMetricsEpochLock.Lock()
	defer cb.guestMetricsEpochLock.Unlock()
	if cb.GuestMetricsEpoch != nil {
		return false, nil
	}
	if startedAt.IsZero() {
		return false, fmt.Errorf("guest metrics epoch start time is required")
	}
	cb.GuestMetricsEpoch = &GuestMetricsEpoch{
		Generation: 1,
		StartedAt:  startedAt.UTC(),
		Reason:     GuestMetricsEpochFreshCreate,
		State:      GuestMetricsEpochPending,
	}
	return true, nil
}

func (cb *CubeBox) PrepareRollbackGuestMetricsEpoch(startedAt time.Time) error {
	return cb.beginGuestMetricsEpoch(GuestMetricsEpochRollback, GuestMetricsEpochPrepared, startedAt)
}

func (cb *CubeBox) beginGuestMetricsEpoch(
	reason GuestMetricsEpochReason,
	state GuestMetricsEpochState,
	startedAt time.Time,
) error {
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	cb.guestMetricsEpochLock.Lock()
	defer cb.guestMetricsEpochLock.Unlock()
	if startedAt.IsZero() {
		return fmt.Errorf("guest metrics epoch start time is required")
	}
	switch reason {
	case GuestMetricsEpochFreshCreate, GuestMetricsEpochRollback:
	default:
		return fmt.Errorf("unsupported guest metrics epoch reason %q", reason)
	}

	generation := uint64(1)
	if cb.GuestMetricsEpoch != nil {
		if cb.GuestMetricsEpoch.Generation == ^uint64(0) {
			return fmt.Errorf("guest metrics epoch generation overflow")
		}
		generation = cb.GuestMetricsEpoch.Generation + 1
	}
	cb.GuestMetricsEpoch = &GuestMetricsEpoch{
		Generation: generation,
		StartedAt:  startedAt.UTC(),
		Reason:     reason,
		State:      state,
	}
	return nil
}

func (cb *CubeBox) ActivatePreparedGuestMetricsEpoch(generation uint64) error {
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	cb.guestMetricsEpochLock.Lock()
	defer cb.guestMetricsEpochLock.Unlock()
	if cb.GuestMetricsEpoch == nil || cb.GuestMetricsEpoch.Generation != generation {
		return fmt.Errorf("guest metrics epoch generation %d is no longer current", generation)
	}
	if cb.GuestMetricsEpoch.State != GuestMetricsEpochPrepared {
		return fmt.Errorf("guest metrics epoch generation %d is not prepared", generation)
	}
	cb.GuestMetricsEpoch.State = GuestMetricsEpochPending
	return nil
}

func (cb *CubeBox) GuestMetricsEpochCopy() *GuestMetricsEpoch {
	if cb == nil {
		return nil
	}
	cb.guestMetricsEpochLock.RLock()
	defer cb.guestMetricsEpochLock.RUnlock()
	return cb.GuestMetricsEpoch.DeepCopy()
}

func (cb *CubeBox) RestoreGuestMetricsEpoch(epoch *GuestMetricsEpoch) {
	if cb == nil {
		return
	}
	cb.guestMetricsEpochLock.Lock()
	defer cb.guestMetricsEpochLock.Unlock()
	cb.GuestMetricsEpoch = epoch.DeepCopy()
}

func (cb *CubeBox) RestoreGuestMetricsEpochIfCurrent(
	generation uint64,
	state GuestMetricsEpochState,
	epoch *GuestMetricsEpoch,
) bool {
	if cb == nil {
		return false
	}
	cb.guestMetricsEpochLock.Lock()
	defer cb.guestMetricsEpochLock.Unlock()
	if cb.GuestMetricsEpoch == nil ||
		cb.GuestMetricsEpoch.Generation != generation ||
		cb.GuestMetricsEpoch.State != state {
		return false
	}
	cb.GuestMetricsEpoch = epoch.DeepCopy()
	return true
}

func (cb *CubeBox) ReadyGuestMetricsEpoch(generation uint64, baseline GuestMetricsEpochBaseline, readyAt time.Time) error {
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	if baseline.ContainerID == "" {
		return fmt.Errorf("guest metrics epoch baseline container ID is required")
	}
	if readyAt.IsZero() {
		return fmt.Errorf("guest metrics epoch ready time is required")
	}
	cb.guestMetricsEpochLock.Lock()
	defer cb.guestMetricsEpochLock.Unlock()
	if cb.GuestMetricsEpoch == nil || cb.GuestMetricsEpoch.Generation != generation {
		return fmt.Errorf("guest metrics epoch generation %d is no longer current", generation)
	}
	if cb.GuestMetricsEpoch.State != GuestMetricsEpochPending {
		return fmt.Errorf("guest metrics epoch generation %d is not pending", generation)
	}
	ready := readyAt.UTC()
	cb.GuestMetricsEpoch.ReadyAt = &ready
	cb.GuestMetricsEpoch.Baseline = &baseline
	cb.GuestMetricsEpoch.State = GuestMetricsEpochReady
	return nil
}

func (cb *CubeBox) DegradeGuestMetricsEpoch(generation uint64) error {
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	cb.guestMetricsEpochLock.Lock()
	defer cb.guestMetricsEpochLock.Unlock()
	if cb.GuestMetricsEpoch == nil || cb.GuestMetricsEpoch.Generation != generation {
		return fmt.Errorf("guest metrics epoch generation %d is no longer current", generation)
	}
	if cb.GuestMetricsEpoch.State != GuestMetricsEpochReady {
		return fmt.Errorf("guest metrics epoch generation %d is not ready", generation)
	}
	cb.GuestMetricsEpoch.State = GuestMetricsEpochDegraded
	return nil
}
