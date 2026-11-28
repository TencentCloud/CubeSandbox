// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package rollback coordinates durable, single-owner cleanup tasks. Callers
// supply the resource lock and cleanup operation; scheduling never releases
// resources or forgets a task merely because its retry budget was exhausted.
package rollback

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

type Identity struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
}

type Candidate struct {
	PID      int       `json:"pid"`
	Source   string    `json:"source"`
	Trusted  bool      `json:"trusted"`
	Identity *Identity `json:"identity,omitempty"`
}

type Evidence struct {
	Identities []Identity  `json:"identities,omitempty"`
	Unresolved []Candidate `json:"unresolved,omitempty"`
	// A non-nil zero time is an intent whose age is unknown: it cannot expire.
	SpawnIntent *time.Time `json:"spawn_intent,omitempty"`
}

type Record struct {
	Version   int    `json:"version"`
	ID        string `json:"sandbox_id"`
	Namespace string `json:"namespace"`
	RequestID string `json:"request_id"`
	Preparing bool   `json:"preparing"`
	// Completed is durable before journal removal, so a failed unlink/fsync
	// cannot turn a successful resume into cleanup after a restart.
	Completed bool      `json:"completed,omitempty"`
	Failures  int       `json:"failures"`
	NextRetry time.Time `json:"next_retry,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Evidence  Evidence  `json:"evidence"`
}

type Options struct {
	Context       context.Context
	Interval      time.Duration
	Timeout       time.Duration
	FastAttempts  int
	SlowInterval  time.Duration
	Execute       func(context.Context, *Task) error
	OnPending     func(int)
	OnFailure     func(Record, error)
	OnDriverError func(error)
}

type journalStore interface {
	save(Record) error
	remove(string) error
}

type Coordinator struct {
	mu    sync.Mutex
	tasks map[string]*Task
	store journalStore
	opts  Options
}

type Task struct {
	mu     sync.Mutex
	owner  *Coordinator
	record Record
	done   chan struct{}
	err    error
}

// deferred errors describe scheduling failures, not failed resource cleanup.
type deferred struct{ error }

func (e deferred) Unwrap() error { return e.error }
func Defer(err error) error      { return deferred{err} }

func New(dir string, opts Options) (*Coordinator, error) {
	if opts.Execute == nil {
		return nil, errors.New("rollback executor is required")
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = time.Minute
	}
	if opts.FastAttempts <= 0 {
		opts.FastAttempts = 60
	}
	if opts.SlowInterval <= 0 {
		opts.SlowInterval = 10 * time.Minute
	}
	j, records, err := openJournal(dir)
	if err != nil {
		return nil, err
	}
	c := &Coordinator{tasks: make(map[string]*Task), store: j, opts: opts}
	for _, r := range records {
		if r.Completed {
			// Removal is housekeeping once completion is durable. Never replay
			// resource cleanup, even if the filesystem still refuses removal.
			if err := j.remove(r.ID); err != nil && opts.OnDriverError != nil {
				opts.OnDriverError(fmt.Errorf("remove completed rollback journal %s: %w", r.ID, err))
			}
			continue
		}
		// A crash may have interrupted create before failover was scheduled.
		r.Preparing = false
		if err := j.save(r); err != nil {
			return nil, err
		}
		c.tasks[r.ID] = &Task{owner: c, record: r}
	}
	c.reportPending()
	return c, nil
}

// Prepare must succeed before allocating resources for a same-ID resume.
func (c *Coordinator) Prepare(id, namespace, requestID string) (*Task, error) {
	if id == "" {
		return nil, errors.New("empty rollback sandbox ID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.tasks[id]; exists {
		return nil, fmt.Errorf("rollback for %s is pending", id)
	}
	r := Record{Version: 1, ID: id, Namespace: namespace, RequestID: requestID, Preparing: true}
	if err := c.store.save(r); err != nil {
		return nil, err
	}
	t := &Task{owner: c, record: r}
	c.tasks[id] = t
	c.reportPending()
	return t, nil
}

func (c *Coordinator) Pending(id string) bool { return c.Task(id) != nil }

func (c *Coordinator) Task(id string) *Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tasks[id]
}

func (c *Coordinator) Current(t *Task) bool { return c.Task(t.record.ID) == t }

func (t *Task) Snapshot() Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.record
	r.Evidence = cloneEvidence(r.Evidence)
	return r
}

func cloneEvidence(e Evidence) Evidence {
	e.Identities = slices.Clone(e.Identities)
	e.Unresolved = slices.Clone(e.Unresolved)
	for i := range e.Unresolved {
		if p := e.Unresolved[i].Identity; p != nil {
			v := *p
			e.Unresolved[i].Identity = &v
		}
	}
	if e.SpawnIntent != nil {
		v := *e.SpawnIntent
		e.SpawnIntent = &v
	}
	return e
}

// Remember must succeed BEFORE removing the bundle or any other PID source.
// Known identities survive every attempt; unresolved candidates are replaced
// only with the collector's freshly revalidated verdicts.
func (t *Task) Remember(e Evidence) (Evidence, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range t.record.Evidence.Identities {
		if !slices.Contains(e.Identities, id) {
			e.Identities = append(e.Identities, id)
		}
	}
	t.record.Evidence = cloneEvidence(e)
	err := t.owner.store.save(t.record)
	return cloneEvidence(t.record.Evidence), err
}

// Activate transfers a prepared create to cleanup. Even a failed write keeps
// the in-memory gate; the earlier durable create intent covers restart recovery.
func (c *Coordinator) Activate(id string) error {
	t := c.Task(id)
	if t == nil {
		return fmt.Errorf("no prepared rollback for %s", id)
	}
	t.mu.Lock()
	t.record.Preparing = false
	err := c.store.save(t.record)
	t.mu.Unlock()
	c.start(t, false)
	return err
}

// Complete durably commits success before removing the journal. A failure to
// persist completion leaves the original intent pending; a removal failure is
// non-fatal housekeeping because recovery can now distinguish it from rollback.
// The caller must hold the sandbox lifecycle lock for successful create.
func (c *Coordinator) Complete(t *Task) error {
	c.mu.Lock()
	var removalErr error
	defer func() {
		c.mu.Unlock()
		if removalErr != nil && c.opts.OnDriverError != nil {
			c.opts.OnDriverError(removalErr)
		}
	}()
	id := t.record.ID
	if c.tasks[id] != t {
		return nil
	}
	t.mu.Lock()
	completed := t.record
	completed.Completed = true
	completed.Preparing = false
	if err := c.store.save(completed); err != nil {
		t.mu.Unlock()
		return fmt.Errorf("persist rollback completion: %w", err)
	}
	t.record = completed
	t.mu.Unlock()
	if err := c.store.remove(id); err != nil {
		removalErr = fmt.Errorf("remove completed rollback journal %s: %w", id, err)
	}
	delete(c.tasks, id)
	c.reportPending()
	return nil
}

// Join lets DELETE wait for active cleanup or immediately retry a quarantined
// task. Caller cancellation stops waiting, not the shared cleanup operation.
func (c *Coordinator) Join(ctx context.Context, id string) error {
	t := c.Task(id)
	if t == nil {
		return nil
	}
	done := c.start(t, true)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	current := c.Task(id)
	if current == nil {
		return nil
	}
	if current != t {
		return Defer(fmt.Errorf("cleanup task for %s was replaced", id))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (c *Coordinator) start(t *Task, force bool) <-chan struct{} {
	if t == nil {
		panic("nil rollback task")
	}
	c.mu.Lock()
	if c.tasks[t.record.ID] != t {
		c.mu.Unlock()
		done := make(chan struct{})
		close(done)
		return done
	}
	t.mu.Lock()
	c.mu.Unlock()
	defer t.mu.Unlock()
	if t.done != nil {
		return t.done
	}
	if !force && (t.record.Preparing || time.Now().Before(t.record.NextRetry)) {
		return nil
	}
	done := make(chan struct{})
	t.done = done
	go c.execute(t, done)
	return done
}

func (c *Coordinator) execute(t *Task, done chan struct{}) {
	var err error
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("rollback panic: %v", p)
		}
		if err != nil && c.opts.Context.Err() != nil {
			err = Defer(err)
		}
		var d deferred
		if err != nil && !errors.As(err, &d) {
			t.mu.Lock()
			t.record.Failures++
			t.record.LastError = err.Error()
			if len(t.record.LastError) > 4096 {
				t.record.LastError = t.record.LastError[:4096]
			}
			if t.record.Failures >= c.opts.FastAttempts {
				t.record.NextRetry = time.Now().Add(c.opts.SlowInterval)
			}
			err = errors.Join(err, c.store.save(t.record))
			t.mu.Unlock()
			if c.opts.OnFailure != nil {
				c.opts.OnFailure(t.Snapshot(), err)
			}
		}
		t.mu.Lock()
		t.err = err
		t.done = nil
		close(done)
		t.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(c.opts.Context, c.opts.Timeout)
	defer cancel()
	err = c.opts.Execute(ctx, t)
	if err == nil {
		err = c.Complete(t)
	}
}

func (c *Coordinator) Tick() {
	c.mu.Lock()
	tasks := make([]*Task, 0, len(c.tasks))
	for _, t := range c.tasks {
		tasks = append(tasks, t)
	}
	c.mu.Unlock()
	for _, t := range tasks {
		c.start(t, false)
	}
}

func (c *Coordinator) Run() {
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		// Recover per iteration: one panic cannot terminate the retry driver.
		func() {
			defer func() {
				if p := recover(); p != nil && c.opts.OnDriverError != nil {
					c.opts.OnDriverError(fmt.Errorf("rollback driver panic: %v", p))
				}
			}()
			c.Tick()
		}()
		select {
		case <-c.opts.Context.Done():
			return
		case <-ticker.C:
		}
	}
}

// Called under c.mu, except during construction before the object is published.
func (c *Coordinator) reportPending() {
	if c.opts.OnPending != nil {
		c.opts.OnPending(len(c.tasks))
	}
}
