// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrVolumeNotFound means the control plane no longer has the volume, for
// example because it was deleted after the token was issued.
var ErrVolumeNotFound = errors.New("volume not found")

// VolumeRefs registers a request's use of a volume with the control plane, the
// way a sandbox does when it mounts one. Hold must succeed before anything is
// attached: a volume that is deleted cannot be held, and a volume that is held
// cannot be deleted.
//
// A nil error returns a release func that must be called exactly when the
// request is done (it is safe to call more than once). ErrVolumeNotFound means
// the volume does not exist. Any other error means the control plane could not
// say, and callers must fail closed.
type VolumeRefs interface {
	Hold(ctx context.Context, volumeID string) (release func(), err error)
}

const (
	// missingTTL absorbs bursts of requests for a deleted volume.
	missingTTL = 5 * time.Second
	// failedTTL absorbs bursts while the control plane is unreachable, so a
	// stampede does not queue up behind one timeout each.
	failedTTL = 2 * time.Second

	maxTrackedVolumes = 4096
	maxMasterBody     = 64 << 10

	// retCodeNotFound is CubeMaster's errorcode.ErrorCode_NotFound.
	retCodeNotFound = 130404

	releaseRetryMin = time.Second
	releaseRetryMax = 30 * time.Second
)

var volumeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// callResult is the outcome of one reference call to CubeMaster.
type callResult int

const (
	// callApplied: CubeMaster applied the change.
	callApplied callResult = iota
	// callNotFound: the volume does not exist.
	callNotFound
	// callRejected: CubeMaster answered with an error, so the change was not
	// applied. Safe to retry.
	callRejected
	// callUnreachable: the connection was never established, so the change was
	// not applied. Safe to retry.
	callUnreachable
	// callUnknown: the request may or may not have been applied.
	callUnknown
)

// MasterRefs holds a CubeMaster reference (POST/DELETE /cube/volume/{id}/ref)
// for each volume this node is serving, for as long as it has requests in
// flight for it. CubeMaster counts it in the same t_cube_volume.refcount that
// sandboxes use, so DELETE /volumes is refused while a request is running and
// a deleted volume can never be taken.
//
// Concurrent requests for one volume share a single reference: it is taken by
// the first and given back by the last. While the reference is held, new
// requests need no CubeMaster call, so an outage does not disturb volumes that
// are already in use.
//
// The CubeMaster counter is anonymous, so a repeated call would corrupt it, and
// a leaked reference only keeps a volume from being deleted whereas an extra
// release could let a volume that is still in use be deleted. Therefore a call
// whose outcome is unknown is never repeated and never compensated; it is
// logged instead. A release that was definitely not applied is retried.
type MasterRefs struct {
	// Base returns the CubeMaster HTTP base URL. It is a function so it is
	// resolved when needed, after Cubelet's configuration has been loaded.
	Base func() string
	HTTP *http.Client
	Now  func() time.Time
	Logf func(format string, args ...any)
	// RetryDelay returns how long to wait before release retry number attempt
	// (starting at 1). Defaults to exponential backoff from 1s to 30s.
	RetryDelay func(attempt int) time.Duration

	mu     sync.Mutex
	vols   map[string]*volRef
	closed chan struct{}
	once   sync.Once
}

// volRef is the state of one volume. mu serialises the reference calls for it.
type volRef struct {
	mu         sync.Mutex
	n          int  // requests in flight
	registered bool // CubeMaster currently holds our reference
	retrying   bool // a background release retry is running

	missingUntil time.Time
	failedUntil  time.Time
	failedErr    error

	users int // goroutines using this entry; guarded by MasterRefs.mu
}

// NewMasterRefs builds a reference holder with a bounded request timeout.
func NewMasterRefs(base func() string) *MasterRefs {
	return &MasterRefs{Base: base, HTTP: &http.Client{Timeout: 3 * time.Second}, Now: time.Now}
}

