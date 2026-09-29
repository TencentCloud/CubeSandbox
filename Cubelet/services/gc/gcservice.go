// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/google/uuid"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/trace"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type GCServicesConfig struct {
	CleanupIntervalStr         string `toml:"cleanup_interval"`
	QuarantineRetryIntervalStr string `toml:"quarantine_retry_interval"`
	// MaxCleanupAttempts bounds how many times a sandbox is retried before it
	// is quarantined. A pointer so that an explicit 0 — the escape hatch that
	// restores unbounded retrying — is distinguishable from "not configured",
	// which takes the default.
	MaxCleanupAttempts *int `toml:"max_cleanup_attempts"`

	cleanupInterval         time.Duration
	quarantineRetryInterval time.Duration
	maxCleanupAttempts      int
}

// defaultMaxCleanupAttempts is deliberately generous. A cleanup round that
// waits on a shim can take tens of seconds, so this is on the order of tens of
// minutes of retrying — long enough that anything transient has resolved, and
// short enough that a structurally stuck sandbox gets reported the same day.
const defaultMaxCleanupAttempts = 60

// defaultQuarantineRetryInterval is how often a quarantined sandbox is still tried.
// Slow enough to stop the noise, frequent enough that killing the holder is
// the only manual step recovery needs.
const defaultQuarantineRetryInterval = 10 * time.Minute
const defaultCleanupInterval = 5 * time.Second

type gcService struct {
	config         *GCServicesConfig
	engine         *workflow.Engine
	gc             *local
	concurrentLock sync.Map
}

func applyGCServiceDefaults(config *GCServicesConfig) {
	t, err := time.ParseDuration(config.CleanupIntervalStr)
	if err != nil || t <= 0 {
		config.cleanupInterval = defaultCleanupInterval
	} else {
		config.cleanupInterval = t
	}

	t, err = time.ParseDuration(config.QuarantineRetryIntervalStr)
	if err != nil || t <= 0 {
		config.quarantineRetryInterval = defaultQuarantineRetryInterval
	} else {
		config.quarantineRetryInterval = t
	}
	if config.quarantineRetryInterval < config.cleanupInterval {
		config.quarantineRetryInterval = config.cleanupInterval
	}

	config.maxCleanupAttempts = defaultMaxCleanupAttempts
	if config.MaxCleanupAttempts != nil && *config.MaxCleanupAttempts >= 0 {
		config.maxCleanupAttempts = *config.MaxCleanupAttempts
	}
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.CubeboxServicePlugin,
		ID:     constants.GCServiceID.ID(),
		Config: &GCServicesConfig{},
		Requires: []plugin.Type{
			constants.InternalPlugin,
			constants.WorkflowPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (_ interface{}, err error) {
			defer func() {
				if err != nil {
					CubeLog.Fatalf("plugin %s init fail:%v", constants.GCServiceID, err.Error())
				}
			}()

			config := ic.Config.(*GCServicesConfig)
			applyGCServiceDefaults(config)
			CubeLog.Infof(
				"gc-service configured: cleanup_interval=%s max_cleanup_attempts=%d quarantine_retry_interval=%s",
				config.cleanupInterval, config.maxCleanupAttempts, config.quarantineRetryInterval,
			)

			p, err := ic.GetByID(constants.WorkflowPlugin, constants.WorkflowID.ID())
			if err != nil {
				return nil, err
			}

			e, ok := p.(*workflow.Engine)
			if !ok {
				return nil, err
			}
			s := &gcService{engine: e, config: config, gc: l}
			// Quarantine survives a restart, and so must the gauge — otherwise
			// bouncing cubelet makes the lost pool capacity look recovered.
			if err := seedQuarantinedGauge(l); err != nil {
				return nil, fmt.Errorf("restore quarantined cleanup state: %w", err)
			}
			go s.run(ic.Context)
			return s, nil
		},
	})
}
func (l *gcService) run(ctx context.Context) {
	cleanUpTicker := time.NewTicker(l.config.cleanupInterval)
	defer cleanUpTicker.Stop()

	rt := &CubeLog.RequestTrace{
		Action: "CleanUp",
		Caller: constants.GCID.ID(),
		Callee: l.engine.ID(),
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanUpTicker.C:
			recov.WithRecover(func() {
				dirtyInfos, err := l.gc.readAll()
				if err != nil {
					cleanupScheduler.WithValues(schedulerReadError).Inc()
					CubeLog.WithContext(ctx).Warnf("read cleanup queue: %v", err)
					return
				}
				if len(dirtyInfos) == 0 {
					return
				}
				for _, info := range dirtyInfos {
					// A quarantined sandbox keeps being retried, just
					// rarely. Hammering it every 5s achieves nothing, but
					// stopping entirely would mean an operator who kills
					// the holder still has to come back and un-quarantine
					// it by hand.
					if !info.dueForRetry(l.config.quarantineRetryInterval) {
						continue
					}

					exist, unlock := l.loadOrStore(info.SandboxID)
					if exist {
						continue
					}
					cleanupScheduler.WithValues(schedulerStarted).Inc()

					recov.GoWithRecover(func() {
						defer unlock()

						opts := &workflow.CleanContext{
							BaseWorkflowInfo: workflow.BaseWorkflowInfo{
								SandboxID: info.SandboxID,
							},
						}
						tmpCtx, cancel := context.WithTimeout(context.Background(), config.GetCommon().CommonTimeout)
						defer cancel()

						defer recov.HandleCrash(func(panicError interface{}) {
							l.onCleanupPanic(tmpCtx, info.SandboxID, panicError)
						})

						tmpCtx = namespaces.WithNamespace(tmpCtx, info.Namespace)
						gcRt := rt.DeepCopy()
						gcRt.CalleeAction = "CleanUp"
						gcRt.RequestID = uuid.New().String()
						gcRt.InstanceID = info.SandboxID

						tmpCtx = CubeLog.WithRequestTrace(tmpCtx, gcRt)
						tmpCtx = log.WithLogger(tmpCtx, log.NewWrapperLogEntry(log.AuditLogger.WithContext(tmpCtx)))
						if err := l.engine.CleanUp(tmpCtx, opts); err != nil {
							gcRt.RetCode = int64(errorcode.ErrorCode_RemoveContainerFailed)
							CubeLog.WithContext(tmpCtx).Fatalf("Cubelet CleanUp fail:%v", err)
							l.recordFailure(tmpCtx, info.SandboxID, err)
						} else {
							cleanupAttempts.WithValues(outcomeSuccess).Inc()
							if info.quarantined() {
								CubeLog.WithContext(tmpCtx).Warnf(
									"sandbox %s cleaned up after quarantine; its host resources are back in the pool",
									info.SandboxID)
							}
						}
						reportTrace(tmpCtx, opts.GetMetric())
					})
				}

				CubeLog.WithContext(ctx).Infof("Cubelet CleanUp")
			})
		}
	}
}

