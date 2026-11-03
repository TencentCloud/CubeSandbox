// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/binding"
	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/s3api"
)

// fakeStore emulates the conditional PUT that makes check-and-register atomic:
// a write whose base etag no longer matches loses with a precondition error,
// exactly as S3 and MinIO report it.
type fakeStore struct {
	mu       sync.Mutex
	data     []byte
	etag     string // empty means the registry object does not exist
	writes   int
	readErr  error
	writeErr error

	// commitThenErr simulates a write the server committed but whose response
	// the client never received: the CAS succeeds, then an error is returned
	// once. Tests use it to reproduce a delayed retry racing a lost response.
	commitThenErr error

	// hasData answers PrefixHasData. Tests about destroy set it; the create
	// path never reaches this fake (it talks to an HTTP stub instead).
	hasData   bool
	prefixErr error

	// gate, when set, holds every read until the expected number of readers
	// has seen the same stale registry. Tests use it to force a real race.
	gate *readGate
}

func newFakeStore() *fakeStore { return &fakeStore{} }

type interceptedClaimsStore struct {
	claimsStore
	write func(context.Context, string, []byte, string) error
}

func (s interceptedClaimsStore) WriteClaims(ctx context.Context, key string, data []byte, etag string) error {
	return s.write(ctx, key, data, etag)
}

func (f *fakeStore) ReadClaims(_ context.Context, _ string) ([]byte, string, error) {
	if f.readErr != nil {
		return nil, "", f.readErr
	}
	read := func() ([]byte, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]byte(nil), f.data...), f.etag
	}
	var data []byte
	var etag string
	if f.gate != nil {
		data, etag = f.gate.do(read)
	} else {
		data, etag = read()
	}
	return data, etag, nil
}

func (f *fakeStore) WriteClaims(_ context.Context, _ string, data []byte, etag string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if etag != f.etag {
		return s3api.ErrPreconditionFailed
	}
	f.data = append([]byte(nil), data...)
	f.writes++
	f.etag = fmt.Sprintf("etag-%d", f.writes)
	if f.commitThenErr != nil {
		err := f.commitThenErr
		f.commitThenErr = nil // one-shot: only the first call loses its response
		return err
	}
	return nil
}

func (f *fakeStore) PrefixHasData(_ context.Context, _ string) (bool, error) {
	return f.hasData, f.prefixErr
}

// readGate releases readers only after all of them have read, so concurrent
// claim calls cannot see each other's write on the first attempt.
type readGate struct {
	n     int
	mu    sync.Mutex
	count int
	ch    chan struct{}
}

func newReadGate(n int) *readGate { return &readGate{n: n, ch: make(chan struct{})} }

func (g *readGate) do(read func() ([]byte, string)) ([]byte, string) {
	g.mu.Lock()
	data, etag := read()
	g.count++
	if g.count == g.n {
		close(g.ch)
	}
	g.mu.Unlock()
	<-g.ch
	return data, etag
}

