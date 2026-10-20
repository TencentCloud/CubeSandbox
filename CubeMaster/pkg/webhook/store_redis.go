// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

// RedisStoreOptions are explicit construction policy, not configuration loading.
// All masters sharing keys must use the same policy. WriterRetryWindow includes
// exit/backoff budgets and clock-error allowance; P3 must not retry beyond it.
type RedisStoreOptions struct {
	MaxRecords        int
	MaxActive         int
	MaxIdle           int
	OperationTimeout  time.Duration
	TerminalRetention time.Duration
	WriterRetryWindow time.Duration
}

type RedisDeliveryStore struct {
	pool    *redis.Pool
	options RedisStoreOptions
	keys    []interface{}
}

var _ DeliveryStore = (*RedisDeliveryStore)(nil)

func NewRedisDeliveryStore(conf config.RedisConf, o RedisStoreOptions) (*RedisDeliveryStore, error) {
	if o.MaxRecords <= 0 || o.MaxActive <= 0 || o.MaxIdle < 0 || o.MaxIdle > o.MaxActive ||
		o.OperationTimeout <= 0 || o.TerminalRetention < time.Millisecond || o.WriterRetryWindow <= 0 ||
		o.TerminalRetention < o.WriterRetryWindow || o.TerminalRetention%time.Millisecond != 0 ||
		!validMillis(time.Now().Add(o.TerminalRetention)) || uint64(o.MaxRecords) > 9007199254740991 || conf.DbNo < 0 ||
		(conf.MasterName == "" && strings.TrimSpace(conf.Nodes) == "") ||
		(conf.MasterName != "" && strings.TrimSpace(conf.SentinelNodes) == "") {
		return nil, ErrInvalidDelivery
	}
	a, b, c := rediskey.WebhookDelivery()
	s := &RedisDeliveryStore{options: o, keys: []interface{}{a, b, c}}
	s.pool = &redis.Pool{MaxActive: o.MaxActive, MaxIdle: o.MaxIdle, Wait: true,
		IdleTimeout: time.Minute, DialContext: func(ctx context.Context) (redis.Conn, error) {
			c, err := wrapredis.DialContext(ctx, conf)
			if err != nil {
				return nil, err
			}
			return &deliveryConn{Conn: c}, nil
		}}
	return s, nil
}

// Close releases only this pool. Cancel callers and await their bounded calls
// first; Close neither flushes a Writer nor alters durable leases.
func (s *RedisDeliveryStore) Close() error { return s.pool.Close() }

// Mark server-error connections unusable (notably READONLY after failover).
// NOSCRIPT is the sole safe fallback and shares the original context budget.
type deliveryConn struct{ redis.Conn }

func (c *deliveryConn) DoContext(ctx context.Context, cmd string, args ...interface{}) (interface{}, error) {
	v, err := redis.DoContext(c.Conn, ctx, cmd, args...)
	if err != nil && !strings.HasPrefix(err.Error(), "NOSCRIPT ") {
		c.Conn.Close()
	}
	return v, err
}
func (c *deliveryConn) ReceiveContext(ctx context.Context) (interface{}, error) {
	return redis.ReceiveContext(c.Conn, ctx)
}

func (s *RedisDeliveryStore) run(ctx context.Context, args ...interface{}) (interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	c, err := s.pool.GetContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	defer c.Close()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	v, err := deliveryScript.DoContext(ctx, c, append(append([]interface{}{}, s.keys...), args...)...)
	if err == nil {
		return v, nil
	}
	var serverErr redis.Error
	if errors.As(err, &serverErr) {
		if strings.HasPrefix(string(serverErr), "READONLY ") || strings.HasPrefix(string(serverErr), "LOADING ") {
			return nil, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
		}
		switch strings.TrimPrefix(string(serverErr), "ERR ") {
		case "CAPACITY":
			return nil, ErrDeliveryCapacity
		case "CONFLICT":
			return nil, ErrDeliveryConflict
		case "STALE":
			return nil, ErrStaleReceipt
		case "INVALID":
			return nil, ErrInvalidDelivery
		}
		// Lua errors are NOT rollback guarantees. Operators must diagnose data/index
		// consistency on unexpected errors (including OOM), not delete the queue.
		return nil, fmt.Errorf("%w: %w", ErrStoreCorrupt, err)
	}
	return nil, fmt.Errorf("%w: %w", ErrStoreUncertain, err)
}

// URL-safe base64 avoids Lua cjson slash escaping doubling an all-0xff body.
// This is storage encoding only; Claim restores the exact original bytes.
type immutableDelivery struct {
	ID             uuid.UUID
	Type           string
	OccurredAt     time.Time
	Body           string
	SubscriptionID string
	TargetURL      string
	CreatedAt      time.Time
	NextAttemptAt  time.Time
	MaxAttempts    int
}
type deliveryRecord struct {
	Input      string         `json:"input"`
	Max        int            `json:"max"`
	Attempts   int            `json:"attempts"`
	Generation int            `json:"generation"`
	Token      string         `json:"token"`
	State      string         `json:"state"`
	Next       int64          `json:"next"`
	Lease      int64          `json:"lease"`
	Finished   int64          `json:"finished"`
	Failure    FailureSummary `json:"failure"`
	Result     TerminalResult `json:"result"`
}

