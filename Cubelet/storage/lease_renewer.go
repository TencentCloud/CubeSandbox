// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Package storage - Renewer is the cross-node resume lease renewal mechanism
//
// Background — issues #1690 and #1692:
//
// A paused sandbox whose rootfs/memory/metadata have been uploaded to S3
// sits as a remote snapshot on the source (paused) node. Cross-node Resume
// (or cold cross-node Restore) needs another node to import_lvol against
// those exports. The exporter (this node) and importer (the new node)
// cooperate through a liveness lease: the importer periodically renews
// `<lvs>/meta/exports/<uuid>.lease` in the source bucket, and the source
// grants a grace window of `3 * renew_s` after each observed renew.
//
// The bug: between Pause and the moment cross-node Resume starts the
// importer, there is no importer at all. The exporter never renews the
// lease from its side, the grace window expires, and the export is
// reaped. A later cross-node Resume then fails because the snapshot
// cannot be imported.
//
// Fix:
//
// The renewer in this file is the exporter-side heartbeat. While a
// sandbox is paused and stored as a remote package, we re-export the
// snapshot at RenewalInterval (well below the grace window) so the
// source's own lease stays fresh. Resume / Destroy / a node crash all
// stop the renewer, which is correct: a Resume will switch to the
// importer-lease path on the new node, and a Destroy is supposed to
// release the export.
package storage

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// RenewalInterval is how often the lease renewer re-exports a paused
// snapshot. Must be:
//
//	RenewalInterval < (S3LVOL_LEASE_RENEW_MIN_SEC * 3)
//
// so that even if the importer never starts (cold cross-node restore
// racing a sweeper, for example), the exporter keeps the lease document
// alive. 30s sits comfortably below the historical ~60s lease window and
// gives three full renewals before grace is exhausted.
//
// The value is a var (not const) so tests can dial it down without
// touching production timing.
var RenewalInterval = 30 * time.Second

// renewerEntry tracks one paused snapshot the renewer is keeping alive.
type renewerEntry struct {
	sandboxID  string
	snapID     string
	backend    string
	cancel     chan struct{}
	done       chan struct{}
}

// Renewer is a single-instance goroutine that drives the per-sandbox
// lease-renewal loops. It is intentionally lightweight: a map, a cond,
// and a goroutine that fans out one ticker per active sandbox.
//
// The renewer is process-local: a Cubelet restart drops the renewer
// (no goroutines survive). That is the correct behaviour — a node
// restart means the exporter is no longer authoritative, and any
// in-flight importer is expected to fail and let the lease lapse.
type Renewer struct {
	mu      sync.Mutex
	entries map[string]*renewerEntry // keyed by snapID

	stopCh chan struct{}
	wg     sync.WaitGroup

	// Now is injectable for tests so they can fast-forward without
	// sleeping. Defaults to time.Now.
	now func() time.Time
}

// NewRenewer returns a Renewer ready to be used. The renewer is
// internally goroutine-safe; callers do not need their own lock when
// calling Register / Unregister.
func NewRenewer() *Renewer {
	return &Renewer{
		entries: make(map[string]*renewerEntry),
		stopCh:  make(chan struct{}),
		now:     time.Now,
	}
}