func seedClaims(t *testing.T, store *fakeStore, claims ...binding.Claim) {
	t.Helper()
	encoded, err := binding.EncodeClaims(binding.Claims{Version: binding.ClaimsVersion, Claims: claims})
	if err != nil {
		t.Fatalf("encode claims: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.data = encoded
	store.writes++
	store.etag = fmt.Sprintf("etag-%d", store.writes)
}

func registryOf(t *testing.T, store *fakeStore) binding.Claims {
	t.Helper()
	store.mu.Lock()
	data := append([]byte(nil), store.data...)
	store.mu.Unlock()
	registry, err := binding.DecodeClaims(data)
	if err != nil {
		t.Fatalf("decode registry: %v", err)
	}
	return registry
}

func TestClaimPrefixRegistersTheBinding(t *testing.T) {
	store := newFakeStore()
	created, err := claimPrefix(context.Background(), store, "user-bucket", "workspaces/user-a/", "vol-1")
	if err != nil {
		t.Fatalf("claimPrefix: %v", err)
	}
	if !created.CreatedThisCall {
		t.Error("created = false, want true for a new claim")
	}
	registry := registryOf(t, store)
	if len(registry.Claims) != 1 {
		t.Fatalf("claims = %+v, want one", registry.Claims)
	}
	if c := registry.Claims[0]; c.VolumeID != "vol-1" || c.Bucket != "user-bucket" || c.Prefix != "workspaces/user-a/" {
		t.Errorf("claim = %+v, want the claimed binding", c)
	}
}

func TestRegistrationRetryBoundAndBorrowedCleanup(t *testing.T) {
	store := newFakeStore()
	attempts := 0
	wrapped := interceptedClaimsStore{claimsStore: store}
	wrapped.write = func(context.Context, string, []byte, string) error { attempts++; return s3api.ErrPreconditionFailed }
	registered, err := claimPrefix(context.Background(), wrapped, "user-bucket", "workspaces/user/", "vol-1")
	if err == nil || !strings.Contains(err.Error(), "contended") || registered.CreatedThisCall || attempts != claimsAttempts {
		t.Fatalf("registration=%+v err=%v attempts=%d, want bounded failure without cleanup authority", registered, err, attempts)
	}
	store.readErr = errors.New("must not read borrowed ownership")
	if err := cancelRegistration(context.Background(), store, registration{Prefix: "workspaces/user/", VolumeID: "vol-1"}); err != nil || store.writes != 0 {
		t.Fatal("canceling borrowed registration touched the backend")
	}
}

func TestClaimPrefixRejectsOverlap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prefix  string
		volume  string
		message string
	}{
		{"same prefix other volume", "workspaces/user-a/", "vol-2", "already assigned"},
		{"nested prefix other volume", "workspaces/user-a/sub/", "vol-2", "already assigned"},
		{"ancestor of existing volume", "workspaces/", "vol-2", "already assigned"},
		{"same volume different prefix", "workspaces/user-a/sub/", "vol-1", "already assigned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "workspaces/user-a/"})

			created, err := claimPrefix(context.Background(), store, "user-bucket", tc.prefix, tc.volume)
			if err == nil {
				t.Fatal("claimPrefix succeeded, want a conflict")
			}
			if created.CreatedThisCall {
				t.Error("created = true on a rejected claim")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error = %q, want it to mention %q", err, tc.message)
			}
		})
	}
}

func TestClaimPrefixIdempotentForSameVolume(t *testing.T) {
	store := newFakeStore()
	seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "workspaces/user-a/"})
	before := store.writes

	created, err := claimPrefix(context.Background(), store, "user-bucket", "workspaces/user-a/", "vol-1")
	if err != nil {
		t.Fatalf("claimPrefix retry: %v", err)
	}
	if created.CreatedThisCall {
		t.Error("created = true, want false when the claim already exists")
	}
	if store.writes != before {
		t.Error("an idempotent claim rewrote the registry")
	}
}