func validMillis(t time.Time) bool {
	return t.After(time.UnixMilli(0)) && !t.After(time.UnixMilli(maxStoreMillis)) && t.UnixMilli() > 0
}
func validText(s string, max int) bool { return len(s) <= max && utf8.ValidString(s) }
func validSummary(f FailureSummary) bool {
	return validText(f.Code, 64) && validText(f.Message, MaxFailureMessageBytes) && (f.HTTPStatus == 0 || f.HTTPStatus >= 100 && f.HTTPStatus <= 599)
}
func validReceipt(r Receipt) bool {
	return len(r.key) > 0 && len(r.key) <= 36+12+MaxSubscriptionIDBytes && r.generation > 0 && r.generation <= maxDeliveryAttempts && len(r.token) == 36
}

func (s *RedisDeliveryStore) Put(ctx context.Context, t DeliveryTask) error {
	e := t.Event
	if e == nil || e.id == uuid.Nil || !validType(e.eventType) || !validMillis(e.occurredAt) ||
		len(e.body) == 0 || len(e.body) > MaxDeliveryBodyBytes || strings.TrimSpace(t.SubscriptionID) == "" ||
		!validText(t.SubscriptionID, MaxSubscriptionIDBytes) || strings.TrimSpace(t.TargetURL) == "" || !validText(t.TargetURL, MaxDeliveryURLBytes) ||
		!validMillis(t.CreatedAt) || !validMillis(t.NextAttemptAt) || t.MaxAttempts < 1 || t.MaxAttempts > maxDeliveryAttempts {
		return ErrInvalidDelivery
	}
	in := immutableDelivery{e.id, e.eventType, e.occurredAt.UTC(), base64.RawURLEncoding.EncodeToString(e.body), t.SubscriptionID, t.TargetURL, t.CreatedAt.UTC(), t.NextAttemptAt.UTC(), t.MaxAttempts}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > maxImmutableBytes {
		return ErrInvalidDelivery
	}
	key := e.id.String() + ":" + strconv.Itoa(len(t.SubscriptionID)) + ":" + t.SubscriptionID
	_, err = s.run(ctx, "put", key, string(raw), t.MaxAttempts, t.NextAttemptAt.UnixMilli(), s.options.MaxRecords)
	return err
}

func (s *RedisDeliveryStore) Claim(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]ClaimedTask, error) {
	if !validMillis(now) || limit < 1 || limit > MaxDeliveryBatch || lease < time.Millisecond ||
		!validMillis(now.Add(lease)) || !validMillis(now.Add(s.options.TerminalRetention)) {
		return nil, ErrInvalidDelivery
	}
	token, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	values, err := redis.Strings(s.run(ctx, "claim", now.UnixMilli(), limit, lease.Milliseconds(), token.String(), s.options.TerminalRetention.Milliseconds()))
	if err != nil {
		return nil, err
	}
	out := make([]ClaimedTask, 0, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		var r deliveryRecord
		var in immutableDelivery
		if json.Unmarshal([]byte(values[i+1]), &r) != nil || json.Unmarshal([]byte(r.Input), &in) != nil {
			return nil, ErrStoreCorrupt
		}
		body, err := base64.RawURLEncoding.DecodeString(in.Body)
		if err != nil {
			return nil, ErrStoreCorrupt
		}
		e := &Event{id: in.ID, eventType: in.Type, occurredAt: in.OccurredAt, body: body}
		out = append(out, ClaimedTask{Task: DeliveryTask{e, in.SubscriptionID, in.TargetURL, in.CreatedAt, in.NextAttemptAt, in.MaxAttempts}, Attempts: r.Attempts, LeaseUntil: time.UnixMilli(r.Lease).UTC(), Receipt: Receipt{values[i], r.Token, r.Generation}})
	}
	return out, nil
}

func (s *RedisDeliveryStore) Complete(ctx context.Context, r Receipt, result TerminalResult) error {
	if !validReceipt(r) || !validSummary(result.Summary) || (result.State != DeliverySucceeded && result.State != DeliveryFailed && result.State != DeliveryCancelled) {
		return ErrInvalidDelivery
	}
	raw, _ := json.Marshal(result)
	_, err := s.run(ctx, "complete", r.key, r.generation, r.token, time.Now().UnixMilli(), s.options.TerminalRetention.Milliseconds(), string(raw))
	return err
}
func (s *RedisDeliveryStore) Retry(ctx context.Context, r Receipt, next time.Time, f FailureSummary) error {
	if !validReceipt(r) || !validMillis(next) || !validSummary(f) {
		return ErrInvalidDelivery
	}
	raw, _ := json.Marshal(f)
	_, err := s.run(ctx, "retry", r.key, r.generation, r.token, time.Now().UnixMilli(), s.options.TerminalRetention.Milliseconds(), string(raw), next.UnixMilli())
	return err
}
func (s *RedisDeliveryStore) Prune(ctx context.Context, now time.Time, limit int) (int, error) {
	if !validMillis(now) || limit < 1 || limit > MaxDeliveryBatch {
		return 0, ErrInvalidDelivery
	}
	return redis.Int(s.run(ctx, "prune", now.UnixMilli(), limit))
}
