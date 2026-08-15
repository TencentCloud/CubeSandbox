// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"gorm.io/gorm"
)

// Delivery status values (must match the CubeDB migration).
const (
	StatusPending         = "pending"
	StatusInProgress      = "in_progress"
	StatusSucceeded       = "succeeded"
	StatusFailed          = "failed"
	StatusPermanentFailed = "permanent_failed"
	StatusDead            = "dead"
)

// DeliveryForSend is the payload + subscription snapshot the sender needs.
type DeliveryForSend struct {
	ID             int64
	EventID        string
	Payload        []byte
	SubscriptionID int64
	URL            string
	Secret         string // decrypted plaintext; "" = unsigned
	Attempts       int    // attempts before this send
}

// ErrSecretDecrypt marks a delivery whose subscription secret could not be
// decrypted; the supervisor classifies it as a permanent failure.
var ErrSecretDecrypt = errors.New("webhook secret decrypt failure")

// DeliveryStore owns the delivery-ledger SQL: idempotent materialization,
// claim candidates + atomic claim, conditional completion, lease release,
// backlog accounting and the keep-pending window sweep.
type DeliveryStore struct {
	db *gorm.DB
}

// NewDeliveryStore wraps a GORM connection for delivery ledger operations.
func NewDeliveryStore(db *gorm.DB) *DeliveryStore { return &DeliveryStore{db: db} }

// MaterializeDeliveries inserts pending rows for eventID × subscriptions,
// idempotently (unique (event_id, subscription_id) constraint), in chunks of
// chunkSize with one transaction per chunk. Returns the number of rows
// inserted (duplicates are skipped).
func (d *DeliveryStore) MaterializeDeliveries(ctx context.Context, eventID string, payload []byte, subscriptionIDs []int64, chunkSize int) (int, error) {
	if len(subscriptionIDs) == 0 {
		return 0, nil
	}
	if chunkSize <= 0 {
		chunkSize = 200
	}
	inserted := 0
	for start := 0; start < len(subscriptionIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(subscriptionIDs) {
			end = len(subscriptionIDs)
		}
		n, err := d.materializeChunk(ctx, eventID, payload, subscriptionIDs[start:end])
		if err != nil {
			return inserted, err
		}
		inserted += n
	}
	return inserted, nil
}

func (d *DeliveryStore) materializeChunk(ctx context.Context, eventID string, payload []byte, ids []int64) (int, error) {
	var inserted int
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		for _, sid := range ids {
			// next_retry_at is explicitly written as now() — the claim query
			// filters next_retry_at <= now() and NULL rows would never be
			// picked up (defensive: the column is NOT NULL).
			res := tx.Exec(insertDeliverySQL(), eventID, sid, string(payload), StatusPending, now)
			if res.Error != nil {
				return res.Error
			}
			inserted += int(res.RowsAffected)
		}
		return nil
	})
	return inserted, err
}

func insertDeliverySQL() string {
	if store.IsPostgres() {
		return `INSERT INTO t_webhook_delivery
			(event_id, subscription_id, payload, status, attempts, next_retry_at)
			VALUES (?, ?, ?, ?, 0, ?)
			ON CONFLICT (event_id, subscription_id) DO NOTHING`
	}
	return `INSERT IGNORE INTO t_webhook_delivery
		(event_id, subscription_id, payload, status, attempts, next_retry_at)
		VALUES (?, ?, ?, ?, 0, ?)`
}

// ClaimQuery carries the filters for the two candidate scans. The delivery
// loop (supervisor) pages through with keyset cursors.
type ClaimQuery struct {
	Limit int
	// ExcludeSubscriptions are over-limit subscription ids (Top-N capped).
	ExcludeSubscriptions []int64
	// KeepPendingWindow bounds keep-pending retries; failed rows older than
	// the window are excluded from claiming. 0 = infinite (omit the guard).
	KeepPendingWindow time.Duration
	// Keyset cursors (zero values = start).
	AfterRetryAt    time.Time
	AfterRetryID    int64
	AfterLeaseUntil time.Time
	AfterLeaseID    int64
}