// onCleanupPanic accounts for a cleanup round that died with a panic.
//
// A cleanup that panics on every round is exactly the "hammering forever"
// behaviour the retry bound exists for, so a panic has to count as a failure:
// without this the attempt counter never advances, the sandbox is never
// quarantined, and the one-shot alert an operator needs never fires.
func (l *gcService) onCleanupPanic(ctx context.Context, sandboxID string, panicError interface{}) {
	log.G(ctx).Fatalf("cleanUpTicker panic :%v %v", panicError, string(debug.Stack()))
	l.recordFailure(ctx, sandboxID, fmt.Errorf("cleanup panicked: %v", panicError))
}

// seedQuarantinedGauge republishes the number of quarantined sandboxes from the
// GC store.
//
// It is called both at plugin init, so a restart does not make lost pool
// capacity look recovered, and from local.Init after it has recreated the store
// — InitHost wipes the GC root, so the gauge seeded before that wipe would
// otherwise keep reporting quarantined sandboxes that no longer exist and an
// alert on it would never clear.
func seedQuarantinedGauge(l *local) error {
	quarantinedCount, err := l.countQuarantined()
	if err != nil {
		return err
	}
	quarantinedSandbox.Set(float64(quarantinedCount))
	return nil
}

// retryWithoutPenalty reports whether a cleanup failure is expected to clear on
// its own and must therefore not advance the quarantine budget.
//
// The predicate is structural on purpose: the producer (the cleanup flow's
// "still inside the shim-spawn intent TTL" refusal) lives in another package
// and marks the error with a RetryWithoutPenalty method. Asserting on that
// method rather than on a concrete type keeps the dependency pointing the
// other way — the budget owner does not import the flow it schedules.
func retryWithoutPenalty(err error) bool {
	var marker interface{ RetryWithoutPenalty() bool }
	if !errors.As(err, &marker) {
		return false
	}
	return marker.RetryWithoutPenalty()
}