// Close stops background release retries. Outstanding references are not
// released: the process is going away and CubeMaster has no holder to expire.
func (r *MasterRefs) Close() {
	r.once.Do(func() {
		r.mu.Lock()
		if r.closed == nil {
			r.closed = make(chan struct{})
		}
		close(r.closed)
		r.mu.Unlock()
	})
}

func (r *MasterRefs) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *MasterRefs) closedCh() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed == nil {
		r.closed = make(chan struct{})
	}
	return r.closed
}

// Hold implements VolumeRefs.
func (r *MasterRefs) Hold(ctx context.Context, volumeID string) (func(), error) {
	if !volumeIDRe.MatchString(volumeID) {
		return nil, ErrVolumeNotFound
	}
	v, err := r.use(volumeID)
	if err != nil {
		return nil, err
	}

	v.mu.Lock()
	if !v.registered {
		now := r.Now()
		switch {
		case now.Before(v.missingUntil):
			v.mu.Unlock()
			r.unuse(volumeID, v)
			return nil, ErrVolumeNotFound
		case now.Before(v.failedUntil):
			err := v.failedErr
			v.mu.Unlock()
			r.unuse(volumeID, v)
			return nil, err
		}
		// A caller going away must not poison the answer other waiters get.
		switch res, err := r.call(context.WithoutCancel(ctx), http.MethodPost, volumeID); res {
		case callApplied:
			v.registered = true
		case callNotFound:
			v.missingUntil = now.Add(missingTTL)
			v.mu.Unlock()
			r.unuse(volumeID, v)
			return nil, ErrVolumeNotFound
		default:
			// Not applied, or unknown. An unknown +1 that did land leaks one
			// reference, which only delays a delete; see the type comment.
			if res == callUnknown {
				r.logf("volume ref: acquire %q: outcome unknown, a reference may leak: %v", volumeID, err)
			}
			v.failedUntil = now.Add(failedTTL)
			v.failedErr = fmt.Errorf("volume reference could not be taken: %w", err)
			ferr := v.failedErr
			v.mu.Unlock()
			r.unuse(volumeID, v)
			return nil, ferr
		}
	}
	v.n++
	v.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { r.release(volumeID, v) }) }, nil
}

func (r *MasterRefs) release(volumeID string, v *volRef) {
	// The caller's use of v passes to the retry goroutine if one is started, so
	// v counts as in use for as long as any goroutine may hold v.mu.
	handedOff := false
	defer func() {
		if !handedOff {
			r.unuse(volumeID, v)
		}
	}()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.n--
	if v.n > 0 || !v.registered {
		return
	}
	r.giveBack(volumeID, v)
	if v.registered && !v.retrying {
		v.retrying = true
		handedOff = true
		go r.retryRelease(volumeID, v)
	}
}

// giveBack makes one release call. v.mu must be held, v.n must be zero.
func (r *MasterRefs) giveBack(volumeID string, v *volRef) {
	res, err := r.call(context.Background(), http.MethodDelete, volumeID)
	switch res {
	case callApplied, callNotFound:
		v.registered = false
	case callRejected, callUnreachable:
		// Definitely not applied: keep the reference and try again later.
		r.logf("volume ref: release %q failed, will retry: %v", volumeID, err)
	default:
		// Unknown: repeating could release a reference that is not ours.
		r.logf("volume ref: release %q: outcome unknown, not retrying; the volume may stay undeletable until its count is repaired: %v", volumeID, err)
		v.registered = false
	}
}

// retryRelease keeps trying to give back a reference that CubeMaster definitely
// did not apply. It owns one use of v (see release).
func (r *MasterRefs) retryRelease(volumeID string, v *volRef) {
	defer r.unuse(volumeID, v)
	done := r.closedCh()
	for attempt := 1; ; attempt++ {
		select {
		case <-done:
			return
		case <-time.After(r.retryDelay(attempt)):
		}
		v.mu.Lock()
		if v.n > 0 || !v.registered {
			// New requests are using the reference, or it is already gone.
			v.retrying = false
			v.mu.Unlock()
			return
		}
		r.giveBack(volumeID, v)
		if !v.registered {
			v.retrying = false
			v.mu.Unlock()
			return
		}
		v.mu.Unlock()
	}
}

