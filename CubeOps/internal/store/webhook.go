// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrWebhookSubscriptionNotFound is returned when a webhook subscription
// does not exist (or has been hard-removed; soft-deleted rows are still
// returned by GetWebhookSubscription so operators can distinguish a
// disabled subscription from a deleted one via deleted_at).
var ErrWebhookSubscriptionNotFound = errors.New("webhook subscription not found")

// WebhookSubscription mirrors t_webhook_subscription.
type WebhookSubscription struct {
	ID               int64                      `gorm:"column:id;primaryKey" json:"id"`
	Name             string                     `gorm:"column:name" json:"name"`
	URL              string                     `gorm:"column:url" json:"url"`
	Enabled          bool                       `gorm:"column:enabled" json:"enabled"`
	DeletedAt        *time.Time                 `gorm:"column:deleted_at" json:"deleted_at,omitempty"`
	SecretCiphertext *string                    `gorm:"column:secret_ciphertext" json:"-"`
	CreatedAt        time.Time                  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt        time.Time                  `gorm:"column:updated_at" json:"updated_at"`
	Events           []WebhookSubscriptionEvent `gorm:"foreignKey:SubscriptionID" json:"events"`
}

// TableName implements gorm.Tabler.
func (WebhookSubscription) TableName() string { return "t_webhook_subscription" }

// WebhookSubscriptionEvent mirrors t_webhook_subscription_event.
type WebhookSubscriptionEvent struct {
	ID             int64  `gorm:"column:id;primaryKey" json:"id"`
	SubscriptionID int64  `gorm:"column:subscription_id" json:"subscription_id"`
	EventType      string `gorm:"column:event_type" json:"event_type"`
}

// TableName implements gorm.Tabler.
func (WebhookSubscriptionEvent) TableName() string { return "t_webhook_subscription_event" }