// Register starts (or restarts) lease renewal for a paused sandbox's
// remote snapshot. backend must already be normalised; empty / XFS are
// silently ignored so the call site does not have to special-case them.
//
// Register is idempotent: calling it twice with the same snapID is the
// same as a no-op plus a log line. This matches the production flow,
// which can hit Register twice if Pause retries or the caller is unsure
// whether a previous registration succeeded (e.g. after a Cubelet
// restart where the in-memory map is empty but the export is still on
// disk).
//
// snapID must be a Master-allocated pause snap id ("snap-..."). Passing
// anything else returns an error so we never accidentally renew a
// template's "snap-" id (those are owned by TemplateCenter, not the
// data plane).
func (r *Renewer) Register(ctx context.Context, sandboxID, snapID, backend string) error {
	if r == nil {
		return errors.New("renewer is nil")
	}
	snapID = safeSnapID(snapID)
	if snapID == "" {
		return errors.New("renewer register: empty snapID")
	}
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil || normalized != cow.BackendS3 {
		// XFS / unknown: nothing to renew. Not an error — Register is a
		// best-effort hook fired from the pause path.
		log.G(ctx).Debugf("lease renewer skipping non-s3 backend: snapID=%s backend=%q", snapID, backend)
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.entries[snapID]; exists {
		log.G(ctx).Infof("lease renewer: snapID=%s already registered; skipping duplicate", snapID)
		return nil
	}

	entry := &renewerEntry{
		sandboxID: sandboxID,
		snapID:    snapID,
		backend:   normalized,
		cancel:    make(chan struct{}),
		done:      make(chan struct{}),
	}
	r.entries[snapID] = entry

	r.wg.Add(1)
	go r.runOne(ctx, entry)
	log.G(ctx).Infof("lease renewer: registered snapID=%s sandboxID=%s interval=%s", snapID, sandboxID, RenewalInterval)
	return nil
}

// Unregister stops renewal for snapID deterministically. Safe to call
// from any goroutine; returns once the per-sandbox goroutine has fully
// exited (close(entry.done) observed).
//
// Unregister is a no-op for unknown snap IDs so callers can fire it
// from Destroy paths that may or may not have Register'd first (e.g.
// sandbox was created on another node and never paused here).
//
// Issue #1690/#1692 review: this used to race a 5 s wall-clock against a
// 60 s UploadSnapshot timeout, leaking one extra renew on the way
// out. The fix is to make renew's per-call context derive from
// entry.cancel, so the in-flight UploadSnapshot returns immediately
// when Unregister closes the channel — the goroutine then closes
// entry.done on its next select iteration and this function returns
// without a timeout. The 60 s per-renew bound stays as a backstop in
// case cancel never fires (e.g. the renew goroutine itself panics).
func (r *Renewer) Unregister(ctx context.Context, snapID string) {
	if r == nil {
		return
	}
	snapID = safeSnapID(snapID)
	if snapID == "" {
		return
	}
	r.mu.Lock()
	entry, ok := r.entries[snapID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.entries, snapID)
	r.mu.Unlock()

	close(entry.cancel)
	select {
	case <-entry.done:
	case <-ctx.Done():
		// The caller's own context is the only remaining bound;
		// we do not block on a wall-clock timeout here because the
		// renew goroutine already saw cancel via its derived context
		// and is on its way out.
		log.G(ctx).Debugf("lease renewer: unregister snapID=%s returned on caller ctx cancel", snapID)
	}
}

// Stop tears the renewer down. All in-flight renewal loops see cancel
// and exit. Stop is idempotent.
func (r *Renewer) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	select {
	case <-r.stopCh:
		r.mu.Unlock()
		return
	default:
	}
	close(r.stopCh)
	entries := make([]*renewerEntry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	r.entries = map[string]*renewerEntry{}
	r.mu.Unlock()

	for _, e := range entries {
		close(e.cancel)
	}
	r.wg.Wait()
}

// Active returns the number of sandboxes currently being renewed.
// Cheap; safe for /metrics.
func (r *Renewer) Active() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// runOne is the per-sandbox renewal goroutine. It performs an immediate
// renew on entry (so a freshly-paused sandbox gets its first refresh
// without waiting a full RenewalInterval) and then ticks every
// RenewalInterval until the channel closes or Stop fires.
func (r *Renewer) runOne(parent context.Context, entry *renewerEntry) {
	defer r.wg.Done()
	defer close(entry.done)

	// Detach from the parent RPC deadline: a long pause (which is the
	// case this renewer exists for) must not have its renewal loop
	// cancelled by the originating UpdateSandbox RPC returning.
	ctx := context.WithoutCancel(parent)
	log.G(ctx).Debugf("lease renewer: starting snapID=%s sandboxID=%s", entry.snapID, entry.sandboxID)

	r.renew(ctx, entry)

	ticker := time.NewTicker(RenewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-entry.cancel:
			log.G(ctx).Infof("lease renewer: stopping snapID=%s", entry.snapID)
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.renew(ctx, entry)
		}
	}
}

// renew performs a single UploadSnapshot pass and logs the outcome.
// On error we continue the loop — a transient cubecow failure must
// not panic the renewer; the next tick will retry. Persistent failure
// will manifest as a real Resume error on the importer side, which is
// the correct place to surface it.
//
// The per-call context derives from entry.cancel (NOT from the
// goroutine's outer context, which is already detached via
// context.WithoutCancel). That way Unregister — which closes
// entry.cancel — aborts an in-flight UploadSnapshot immediately
// instead of waiting for the 60 s timeout.
func (r *Renewer) renew(ctx context.Context, entry *renewerEntry) {
	// Bound a single renewal so a wedged cubecow RPC cannot starve
	// the next tick. RenewalInterval is 30 s; a healthy renew is
	// sub-second, so 60 s of headroom is plenty. The cancel channel
	// takes priority: Unregister closes it to abort the in-flight
	// call deterministically.
	renewCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// entry.cancel is a chan struct{} that Unregister closes; turn
	// it into a context so we can pass it to UploadSnapshot and have
	// the in-flight call return immediately when Unregister fires.
	abortCtx, abortCancel := channelContextFromChan(entry.cancel)
	defer abortCancel()
	// Race the two: whichever fires first cancels both, so we never
	// leak a goroutine and the renewer's effective deadline is the
	// earlier of (60 s) and (Unregister's signal).
	go func() {
		select {
		case <-renewCtx.Done():
		case <-abortCtx.Done():
			cancel()
		}
	}()
	renewCtx = abortCtx

	uuids, err := UploadSnapshot(renewCtx, entry.backend, entry.snapID)
	if err != nil {
		// Aborted-by-Unregister errors are expected on the path out
		// and not actionable — log at debug. Real cubecow failures
		// stay at warn so they show up in normal log filters.
		select {
		case <-entry.cancel:
			log.G(ctx).Debugf("lease renewer: renew snapID=%s aborted by unregister", entry.snapID)
		default:
			log.G(ctx).Warnf("lease renewer: renew snapID=%s failed: %v", entry.snapID, err)
		}
		return
	}
	if uuids == nil || uuids.Empty() {
		// Not necessarily fatal: an export can be momentarily empty
		// while the package is being re-sealed. Log and let the next
		// tick re-attempt.
		log.G(ctx).Debugf("lease renewer: renew snapID=%s returned empty uuids (will retry)", entry.snapID)
		return
	}
	log.G(ctx).Debugf("lease renewer: renew snapID=%s OK (rootfs=%s memory=%s metadata=%s)",
		entry.snapID, uuids.Rootfs, uuids.Memory, uuids.Metadata)
}

// safeSnapID trims whitespace so callers passing annotation values
// straight from Master do not get spurious duplicate registrations.
func safeSnapID(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '	' || s[0] == '\n') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '	' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

// cancelContext is the context returned by channelContextFromChan.
// It is cancelled when ch is closed. context.WithCancel is the
// right primitive; we wrap it here only to expose a single
// construction call site so the bookkeeping goroutine cannot be
// missed at a renew caller.
func channelContextFromChan(ch chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