// Two masters that both read an empty registry must not both win. One CAS
// succeeds; the loser re-reads and reports the conflict.
func TestClaimPrefixConcurrentCreatesSerialize(t *testing.T) {
	store := newFakeStore()
	store.gate = newReadGate(2)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, volume := range []string{"vol-1", "vol-2"} {
		go func(volume string) {
			<-start
			_, err := claimPrefix(context.Background(), store, "user-bucket", "workspaces/shared/", volume)
			errs <- err
		}(volume)
	}
	close(start)

	var succeeded, conflicted int
	for i := 0; i < 2; i++ {
		err := <-errs
		switch {
		case err == nil:
			succeeded++
		case strings.Contains(err.Error(), "already assigned"):
			conflicted++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d, want exactly one of each", succeeded, conflicted)
	}
	if claims := registryOf(t, store).Claims; len(claims) != 1 {
		t.Errorf("registry has %d claims, want one: %+v", len(claims), claims)
	}
}

// A damaged registry must fail closed: silently ignoring it would drop every
// other volume's claim on the next rewrite.
func TestClaimPrefixRejectsMalformedRegistry(t *testing.T) {
	store := newFakeStore()
	store.data = []byte("{not json")

	if _, err := claimPrefix(context.Background(), store, "user-bucket", "workspaces/user-a/", "vol-1"); err == nil {
		t.Fatal("claimPrefix succeeded with a malformed registry, want an error")
	}
	if store.writes != 0 {
		t.Error("registry was rewritten despite being unreadable")
	}
}

func TestClaimPrefixSurfacesErrors(t *testing.T) {
	store := newFakeStore()
	store.readErr = fmt.Errorf("access denied")
	if _, err := claimPrefix(context.Background(), store, "user-bucket", "workspaces/user-a/", "vol-1"); err == nil ||
		!strings.Contains(err.Error(), "access denied") {
		t.Fatalf("read error = %v, want the access error", err)
	}

	store = newFakeStore()
	store.writeErr = fmt.Errorf("bucket policy denied")
	if _, err := claimPrefix(context.Background(), store, "user-bucket", "workspaces/user-a/", "vol-1"); err == nil ||
		!strings.Contains(err.Error(), "bucket policy denied") {
		t.Fatalf("write error = %v, want the policy error", err)
	}
	if store.writes != 0 {
		t.Error("registry write counted despite an error")
	}
}

func TestCancelRegistrationRemovesOnlyOwnClaim(t *testing.T) {
	store := newFakeStore()
	seedClaims(t, store,
		binding.Claim{VolumeID: "vol-1", Prefix: "a/"},
		binding.Claim{VolumeID: "vol-2", Prefix: "b/"},
	)
	if err := cancelRegistration(context.Background(), store, registration{Prefix: "a/", VolumeID: "vol-1", CreatedThisCall: true}); err != nil {
		t.Fatalf("cancelRegistration: %v", err)
	}
	claims := registryOf(t, store).Claims
	if len(claims) != 1 || claims[0].VolumeID != "vol-2" {
		t.Errorf("claims = %+v, want only vol-2", claims)
	}

	// Releasing a claim that is already gone is a no-op.
	before := store.writes
	if err := cancelRegistration(context.Background(), store, registration{Prefix: "a/", VolumeID: "vol-1", CreatedThisCall: true}); err != nil {
		t.Fatalf("cancelRegistration retry: %v", err)
	}
	if store.writes != before {
		t.Error("a missing claim triggered a registry rewrite")
	}
}

// A destroy that lost its lease to a takeover must not remove the claim the
// new owner now holds.
func TestFinishDestroyRefusesAfterTakeover(t *testing.T) {
	store := newFakeStore()
	seedClaims(t, store, binding.Claim{
		VolumeID: "vol-1", Prefix: "a/", DeletingAt: "2026-01-01T00:00:00Z", LeaseID: "first-attempt",
	})
	err := finishDestroy(context.Background(), store, deletionLease{Prefix: "a/", VolumeID: "vol-1", ID: "second-attempt"})
	if err == nil || !strings.Contains(err.Error(), "taken over") {
		t.Fatalf("finishDestroy error = %v, want a takeover refusal", err)
	}
	if claims := registryOf(t, store).Claims; len(claims) != 1 {
		t.Errorf("claims = %+v, want the claim left in place", claims)
	}
}

// A competing destroy can take over after the lease expires, fail, and clear
// the lease again. The original deleter may then finish its own delete; with no
// lease left it must release the claim, or the prefix stays blocked forever.
func TestFinishDestroyReleasesAfterCompetingAttemptClearedTheLease(t *testing.T) {
	store := newFakeStore()
	seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "a/"})
	if err := finishDestroy(context.Background(), store, deletionLease{Prefix: "a/", VolumeID: "vol-1", ID: "first-attempt"}); err != nil {
		t.Fatalf("finishDestroy: %v", err)
	}
	if claims := registryOf(t, store).Claims; len(claims) != 0 {
		t.Fatalf("claims = %+v, want the claim released", claims)
	}
}

func TestBeginDestroyTakesTheLease(t *testing.T) {
	store := newFakeStore()
	seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "a/"})

	lease, err := beginDestroy(context.Background(), store, "a/", "vol-1")
	if err != nil {
		t.Fatalf("beginDestroy: %v", err)
	}
	if lease == nil || lease.ID == "" {
		t.Fatalf("lease=%v, want an owned deletion", lease)
	}
	claim := registryOf(t, store).Claims[0]
	if claim.LeaseID != lease.ID {
		t.Errorf("registry lease ID = %q, want %q", claim.LeaseID, lease.ID)
	}
	if _, err := time.Parse(time.RFC3339, claim.DeletingAt); err != nil {
		t.Errorf("registry deleting_at = %q, want an RFC3339 timestamp: %v", claim.DeletingAt, err)
	}

	// A duplicate destroy while the lease is fresh must not delete too.
	if _, err := beginDestroy(context.Background(), store, "a/", "vol-1"); err == nil ||
		!strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second beginDestroy error = %v, want an in-progress refusal", err)
	}
}