// WebhookDelivery mirrors t_webhook_delivery. The read side is exercised by
// the deliveries query endpoint; the claim/update SQL lives in the webhook
// worker package (internal/webhook/delivery.go).
type WebhookDelivery struct {
	ID             int64      `gorm:"column:id;primaryKey" json:"id"`
	EventID        string     `gorm:"column:event_id" json:"event_id"`
	SubscriptionID int64      `gorm:"column:subscription_id" json:"subscription_id"`
	Payload        string     `gorm:"column:payload" json:"-"`
	Status         string     `gorm:"column:status" json:"status"`
	Attempts       int        `gorm:"column:attempts" json:"attempts"`
	NextRetryAt    time.Time  `gorm:"column:next_retry_at" json:"next_retry_at"`
	FirstFailedAt  *time.Time `gorm:"column:first_failed_at" json:"first_failed_at,omitempty"`
	LeaseUntil     *time.Time `gorm:"column:lease_until" json:"lease_until,omitempty"`
	LeaseOwner     *string    `gorm:"column:lease_owner" json:"-"`
	HTTPStatus     *int       `gorm:"column:http_status" json:"http_status,omitempty"`
	LastError      *string    `gorm:"column:last_error" json:"last_error,omitempty"`
	CreatedAt      time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// TableName implements gorm.Tabler.
func (WebhookDelivery) TableName() string { return "t_webhook_delivery" }

// CreateWebhookSubscription inserts the subscription and its event allowlist
// in one transaction. The caller must populate Events with SubscriptionID 0;
// the store assigns the generated id.
func (s *Store) CreateWebhookSubscription(ctx context.Context, sub *WebhookSubscription) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Omit("Events") prevents GORM's association auto-save from inserting
		// the allowlist twice (Create saves associations by default); we insert
		// the rows explicitly below after the FK is known.
		if err := tx.Omit("Events").Create(sub).Error; err != nil {
			return err
		}
		for i := range sub.Events {
			sub.Events[i].SubscriptionID = sub.ID
		}
		if len(sub.Events) > 0 {
			if err := tx.Create(&sub.Events).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// GetWebhookSubscription returns a subscription by id, including soft-deleted
// rows (deleted_at non-NULL) so callers can distinguish "disabled" from
// "deleted" and operators can still inspect historical records.
func (s *Store) GetWebhookSubscription(ctx context.Context, id int64) (*WebhookSubscription, error) {
	var sub WebhookSubscription
	if err := s.db.WithContext(ctx).
		Preload("Events").
		First(&sub, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWebhookSubscriptionNotFound
		}
		return nil, err
	}
	return &sub, nil
}

// ListWebhookSubscriptions returns a page of non-deleted subscriptions,
// ordered by id DESC. limit/offset follow the store pagination convention
// (DefaultListLimit / MaxListLimit).
func (s *Store) ListWebhookSubscriptions(ctx context.Context, limit, offset int) ([]WebhookSubscription, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if offset < 0 {
		offset = 0
	}
	var subs []WebhookSubscription
	err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Order("id DESC").
		Limit(limit).
		Offset(offset).
		Preload("Events").
		Find(&subs).Error
	return subs, err
}

// UpdateWebhookSubscription updates the subscription row and atomically
// replaces its event allowlist in one transaction. The caller passes the
// full desired state (secret_ciphertext included — nil clears the secret).
func (s *Store) UpdateWebhookSubscription(ctx context.Context, sub *WebhookSubscription) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&WebhookSubscription{}).
			Where("id = ? AND deleted_at IS NULL", sub.ID).
			Updates(map[string]interface{}{
				"name":              sub.Name,
				"url":               sub.URL,
				"enabled":           sub.Enabled,
				"secret_ciphertext": sub.SecretCiphertext,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrWebhookSubscriptionNotFound
		}
		if err := tx.Where("subscription_id = ?", sub.ID).Delete(&WebhookSubscriptionEvent{}).Error; err != nil {
			return err
		}
		for i := range sub.Events {
			sub.Events[i].ID = 0
			sub.Events[i].SubscriptionID = sub.ID
		}
		if len(sub.Events) > 0 {
			if err := tx.Create(&sub.Events).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// SoftDeleteWebhookSubscription marks the subscription deleted: enabled=false,
// deleted_at=now(), and name renamed to name[:96]+"#del#"+id so the UNIQUE(name)
// constraint is released and the same name can be re-created (as a new id).
func (s *Store) SoftDeleteWebhookSubscription(ctx context.Context, id int64) error {
	var sub WebhookSubscription
	if err := s.db.WithContext(ctx).
		Select("id", "name").
		First(&sub, "id = ? AND deleted_at IS NULL", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrWebhookSubscriptionNotFound
		}
		return err
	}
	now := time.Now()
	// Rename, disable and mark deleted in ONE atomic UPDATE: either the whole
	// transition lands or nothing changes. The renamed value embeds the row
	// id, so it cannot collide with another row's renamed name; a collision
	// with a user-created literal name would fail the statement atomically
	// (no partial soft-delete), and the caller surfaces it as an error.
	res := s.db.WithContext(ctx).Model(&WebhookSubscription{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]interface{}{
			"enabled":    false,
			"deleted_at": &now,
			"name":       renameDeletedSubscription(sub.Name, id),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrWebhookSubscriptionNotFound
	}
	return nil
}

// renameDeletedSubscription builds the soft-delete name that releases the
// UNIQUE(name) slot while staying within the 128-byte column limit.
func renameDeletedSubscription(name string, id int64) string {
	const maxLen = 128
	suffix := fmt.Sprintf("#del#%d", id)
	base := name
	if len(base) > maxLen-len(suffix) {
		base = base[:maxLen-len(suffix)]
	}
	return base + suffix
}

// ListWebhookSubscriptionsByEventType returns enabled, non-deleted
// subscriptions whose allowlist contains eventType. This is the fan-out
// lookup used by the delivery consumer.
func (s *Store) ListWebhookSubscriptionsByEventType(ctx context.Context, eventType string) ([]WebhookSubscription, error) {
	var subs []WebhookSubscription
	err := s.db.WithContext(ctx).
		Table("t_webhook_subscription").
		Joins("JOIN t_webhook_subscription_event e ON e.subscription_id = t_webhook_subscription.id").
		Where("t_webhook_subscription.deleted_at IS NULL AND t_webhook_subscription.enabled = ? AND e.event_type = ?",
			true, eventType).
		Find(&subs).Error
	return subs, err
}

// CreateWebhookDelivery inserts a delivery row (used by the test endpoint
// and, later, by the materialization path).
func (s *Store) CreateWebhookDelivery(ctx context.Context, d *WebhookDelivery) error {
	return s.db.WithContext(ctx).Create(d).Error
}

// ListWebhookDeliveries returns a page of delivery rows for a subscription,
// optionally filtered by status and event_id prefix, ordered by id DESC.
func (s *Store) ListWebhookDeliveries(ctx context.Context, subscriptionID int64, status, eventIDPrefix string, limit, offset int) ([]WebhookDelivery, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if offset < 0 {
		offset = 0
	}
	q := s.db.WithContext(ctx).Where("subscription_id = ?", subscriptionID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if eventIDPrefix != "" {
		q = q.Where("event_id LIKE ?", eventIDPrefix+"%")
	}
	var out []WebhookDelivery
	err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&out).Error
	return out, err
}