func (r *MasterRefs) retryDelay(attempt int) time.Duration {
	if r.RetryDelay != nil {
		return r.RetryDelay(attempt)
	}
	d := releaseRetryMin << min(attempt-1, 6)
	if d > releaseRetryMax {
		d = releaseRetryMax
	}
	return d
}

// use returns the entry for volumeID and counts the caller as a user, so the
// entry cannot be dropped while it is in use.
func (r *MasterRefs) use(volumeID string) (*volRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vols == nil {
		r.vols = make(map[string]*volRef)
	}
	v, ok := r.vols[volumeID]
	if !ok {
		if len(r.vols) >= maxTrackedVolumes {
			r.sweepLocked()
			if len(r.vols) >= maxTrackedVolumes {
				return nil, errors.New("too many volumes in use on this node")
			}
		}
		v = &volRef{}
		r.vols[volumeID] = v
	}
	v.users++
	return v, nil
}

func (r *MasterRefs) unuse(volumeID string, v *volRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v.users--
	r.dropIfIdleLocked(volumeID, v)
}

// idleLocked reports whether nothing about v still needs to be remembered.
// r.mu must be held.
func (r *MasterRefs) idleLocked(v *volRef, now time.Time) bool {
	if v.users > 0 {
		return false
	}
	// Every goroutine that takes v.mu counts as a user, so with none left this
	// lock is uncontended; it only publishes the fields safely.
	v.mu.Lock()
	defer v.mu.Unlock()
	return !v.registered && !v.retrying && v.n == 0 &&
		!now.Before(v.missingUntil) && !now.Before(v.failedUntil)
}

func (r *MasterRefs) dropIfIdleLocked(volumeID string, v *volRef) {
	if r.idleLocked(v, r.Now()) {
		delete(r.vols, volumeID)
	}
}

func (r *MasterRefs) sweepLocked() {
	now := r.Now()
	for id, v := range r.vols {
		if r.idleLocked(v, now) {
			delete(r.vols, id)
		}
	}
}

// call makes one reference call to CubeMaster and classifies the outcome.
func (r *MasterRefs) call(ctx context.Context, method, volumeID string) (callResult, error) {
	base := strings.TrimRight(strings.TrimSpace(r.Base()), "/")
	if base == "" {
		return callUnreachable, errors.New("cubemaster address is not configured")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/cube/volume/"+url.PathEscape(volumeID)+"/ref", nil)
	if err != nil {
		return callUnreachable, err
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return callUnreachable, err
		}
		return callUnknown, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMasterBody))
	if err != nil {
		return callUnknown, err
	}
	if resp.StatusCode != http.StatusOK {
		// CubeMaster answers handled errors with 200 and a ret code, so any
		// other status came from somewhere that may have applied the change.
		return callUnknown, fmt.Errorf("cubemaster volume ref: http %d", resp.StatusCode)
	}
	var out struct {
		Ret struct {
			RetCode int    `json:"ret_code"`
			RetMsg  string `json:"ret_msg"`
		} `json:"ret"`
		VolumeID string `json:"volumeID"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return callUnknown, fmt.Errorf("cubemaster volume ref: decode: %w", err)
	}
	switch {
	case out.Ret.RetCode == retCodeNotFound:
		return callNotFound, nil
	case out.Ret.RetCode != 0:
		return callRejected, fmt.Errorf("cubemaster volume ref: ret_code %d: %s", out.Ret.RetCode, out.Ret.RetMsg)
	case out.VolumeID != volumeID:
		return callUnknown, errors.New("cubemaster volume ref: unexpected volume in response")
	}
	return callApplied, nil
}