// A lease PUT that commits but whose response is lost must not strand the
// prefix behind its own lease: beginDestroy re-reads and recognises the token
// it just generated.
func TestLostLeaseResponseStillYieldsTheLease(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport error", errors.New("connection reset after commit")},
		{"precondition failed after SDK replay", s3api.ErrPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "a/"})
			store.commitThenErr = tc.err

			lease, err := beginDestroy(context.Background(), store, "a/", "vol-1")
			if err != nil {
				t.Fatalf("beginDestroy: %v", err)
			}
			if lease == nil || lease.ID == "" {
				t.Fatalf("lease=%v, want the committed lease", lease)
			}
			if claim := registryOf(t, store).Claims[0]; claim.LeaseID != lease.ID {
				t.Errorf("registry lease ID = %q, want %q", claim.LeaseID, lease.ID)
			}
			// The recovered lease is usable: clearing it must succeed.
			if err := clearDeleteLease(context.Background(), store, *lease); err != nil {
				t.Fatalf("clearDeleteLease: %v", err)
			}
			if claim := registryOf(t, store).Claims[0]; claim.LeaseID != "" {
				t.Errorf("lease = %q, want it cleared", claim.LeaseID)
			}
		})
	}
}

func TestBeginDestroyPreconditionFailureOwnership(t *testing.T) {
	for _, kind := range []string{"unrelated registry update", "different lease", "different volume", "different prefix"} {
		t.Run(kind, func(t *testing.T) {
			store := newFakeStore()
			seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "a/"})
			writes := 0
			wrapped := interceptedClaimsStore{claimsStore: store}
			wrapped.write = func(ctx context.Context, key string, data []byte, etag string) error {
				writes++
				if writes > 1 {
					return store.WriteClaims(ctx, key, data, etag)
				}
				registry, err := binding.DecodeClaims(data)
				if err != nil {
					t.Fatal(err)
				}
				claim := registry.Claims[0]
				switch kind {
				case "unrelated registry update":
					seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "a/"}, binding.Claim{VolumeID: "vol-2", Prefix: "b/"})
				case "different lease":
					claim.LeaseID = "competing-attempt"
					seedClaims(t, store, claim)
				case "different volume":
					claim.VolumeID = "vol-2"
					seedClaims(t, store, claim)
				case "different prefix":
					claim.Prefix = "b/"
					seedClaims(t, store, claim)
					store.hasData = true
				}
				return s3api.ErrPreconditionFailed
			}
			lease, err := beginDestroy(context.Background(), wrapped, "a/", "vol-1")
			if kind == "unrelated registry update" {
				if err != nil || lease == nil || lease.ID == "" || writes != 2 {
					t.Fatalf("lease=%v err=%v writes=%d, want a successful CAS retry", lease, err, writes)
				}
				return
			}
			if err == nil || lease != nil || writes != 1 {
				t.Fatalf("lease=%v err=%v writes=%d, want refusal without another write", lease, err, writes)
			}
		})
	}
}

func TestBeginDestroyTakesOverAStaleLease(t *testing.T) {
	store := newFakeStore()
	seedClaims(t, store, binding.Claim{
		VolumeID: "vol-1", Prefix: "a/", DeletingAt: "2020-01-01T00:00:00Z", LeaseID: "stale-attempt",
	})
	lease, err := beginDestroy(context.Background(), store, "a/", "vol-1")
	if err != nil {
		t.Fatalf("beginDestroy: %v", err)
	}
	if lease == nil || lease.ID == "stale-attempt" || lease.ID == "" {
		t.Fatalf("lease=%v, want a new random lease replacing the stale one", lease)
	}
	if got := registryOf(t, store).Claims[0].LeaseID; got != lease.ID {
		t.Errorf("registry lease ID = %q, want %q", got, lease.ID)
	}
}