// recordFailure accounts for one failed cleanup round and, once the retry
// budget is spent, moves the sandbox into quarantine and says so loudly.
func (l *gcService) recordFailure(ctx context.Context, sandboxID string, cause error) {
	// A cleanup can fail for a reason that time alone fixes — a shim-spawn
	// intent that has not yet reached its TTL. Counting that against the retry
	// budget is what makes a ticker faster than the TTL quarantine sandboxes
	// that are not actually stuck: the budget is spent in minutes while the
	// intent only ages out after the (much longer) TTL. Defer instead of
	// penalising, and leave the record untouched so the first genuine failure
	// still starts the clock.
	if retryWithoutPenalty(cause) {
		cleanupAttempts.WithValues(outcomeDeferred).Inc()
		CubeLog.WithContext(ctx).Warnf(
			"sandbox %s cleanup deferred and not counted against the retry budget of %d: %v",
			sandboxID, l.config.maxCleanupAttempts, cause)
		return
	}

	info, justQuarantined, err := l.gc.recordCleanupFailure(sandboxID, l.config.maxCleanupAttempts)
	if err != nil {
		CubeLog.WithContext(ctx).Warnf("record cleanup failure for %s: %v", sandboxID, err)
		return
	}
	if !justQuarantined {
		cleanupAttempts.WithValues(outcomeFail).Inc()
		return
	}

	cleanupAttempts.WithValues(outcomeQuarantined).Inc()

	// This message is the handover to a human, so it has to carry what they
	// need to act: which sandbox, who is still holding it, how long it has
	// been stuck, and — the part that is easy to misread — that quarantine
	// released nothing.
	CubeLog.WithContext(ctx).Fatalf(
		"sandbox %s QUARANTINED after %d failed cleanups over %s (last error: %v); holder: %s. "+
			"Its tap/IP and volumes are STILL HELD and host pool capacity stays reduced until the holder is gone. "+
			"Retries continue every %s but will keep failing on their own: use 'cubecli diag tap-holders' to confirm the holder, "+
			"then 'cubecli diag reclaim' to kill it — cleanup completes by itself afterwards",
		sandboxID, info.Attempts, sinceRounded(info.FirstFailedAt), cause,
		l.describeHolder(ctx, sandboxID), l.config.quarantineRetryInterval)
}

func sinceRounded(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t).Round(time.Second)
}

// describeHolder names the process that is keeping the sandbox alive, for the
// quarantine alert, when the recorded pid is confirmed still running. In
// every other case — no record, no pid, or the recorded pid already gone —
// the recorded pid is not a reliable answer to "who is holding this", so we
// point at the one thing that is: asking the kernel directly.
const describeHolderFallback = "use 'cubecli diag tap-holders' to find the live holder"

func (l *gcService) describeHolder(ctx context.Context, sandboxID string) string {
	cb, err := l.gc.cubeboxManger.Get(ctx, sandboxID)
	if err != nil || cb == nil {
		return describeHolderFallback
	}
	pid := int(cb.Endpoint.Pid)
	if pid <= 1 {
		return describeHolderFallback
	}
	id := utils.ProcessIdentity{Pid: pid, StartTime: cb.Endpoint.PidStartTime}
	if id.Status() == utils.LivenessAlive {
		return fmt.Sprintf("pid %d (%s), still running", pid, utils.ProcessComm(pid))
	}
	// Gone or unknown: the recorded pid is not (or may not be) the real
	// holder — e.g. it exited but something else still holds the fd, or the
	// pid number was recycled. Either way, naming this pid would mislead.
	return describeHolderFallback
}

func (l *gcService) loadOrStore(sandboxID string) (bool, func()) {
	_, exist := l.concurrentLock.LoadOrStore(sandboxID, struct{}{})
	return exist, func() {
		l.concurrentLock.Delete(sandboxID)
	}
}

func reportTrace(ctx context.Context, metrics []*workflow.Metric) {
	action, _ := ctx.Value(CubeLog.KeyAction).(string)
	calleeA, _ := ctx.Value(CubeLog.KeyCalleeAction).(string)
	for _, m := range metrics {
		if m != nil {
			cubelogCode := CubeLog.CodeSuccess
			retCode := errorcode.ErrorCode_Success
			if m.Error() != nil {
				cubelogCode = CubeLog.CodeInternalError
				retCode = errorcode.ErrorCode_Unknown
			}
			trace.Report(ctx, m.ID(), "", action, calleeA, m.Duration(), retCode, cubelogCode)
		}
	}
}
