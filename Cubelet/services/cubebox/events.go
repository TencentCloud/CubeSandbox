// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/google/uuid"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	backOffInitDuration        = 1 * time.Second
	backOffMaxDuration         = 5 * time.Minute
	backOffExpireCheckDuration = 1 * time.Second

	handleEventTimeout = 10 * time.Second
)

type eventMonitor struct {
	c       *local
	ch      <-chan *events.Envelope
	errCh   <-chan error
	ctx     context.Context
	cancel  context.CancelFunc
	backOff *backOff
}

type backOff struct {
	queuePoolMu sync.Mutex

	queuePool map[string]*backOffQueue

	tickerMu      sync.Mutex
	ticker        *time.Ticker
	minDuration   time.Duration
	maxDuration   time.Duration
	checkDuration time.Duration
}

type backOffQueue struct {
	events     []interface{}
	expireTime time.Time
	duration   time.Duration
}

func newEventMonitor(c *local) *eventMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &eventMonitor{
		c:       c,
		ctx:     ctx,
		cancel:  cancel,
		backOff: newBackOff(),
	}
}

func (em *eventMonitor) subscribe(subscriber events.Subscriber) {

	filters := []string{
		`topic=="/tasks/exit"`,
		`topic=="/tasks/oom"`,
		`topic=="/tasks/paused"`,
		`topic=="/tasks/resumed"`,
		`topic~="/images/"`,
	}
	em.ch, em.errCh = subscriber.Subscribe(em.ctx, filters...)
}

func convertEvent(e typeurl.Any) (string, interface{}, error) {
	id := ""
	evt, err := typeurl.UnmarshalAny(e)
	if err != nil {
		return "", nil, fmt.Errorf("failed to unmarshalany: %w", err)
	}

	switch e := evt.(type) {
	case *eventtypes.TaskOOM:
		id = e.ContainerID
	case *eventtypes.SandboxExit:
		id = e.SandboxID
	case *eventtypes.ImageCreate:
		id = e.Name
	case *eventtypes.ImageUpdate:
		id = e.Name
	case *eventtypes.ImageDelete:
		id = e.Name
	case *eventtypes.TaskExit:
		id = e.ContainerID
	case *eventtypes.TaskPaused:
		id = e.ContainerID
	case *eventtypes.TaskResumed:
		id = e.ContainerID
	default:
		return "", nil, errors.New("unsupported event")
	}
	return id, evt, nil
}

type backoffEvent struct {
	evt       interface{}
	Namespace string
}

func (em *eventMonitor) start() <-chan error {
	errCh := make(chan error)
	if em.ch == nil || em.errCh == nil {
		panic(any("event channel is nil"))
	}
	backOffCheckCh := em.backOff.start()
	go func() {
		defer close(errCh)
		defer utils.Recover()
		for {
			select {
			case e := <-em.ch:
				CubeLog.Debugf("Received containerd event timestamp - %v, namespace - %q, topic - %q", e.Timestamp, e.Namespace, e.Topic)
				id, evt, err := convertEvent(e.Event)
				if err != nil {
					CubeLog.Errorf("Failed to convert event %+v: %v", e, err)
					break
				}
				if em.backOff.isInBackOff(id) {
					CubeLog.Infof("Events for %q is in backoff, enqueue event %+v", id, evt)
					em.backOff.enBackOff(id, &backoffEvent{
						evt:       evt,
						Namespace: e.Namespace,
					})
					break
				}
				ctx := namespaces.WithNamespace(context.Background(), e.Namespace)
				if err := em.handleEvent(ctx, evt); err != nil {
					CubeLog.Errorf("Failed to handle event %+v for %s: %v", evt, id, err)
					em.backOff.enBackOff(id, &backoffEvent{
						evt:       evt,
						Namespace: e.Namespace,
					})
				}
			case err := <-em.errCh:

				if err != nil {
					CubeLog.Error("Failed to handle event stream: %v", err)
					errCh <- err
				}
				return
			case <-backOffCheckCh:
				ids := em.backOff.getExpiredIDs()
				for _, id := range ids {
					queue := em.backOff.deBackOff(id)
					for i, any := range queue.events {
						evt := any.(*backoffEvent)
						ctx := namespaces.WithNamespace(context.Background(), evt.Namespace)
						if err := em.handleEvent(ctx, evt.evt); err != nil {
							CubeLog.Errorf("Failed to handle backOff event %+v for %s: %v", evt, id, err)
							em.backOff.reBackOff(id, queue.events[i:], queue.duration)
							break
						}
					}
				}
			}
		}
	}()
	return errCh
}

func (em *eventMonitor) stop() {
	em.backOff.stop()
	em.cancel()
}