func TestBeginDestroyRefusals(t *testing.T) {
	t.Run("foreign volume", func(t *testing.T) {
		store := newFakeStore()
		seedClaims(t, store, binding.Claim{VolumeID: "vol-1", Prefix: "a/"})
		if _, err := beginDestroy(context.Background(), store, "a/", "vol-2"); err == nil ||
			!strings.Contains(err.Error(), "belongs to volume") {
			t.Fatalf("error = %v, want a foreign-owner refusal", err)
		}
	})

	t.Run("nested volume", func(t *testing.T) {
		store := newFakeStore()
		seedClaims(t, store, binding.Claim{VolumeID: "vol-2", Prefix: "a/b/"})
		if _, err := beginDestroy(context.Background(), store, "a/", "vol-1"); err == nil ||
			!strings.Contains(err.Error(), "contains volume") {
			t.Fatalf("error = %v, want a nested-volume refusal", err)
		}
	})

	t.Run("no claim and data present", func(t *testing.T) {
		store := newFakeStore()
		store.hasData = true
		if _, err := beginDestroy(context.Background(), store, "a/", "vol-1"); err == nil ||
			!strings.Contains(err.Error(), "no ownership claim") {
			t.Fatalf("error = %v, want a missing-claim refusal", err)
		}
	})

	t.Run("already deleted", func(t *testing.T) {
		store := newFakeStore()
		lease, err := beginDestroy(context.Background(), store, "a/", "vol-1")
		if err != nil || lease != nil {
			t.Fatalf("lease=%v err=%v, want an idempotent no-op", lease, err)
		}
	})
}

func TestClearDeleteLease(t *testing.T) {
	store := newFakeStore()
	seedClaims(t, store, binding.Claim{
		VolumeID: "vol-1", Prefix: "a/", DeletingAt: "2026-01-01T00:00:00Z", LeaseID: "attempt-1",
	})
	if err := clearDeleteLease(context.Background(), store, deletionLease{Prefix: "a/", VolumeID: "vol-1", ID: "attempt-1"}); err != nil {
		t.Fatalf("clearDeleteLease: %v", err)
	}
	if claim := registryOf(t, store).Claims[0]; claim.DeletingAt != "" || claim.LeaseID != "" {
		t.Errorf("lease = %q/%q, want it cleared", claim.DeletingAt, claim.LeaseID)
	}

	// A lease owned by a later attempt must be left alone.
	seedClaims(t, store, binding.Claim{
		VolumeID: "vol-1", Prefix: "a/", DeletingAt: "2026-01-01T00:00:02Z", LeaseID: "attempt-2",
	})
	if err := clearDeleteLease(context.Background(), store, deletionLease{Prefix: "a/", VolumeID: "vol-1", ID: "attempt-1"}); err != nil {
		t.Fatalf("clearDeleteLease takeover: %v", err)
	}
	claim := registryOf(t, store).Claims[0]
	if claim.DeletingAt != "2026-01-01T00:00:02Z" || claim.LeaseID != "attempt-2" {
		t.Errorf("lease = %q/%q, want the newer lease kept", claim.DeletingAt, claim.LeaseID)
	}
}

// A clear whose response is lost is committed on the server; a second destroy
// may then take a new lease within the same second. When the first clear is
// retried it must match the random lease ID, not the timestamp, so it cannot
// release the second attempt's lease.
func TestLostClearResponseCannotClearANewLease(t *testing.T) {
	fixed := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { return fixed }
	defer func() { now = oldNow }()

	store := newFakeStore()
	// Both attempts record the same second: timestamps cannot tell them apart.
	firstAt := fixed.Format(time.RFC3339)
	seedClaims(t, store, binding.Claim{
		VolumeID: "vol-1", Prefix: "a/", DeletingAt: firstAt, LeaseID: "first-attempt",
	})

	// The first attempt's clear commits server-side, but the client sees an
	// error, so it will retry.
	store.commitThenErr = errors.New("connection reset after commit")
	if err := clearDeleteLease(context.Background(), store, deletionLease{Prefix: "a/", VolumeID: "vol-1", ID: "first-attempt"}); err == nil {
		t.Fatal("clearDeleteLease succeeded, want the injected lost-response error")
	}
	if claim := registryOf(t, store).Claims[0]; claim.DeletingAt != "" || claim.LeaseID != "" {
		t.Fatalf("lease = %q/%q, want the lost clear to have committed", claim.DeletingAt, claim.LeaseID)
	}

	// A second destroy sees the empty lease and takes its own.
	second, err := beginDestroy(context.Background(), store, "a/", "vol-1")
	if err != nil {
		t.Fatalf("second beginDestroy: %v", err)
	}
	if second == nil || second.ID == "first-attempt" {
		t.Fatalf("lease=%v, want a new lease", second)
	}
	claim := registryOf(t, store).Claims[0]
	if claim.LeaseID != second.ID {
		t.Fatalf("registry lease ID = %q, want %q", claim.LeaseID, second.ID)
	}
	if claim.DeletingAt != firstAt {
		t.Fatalf("deleting_at = %q, want %q; the test only proves the fix when both attempts share a second",
			claim.DeletingAt, firstAt)
	}

	// The delayed retry of the first clear must not release the second lease.
	if err := clearDeleteLease(context.Background(), store, deletionLease{Prefix: "a/", VolumeID: "vol-1", ID: "first-attempt"}); err != nil {
		t.Fatalf("delayed clearDeleteLease: %v", err)
	}
	claim = registryOf(t, store).Claims[0]
	if claim.LeaseID != second.ID || claim.DeletingAt != firstAt {
		t.Fatalf("lease = %q/%q, want the second attempt's lease %q kept",
			claim.DeletingAt, claim.LeaseID, second.ID)
	}
}