// ClaimCandidates returns up to Limit candidate ids from the two claim
// queries: pending/failed due for retry, and in_progress rows whose lease
// has expired. Over-limit subscriptions and keep-pending window-expired
// failed rows are excluded.
func (d *DeliveryStore) ClaimCandidates(ctx context.Context, q ClaimQuery) ([]int64, error) {
	if q.Limit <= 0 {
		q.Limit = 32
	}
	ids := make([]int64, 0, q.Limit*2)

	// ① 待发/待重试 — (status, next_retry_at) index.
	args := []interface{}{}
	where := `status IN ('pending','failed') AND next_retry_at <= now()`
	if q.KeepPendingWindow > 0 {
		where += ` AND NOT (status='failed' AND first_failed_at IS NOT NULL AND first_failed_at < now() - ` + intervalExpr(q.KeepPendingWindow) + `)`
	}
	if len(q.ExcludeSubscriptions) > 0 {
		where += ` AND subscription_id NOT IN (` + placeholders(len(q.ExcludeSubscriptions)) + `)`
		for _, s := range q.ExcludeSubscriptions {
			args = append(args, s)
		}
	}
	if !q.AfterRetryAt.IsZero() {
		where += ` AND (next_retry_at, id) > (?, ?)`
		args = append(args, q.AfterRetryAt, q.AfterRetryID)
	}
	args = append(args, q.Limit)
	rows, err := d.db.WithContext(ctx).Raw(
		`SELECT id FROM t_webhook_delivery WHERE `+where+` ORDER BY next_retry_at, id LIMIT ?`, args...).Rows()
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ② lease 过期的在途任务 — (status, lease_until) index.
	args2 := []interface{}{}
	where2 := `status = 'in_progress' AND lease_until < now()`
	if len(q.ExcludeSubscriptions) > 0 {
		where2 += ` AND subscription_id NOT IN (` + placeholders(len(q.ExcludeSubscriptions)) + `)`
		for _, s := range q.ExcludeSubscriptions {
			args2 = append(args2, s)
		}
	}
	if !q.AfterLeaseUntil.IsZero() {
		where2 += ` AND (lease_until, id) > (?, ?)`
		args2 = append(args2, q.AfterLeaseUntil, q.AfterLeaseID)
	}
	args2 = append(args2, q.Limit)
	rows2, err := d.db.WithContext(ctx).Raw(
		`SELECT id FROM t_webhook_delivery WHERE `+where2+` ORDER BY lease_until, id LIMIT ?`, args2...).Rows()
	if err != nil {
		return nil, err
	}
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			rows2.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows2.Close()
	if err := rows2.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ClaimCandidatesDue runs claim query ① (pending/failed due for retry) only,
// so the supervisor can page each query with its own keyset cursor.
func (d *DeliveryStore) ClaimCandidatesDue(ctx context.Context, q ClaimQuery) ([]int64, error) {
	if q.Limit <= 0 {
		q.Limit = 32
	}
	where := `status IN ('pending','failed') AND next_retry_at <= now()`
	if q.KeepPendingWindow > 0 {
		where += ` AND NOT (status='failed' AND first_failed_at IS NOT NULL AND first_failed_at < now() - ` + intervalExpr(q.KeepPendingWindow) + `)`
	}
	args := []interface{}{}
	if len(q.ExcludeSubscriptions) > 0 {
		where += ` AND subscription_id NOT IN (` + placeholders(len(q.ExcludeSubscriptions)) + `)`
		for _, s := range q.ExcludeSubscriptions {
			args = append(args, s)
		}
	}
	if !q.AfterRetryAt.IsZero() {
		where += ` AND (next_retry_at, id) > (?, ?)`
		args = append(args, q.AfterRetryAt, q.AfterRetryID)
	}
	args = append(args, q.Limit)
	rows, err := d.db.WithContext(ctx).Raw(
		`SELECT id FROM t_webhook_delivery WHERE `+where+` ORDER BY next_retry_at, id LIMIT ?`, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimCandidatesLease runs claim query ② (expired in_progress leases) only.
func (d *DeliveryStore) ClaimCandidatesLease(ctx context.Context, q ClaimQuery) ([]int64, error) {
	if q.Limit <= 0 {
		q.Limit = 32
	}
	where := `status = 'in_progress' AND lease_until < now()`
	args := []interface{}{}
	if len(q.ExcludeSubscriptions) > 0 {
		where += ` AND subscription_id NOT IN (` + placeholders(len(q.ExcludeSubscriptions)) + `)`
		for _, s := range q.ExcludeSubscriptions {
			args = append(args, s)
		}
	}
	if !q.AfterLeaseUntil.IsZero() {
		where += ` AND (lease_until, id) > (?, ?)`
		args = append(args, q.AfterLeaseUntil, q.AfterLeaseID)
	}
	args = append(args, q.Limit)
	rows, err := d.db.WithContext(ctx).Raw(
		`SELECT id FROM t_webhook_delivery WHERE `+where+` ORDER BY lease_until, id LIMIT ?`, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CursorFor returns the keyset sort values of a candidate row so the claim
// loop can continue paging without offset.
func (d *DeliveryStore) CursorFor(ctx context.Context, id int64) (nextRetryAt time.Time, leaseUntil *time.Time, err error) {
	var row struct {
		NextRetryAt time.Time
		LeaseUntil  *time.Time
	}
	if err := d.db.WithContext(ctx).Raw(
		`SELECT next_retry_at, lease_until FROM t_webhook_delivery WHERE id = ?`, id,
	).Scan(&row).Error; err != nil {
		return time.Time{}, nil, err
	}
	return row.NextRetryAt, row.LeaseUntil, nil
}

// SubscriptionForDelivery returns the subscription_id for a candidate row
// (used for per-subscription admission before claiming).
func (d *DeliveryStore) SubscriptionForDelivery(ctx context.Context, id int64) (int64, error) {
	var sid int64
	if err := d.db.WithContext(ctx).Raw(
		`SELECT subscription_id FROM t_webhook_delivery WHERE id = ?`, id,
	).Scan(&sid).Error; err != nil {
		return 0, err
	}
	return sid, nil
}

// Claim atomically locks one delivery row. Returns true when this worker won
// the lease. The keep-pending guard is applied again as defence-in-depth
// (window=0 omits it).
func (d *DeliveryStore) Claim(ctx context.Context, id int64, owner string, effectiveLease, keepPendingWindow time.Duration) (bool, error) {
	sql := `UPDATE t_webhook_delivery
		SET status='in_progress', lease_owner=?, lease_until=?
		WHERE id=? AND (
			(status IN ('pending','failed') AND next_retry_at <= now()`
	args := []interface{}{owner, time.Now().Add(effectiveLease), id}
	if keepPendingWindow > 0 {
		sql += ` AND NOT (status='failed' AND first_failed_at IS NOT NULL AND first_failed_at < now() - ` + intervalExpr(keepPendingWindow) + `)`
	}
	sql += `) OR (status='in_progress' AND lease_until < now()))`
	res := d.db.WithContext(ctx).Exec(sql, args...)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		leaseContentionTotal.Inc()
		return false, nil
	}
	return true, nil
}

// LoadDeliveryForSend loads the delivery row plus the subscription URL and
// (decrypted) secret for a send. Decrypt failures are surfaced as errors and
// counted; the caller classifies them as permanent.
func (d *DeliveryStore) LoadDeliveryForSend(ctx context.Context, id int64) (*DeliveryForSend, error) {
	var row struct {
		ID             int64
		EventID        string
		Payload        string
		SubscriptionID int64
		Attempts       int
	}
	if err := d.db.WithContext(ctx).Raw(
		`SELECT id, event_id, payload, subscription_id, attempts FROM t_webhook_delivery WHERE id = ?`, id,
	).Scan(&row).Error; err != nil {
		return nil, err
	}
	var sub struct {
		URL              string
		SecretCiphertext *string
	}
	if err := d.db.WithContext(ctx).Raw(
		`SELECT url, secret_ciphertext FROM t_webhook_subscription WHERE id = ? AND deleted_at IS NULL`, row.SubscriptionID,
	).Scan(&sub).Error; err != nil {
		return nil, err
	}
	secret := ""
	if sub.SecretCiphertext != nil {
		plain, err := crypto.DecryptSecret(*sub.SecretCiphertext)
		if err != nil {
			decryptFailureTotal.Inc()
			return nil, fmt.Errorf("%w for subscription %d: %v", ErrSecretDecrypt, row.SubscriptionID, err)
		}
		secret = plain
	}
	return &DeliveryForSend{
		ID:             row.ID,
		EventID:        row.EventID,
		Payload:        []byte(row.Payload),
		SubscriptionID: row.SubscriptionID,
		URL:            sub.URL,
		Secret:         secret,
		Attempts:       row.Attempts,
	}, nil
}

// Completion carries the ledger update for one finished send.
type Completion struct {
	Result      string // ResultSucceeded | ResultRetryable | ResultPermanent
	HTTPStatus  *int
	LastError   *string
	NextRetryAt time.Time // retryable only
	FirstFailed bool      // retryable only: set first_failed_at (COALESCE)
}

// Complete applies the conditional completion update. Returns false when the
// update affected 0 rows (lease lost / task re-claimed); the late result is
// dropped and counted. attempts increments atomically in SQL.
func (d *DeliveryStore) Complete(ctx context.Context, id int64, owner string, c Completion) (bool, error) {
	var sql string
	args := []interface{}{}
	switch c.Result {
	case ResultSucceeded:
		sql = `UPDATE t_webhook_delivery
			SET status='succeeded', http_status=?, last_error=NULL, first_failed_at=NULL,
			    lease_owner=NULL, lease_until=NULL
			WHERE id=? AND lease_owner=? AND status='in_progress'`
		args = append(args, c.HTTPStatus, id, owner)
	case ResultRetryable:
		sql = `UPDATE t_webhook_delivery
			SET status='failed', attempts=attempts+1,
			    first_failed_at=COALESCE(first_failed_at, now()),
			    next_retry_at=?, http_status=?, last_error=?,
			    lease_owner=NULL, lease_until=NULL
			WHERE id=? AND lease_owner=? AND status='in_progress'`
		args = append(args, c.NextRetryAt, c.HTTPStatus, c.LastError, id, owner)
	case ResultPermanent:
		sql = `UPDATE t_webhook_delivery
			SET status='permanent_failed', http_status=?, last_error=?,
			    lease_owner=NULL, lease_until=NULL
			WHERE id=? AND lease_owner=? AND status='in_progress'`
		args = append(args, c.HTTPStatus, c.LastError, id, owner)
	case ResultDead:
		sql = `UPDATE t_webhook_delivery
			SET status='dead', http_status=?, last_error=?,
			    lease_owner=NULL, lease_until=NULL
			WHERE id=? AND lease_owner=? AND status='in_progress'`
		args = append(args, c.HTTPStatus, c.LastError, id, owner)
	default:
		return false, fmt.Errorf("unknown completion result %q", c.Result)
	}
	res := d.db.WithContext(ctx).Exec(sql, args...)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		lateResultDroppedTotal.Inc()
		return false, nil
	}
	return true, nil
}

// IsolateMaterializationFailure marks every still-actionable delivery row of
// a poison entry permanent_failed (best-effort: already-sent rows cannot be
// recalled). Used after the materialization failure threshold is reached.
func (d *DeliveryStore) IsolateMaterializationFailure(ctx context.Context, eventID string) (int64, error) {
	res := d.db.WithContext(ctx).Exec(
		`UPDATE t_webhook_delivery
		 SET status='permanent_failed', last_error='materialization failed beyond threshold', updated_at=now()
		 WHERE event_id=? AND status IN ('pending','failed','in_progress')`,
		eventID,
	)
	return res.RowsAffected, res.Error
}

// ReleaseLease returns a still-owned in_progress row to pending without
// touching attempts (graceful shutdown / abnormal exit path).
func (d *DeliveryStore) ReleaseLease(ctx context.Context, id int64, owner string) error {
	return d.db.WithContext(ctx).Exec(
		`UPDATE t_webhook_delivery SET status='pending', lease_owner=NULL, lease_until=NULL
		 WHERE id=? AND lease_owner=? AND status='in_progress'`,
		id, owner,
	).Error
}

// BacklogCounts returns actionable backlog rows grouped by status
// (pending + retryable failed). Window-expired failed rows are excluded with
// the same predicate as the claim guard.
func (d *DeliveryStore) BacklogCounts(ctx context.Context, keepPendingWindow time.Duration) (map[string]int64, error) {
	where := `status IN ('pending','failed')`
	if keepPendingWindow > 0 {
		where += ` AND NOT (status='failed' AND first_failed_at IS NOT NULL AND first_failed_at < now() - ` + intervalExpr(keepPendingWindow) + `)`
	}
	rows, err := d.db.WithContext(ctx).Raw(
		`SELECT status, COUNT(*) FROM t_webhook_delivery WHERE ` + where + ` GROUP BY status`).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// SubscriptionBacklogs returns actionable backlog per subscription (same
// window predicate), for the soft-limit cache and per-subscription metrics.
func (d *DeliveryStore) SubscriptionBacklogs(ctx context.Context, keepPendingWindow time.Duration) (map[int64]int64, error) {
	where := `status IN ('pending','failed')`
	if keepPendingWindow > 0 {
		where += ` AND NOT (status='failed' AND first_failed_at IS NOT NULL AND first_failed_at < now() - ` + intervalExpr(keepPendingWindow) + `)`
	}
	rows, err := d.db.WithContext(ctx).Raw(
		`SELECT subscription_id, COUNT(*) FROM t_webhook_delivery WHERE ` + where + ` GROUP BY subscription_id`).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var sid int64
		var n int64
		if err := rows.Scan(&sid, &n); err != nil {
			return nil, err
		}
		out[sid] = n
	}
	return out, rows.Err()
}

// SweepKeepPendingWindow converts failed rows past the retry window to dead.
// Called by the cleanup goroutine; window=0 disables the sweep.
func (d *DeliveryStore) SweepKeepPendingWindow(ctx context.Context, keepPendingWindow time.Duration) (int64, error) {
	if keepPendingWindow <= 0 {
		return 0, nil
	}
	res := d.db.WithContext(ctx).Exec(
		`UPDATE t_webhook_delivery
		 SET status='dead', last_error='keep-pending max retry window exceeded', updated_at=now()
		 WHERE status='failed' AND first_failed_at IS NOT NULL
		   AND first_failed_at < now() - ` + intervalExpr(keepPendingWindow),
	)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		keepPendingDeadTotal.Add(float64(res.RowsAffected))
	}
	return res.RowsAffected, nil
}

// RecordMaterializationFailure upserts the poison-entry failure counter and
// returns the new attempts count (persisted, cross-replica).
func (d *DeliveryStore) RecordMaterializationFailure(ctx context.Context, eventID, sandboxID string, subscriptionID *int64, op string, payload []byte, errMsg string) (int, error) {
	payloadStr := ""
	if len(payload) > 0 {
		payloadStr = truncate(string(payload), 65535)
	}
	if store.IsPostgres() {
		var attempts int
		err := d.db.WithContext(ctx).Raw(
			`INSERT INTO t_webhook_materialization_failure
				(event_id, sandbox_id, subscription_id, op, payload, error, attempts, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, 1, now(), now())
			 ON CONFLICT (event_id) DO UPDATE SET
				attempts = t_webhook_materialization_failure.attempts + 1,
				error = EXCLUDED.error,
				updated_at = now()
			 RETURNING attempts`,
			eventID, sandboxID, subscriptionID, op, payloadStr, truncate(errMsg, 4096),
		).Scan(&attempts).Error
		return attempts, err
	}
	if err := d.db.WithContext(ctx).Exec(
		`INSERT INTO t_webhook_materialization_failure
			(event_id, sandbox_id, subscription_id, op, payload, error, attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 1, now(), now())
		 ON DUPLICATE KEY UPDATE attempts = attempts + 1, error = VALUES(error), updated_at = now()`,
		eventID, sandboxID, subscriptionID, op, payloadStr, truncate(errMsg, 4096),
	).Error; err != nil {
		return 0, err
	}
	var attempts int
	if err := d.db.WithContext(ctx).Raw(
		`SELECT attempts FROM t_webhook_materialization_failure WHERE event_id = ?`, eventID,
	).Scan(&attempts).Error; err != nil {
		return 0, err
	}
	return attempts, nil
}

// MaterializationFailureAttempts reads the persisted poison-entry failure
// count (0 when absent). Used by the consumer's self-heal check.
func (d *DeliveryStore) MaterializationFailureAttempts(ctx context.Context, eventID string) (int, error) {
	var attempts int
	err := d.db.WithContext(ctx).Raw(
		`SELECT attempts FROM t_webhook_materialization_failure WHERE event_id = ?`, eventID,
	).Scan(&attempts).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return attempts, err
}

// RetentionCleanup deletes terminal rows past their retention windows in
// batches: succeeded beyond succeededRetention, permanent_failed/dead beyond
// terminalRetention, and materialization failure rows beyond terminalRetention.
// Retryable failed rows are never touched by retention.
func (d *DeliveryStore) RetentionCleanup(ctx context.Context, succeededRetention, terminalRetention time.Duration, batch int) (int64, error) {
	if batch <= 0 {
		batch = 500
	}
	var total int64
	steps := []struct {
		table string
		cond  string
	}{
		{"t_webhook_delivery", "status='succeeded' AND updated_at < now() - " + intervalExpr(succeededRetention)},
		{"t_webhook_delivery", "status IN ('permanent_failed','dead') AND updated_at < now() - " + intervalExpr(terminalRetention)},
		{"t_webhook_materialization_failure", "updated_at < now() - " + intervalExpr(terminalRetention)},
	}
	for _, step := range steps {
		for {
			res := d.db.WithContext(ctx).Exec(retentionDeleteSQL(step.table, step.cond), batch)
			if res.Error != nil {
				return total, res.Error
			}
			total += res.RowsAffected
			if res.RowsAffected < int64(batch) {
				break
			}
		}
	}
	return total, nil
}

func retentionDeleteSQL(table, cond string) string {
	if store.IsPostgres() {
		return `DELETE FROM ` + table + ` WHERE id IN (SELECT id FROM ` + table + ` WHERE ` + cond + ` LIMIT ?)`
	}
	return `DELETE FROM ` + table + ` WHERE ` + cond + ` LIMIT ?`
}

func intervalExpr(d time.Duration) string {
	seconds := int64(d.Seconds())
	if store.IsPostgres() {
		return fmt.Sprintf("interval '%d second'", seconds)
	}
	return fmt.Sprintf("interval %d second", seconds)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