func (em *eventMonitor) handleEvent(ctx context.Context, any interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, handleEventTimeout)
	_, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		ctx = namespaces.WithNamespace(ctx, namespaces.Default)
	}
	defer cancel()

	defer utils.Recover()

	var (
		cb   *cubeboxstore.CubeBox
		cntr *cubeboxstore.Container
	)

	switch e := any.(type) {
	case *eventtypes.TaskExit:
		if strings.HasPrefix(e.ID, "exec-") {
			return nil
		}
		CubeLog.Errorf("TaskExit event %+v", e)
		if strings.HasPrefix(e.ID, "exec-") {
			return nil
		}
		cntr, cb, err = em.c.cubeboxManger.FindContainerOfCubebox(ctx, e.ID)
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("failed to find container of cubebox: %w", err)
		}
		if cb == nil {
			return nil
		}
		if cb.Namespace != "" {
			ctx = namespaces.WithNamespace(ctx, cb.Namespace)
		}

		cb.Lock()
		defer cb.Unlock()

		if cb.UserMarkDeletedTime != nil {
			return nil
		}

		rt := &CubeLog.RequestTrace{
			Action:       "handleEvent",
			RequestID:    uuid.New().String(),
			Caller:       "eventMonitor",
			CalleeAction: "handleContainerExit",
			InstanceID:   cntr.SandboxID,
			ContainerID:  e.ID,
		}
		ctx = CubeLog.WithRequestTrace(ctx, rt)
		if err := em.handleContainerExit(ctx, e, cntr); err != nil {
			return fmt.Errorf("failed to handle container TaskExit event: %w", err)
		}
	case *eventtypes.TaskOOM:

		cntr, _, err := em.c.cubeboxManger.FindContainerOfCubebox(ctx, e.ContainerID)
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("failed to find container of cubebox: %w", err)
		}
		if cntr == nil {
			return nil
		}

		CubeLog.WithFields(CubeLog.Fields{"ContainerId": e.ContainerID}).Error("container oom")
		cntr.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
			status.ExitCode = 137
			status.Reason = "OOMKilled"
			status.FinishedAt = time.Now().UnixNano()
			return status, nil
		})
		return em.c.cubeboxManger.SyncByID(ctx, cntr.ID)
	case *eventtypes.TaskPaused:
		if strings.HasPrefix(e.ContainerID, "exec-") {
			return nil
		}
		cntr, cb, err = em.c.cubeboxManger.FindContainerOfCubebox(ctx, e.ContainerID)
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("failed to find container of cubebox: %w", err)
		}
		if cb == nil {
			return nil
		}
		if cb.Namespace != "" {
			ctx = namespaces.WithNamespace(ctx, cb.Namespace)
		}
		cb.Lock()
		defer cb.Unlock()
		// Do not backfill paused semantics once it has entered a terminal state.
		if cb.UserMarkDeletedTime != nil {
			return nil
		}
		if cntr != nil && cntr.Status != nil && cntr.Status.IsTerminated() {
			return nil
		}
		// Idempotent: converge PausedAt/PausingAt of all containers to PAUSED.
		for _, c := range cb.AllContainers() {
			if c.Status == nil {
				continue
			}
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				if status.PausedAt == 0 {
					status.PausedAt = time.Now().UnixNano()
				}
				status.PausingAt = 0
				return status, nil
			})
		}
		return em.c.cubeboxManger.SyncByID(ctx, cb.ID)
	case *eventtypes.TaskResumed:
		if strings.HasPrefix(e.ContainerID, "exec-") {
			return nil
		}
		cntr, cb, err = em.c.cubeboxManger.FindContainerOfCubebox(ctx, e.ContainerID)
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("failed to find container of cubebox: %w", err)
		}
		if cb == nil {
			return nil
		}
		if cb.Namespace != "" {
			ctx = namespaces.WithNamespace(ctx, cb.Namespace)
		}
		cb.Lock()
		defer cb.Unlock()
		if cb.UserMarkDeletedTime != nil {
			return nil
		}
		if cntr != nil && cntr.Status != nil && cntr.Status.IsTerminated() {
			return nil
		}
		// Resume is an opaque restore from CubeShim's internal pause snapshot, so
		// use the shared converger that also invalidates stale runtime snapshot
		// bindings before the next CommitSandbox.
		convergeResumeStateAfterOpaqueRestore(cb, time.Now().UTC())
		return em.c.cubeboxManger.SyncByID(ctx, cb.ID)
	case *eventtypes.ImageCreate:
		log.G(ctx).Infof("image create event: %+v", e)
		return em.c.criImage.UpdateImage(ctx, e.Name)
	case *eventtypes.ImageUpdate:
		log.G(ctx).Infof("image update event: %+v", e)
		return em.c.criImage.UpdateImage(ctx, e.Name)
	case *eventtypes.ImageDelete:
		log.G(ctx).Infof("image delete event: %+v", e)
		return em.c.criImage.UpdateImage(ctx, e.Name)
	default:

		return nil
	}

	return nil
}