// A create whose resolved state cannot fit in private_data must fail before it
// writes anything: the prefix may already hold the caller's data, so rolling
// back with a recursive delete would destroy it.
func TestCreateRejectsOversizeStateBeforeTouchingTheBucket(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "volume-s3.conf")
	body := "ACCESS_KEY_ID=ak\nSECRET_ACCESS_KEY=sk\nBUCKET=bucket\nENDPOINT=http://127.0.0.1:1\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_S3_CONFIG", conf)

	// The descriptor fits: 300 literal '<' are ~330 bytes, under the 1024-byte
	// input limit, but the resolved state escapes each one as < and does
	// not fit. The caller's raw JSON is used verbatim, as CubeMaster passes it.
	raw := `{"bucket":"user-bucket","prefix":"` + strings.Repeat("<", 300) + `/"}`
	if len(raw) > binding.MaxPrivateDataLen {
		t.Fatalf("test descriptor is %d bytes, must fit the input limit", len(raw))
	}

	err := doCreate(context.Background(), &options{volumeID: "vol-1", privateData: raw})
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("doCreate error = %v, want the private_data size limit before any S3 call", err)
	}
}

// Reuse the CAS store to test provisioning decisions without reproducing the
// S3 HTTP protocol here. Client HTTP behavior has its own focused tests.
type fakeCreateStore struct {
	*fakeStore
	exists       bool
	createErr    error
	prefixWrites int
}

func (s *fakeCreateStore) BucketExists(context.Context) (bool, error) { return s.exists, nil }
func (s *fakeCreateStore) CreatePrefix(context.Context, string) error {
	s.prefixWrites++
	return s.createErr
}

func TestDynamicCreatePreconditions(t *testing.T) {
	for _, tc := range []struct {
		name            string
		exists, hasData bool
		writeErr        error
		wantError       string
	}{
		{name: "missing bucket", wantError: "does not exist"},
		{name: "existing user data", exists: true, hasData: true, wantError: "already contains objects"},
		{name: "conditional writes unsupported", exists: true, writeErr: s3api.ErrConditionalWritesUnsupported, wantError: "conditional PUT"},
		{name: "empty prefix", exists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCreateStore{fakeStore: newFakeStore(), exists: tc.exists}
			store.hasData, store.writeErr = tc.hasData, tc.writeErr
			err := createDynamicPrefix(context.Background(), store, "user-bucket", "workspaces/user-a/", "vol-1")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || store.prefixWrites != 0 || store.writes != 0 {
					t.Fatalf("err=%v prefixWrites=%d registryWrites=%d, want rejection before provisioning", err, store.prefixWrites, store.writes)
				}
				return
			}
			if err != nil || store.prefixWrites != 1 || !binding.HasClaim(registryOf(t, store.fakeStore).Claims, "workspaces/user-a/", "vol-1") {
				t.Fatalf("err=%v prefixWrites=%d, want one owned prefix", err, store.prefixWrites)
			}
		})
	}
}

func TestMountDirFromMetadata(t *testing.T) {
	if got := mountDirFromMetadata(`{"mount_dir":"/data/x/s3-v1"}`); got != "/data/x/s3-v1" {
		t.Errorf("mountDirFromMetadata = %q", got)
	}
	for _, raw := range []string{"", "not json", `{}`} {
		if got := mountDirFromMetadata(raw); got != "" {
			t.Errorf("mountDirFromMetadata(%q) = %q, want empty", raw, got)
		}
	}
}