func (em *eventMonitor) handleContainerExit(ctx context.Context, e *eventtypes.TaskExit, cntr *cubeboxstore.Container) error {
	if cntr.Status != nil && cntr.Status.Get().Removing {
		return nil
	}
	if cntr.Status != nil && cntr.Status.Get().RollingBack {
		// Snapshot rollback intentionally tears down the OLD VM via shim
		// delete_vm before bringing up the restored VM via
		// resume_vm_with_config. The OLD VM's TaskExit event is delivered
		// to us during/right after that sequence; if we let the default
		// handler run we (a) call task.Delete(WithProcessKill) which can
		// race with the freshly-resumed task slot, and (b) stamp
		// FinishedAt + ExitCode onto the cubebox status, making
		// IsTerminated() return true and breaking a follow-up pause/
		// resume. RollbackSandbox is responsible for resyncing the
		// status; defer to it. See Cubelet/services/cubebox/rollback.go.
		log.G(ctx).Infof("ignoring TaskExit for container %s: rollback in flight", e.ID)
		return nil
	}
	if cntr.Container == nil {
		return nil
	}
	if cntr.DeletedTime != nil {
		return nil
	}
	task, err := cntr.Container.Task(ctx, nil)
	if err != nil {
		if !cubes.IsNotFoundContainerError(err) {
			return fmt.Errorf("failed to load task for container: %v", err)
		}
	} else {
		if _, err = task.Delete(ctx, containerd.WithProcessKill); err != nil {
			if !cubes.IsNotFoundContainerError(err) {
				return fmt.Errorf("failed to stop container: %v", err)
			}
		}
	}

	if e.ExitStatus != 0 {
		log.G(ctx).Errorf("container abnormal exit with code %v", e.ExitStatus)
	}
	pid := uint32(0)
	if cntr.IsPod {
		if utils.ProcessExists(ctx, int(cntr.Status.Get().Pid)) {
			pid = cntr.Status.Status.Pid
			log.G(ctx).Errorf("sandbox %s still exist", e.ID)
		}
	}

	exitTime := e.ExitedAt.AsTime().UnixNano()
	if exitTime <= 0 {
		exitTime = time.Now().UnixNano()
	}
	cntr.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.Pid = pid
		status.FinishedAt = exitTime
		status.ExitCode = int32(e.ExitStatus)
		return status, nil
	})

	if e.ID == e.ContainerID {
		doPostStop(ctx, cntr)
	}
	return em.c.cubeboxManger.SyncByID(ctx, e.ID)
}

func newBackOff() *backOff {
	return &backOff{
		queuePool:     map[string]*backOffQueue{},
		minDuration:   backOffInitDuration,
		maxDuration:   backOffMaxDuration,
		checkDuration: backOffExpireCheckDuration,
	}
}

func (b *backOff) getExpiredIDs() []string {
	b.queuePoolMu.Lock()
	defer b.queuePoolMu.Unlock()

	var ids []string
	for id, q := range b.queuePool {
		if q.isExpire() {
			ids = append(ids, id)
		}
	}
	return ids
}

func (b *backOff) isInBackOff(key string) bool {
	b.queuePoolMu.Lock()
	defer b.queuePoolMu.Unlock()

	if _, ok := b.queuePool[key]; ok {
		return true
	}
	return false
}

func (b *backOff) enBackOff(key string, evt interface{}) {
	b.queuePoolMu.Lock()
	defer b.queuePoolMu.Unlock()

	if queue, ok := b.queuePool[key]; ok {
		queue.events = append(queue.events, evt)
		return
	}
	b.queuePool[key] = newBackOffQueue([]interface{}{evt}, b.minDuration)
}

func (b *backOff) deBackOff(key string) *backOffQueue {
	b.queuePoolMu.Lock()
	defer b.queuePoolMu.Unlock()

	queue := b.queuePool[key]
	delete(b.queuePool, key)
	return queue
}

func (b *backOff) reBackOff(key string, events []interface{}, oldDuration time.Duration) {
	b.queuePoolMu.Lock()
	defer b.queuePoolMu.Unlock()

	duration := 2 * oldDuration
	if duration > b.maxDuration {
		duration = b.maxDuration
	}
	b.queuePool[key] = newBackOffQueue(events, duration)
}

func (b *backOff) start() <-chan time.Time {
	b.tickerMu.Lock()
	defer b.tickerMu.Unlock()
	b.ticker = time.NewTicker(b.checkDuration)
	return b.ticker.C
}

func (b *backOff) stop() {
	b.tickerMu.Lock()
	defer b.tickerMu.Unlock()
	if b.ticker != nil {
		b.ticker.Stop()
	}
}

func newBackOffQueue(events []interface{}, init time.Duration) *backOffQueue {
	return &backOffQueue{
		events:     events,
		duration:   init,
		expireTime: time.Now().Add(init),
	}
}

func (q *backOffQueue) isExpire() bool {

	return !time.Now().Before(q.expireTime)
}

func isTtrpcError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "ttrpc: closed")
}
