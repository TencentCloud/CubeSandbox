// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package dao

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3/lock"
	"gorm.io/gorm"
)

type fakeDriver struct {
	name string
}

func (d fakeDriver) Name() string { return d.name }

func (d fakeDriver) Open(context.Context, Config) (*sql.DB, *gorm.DB, error) {
	db, err := sql.Open("mysql", "fake:fake@tcp(127.0.0.1:1)/fake")
	if err != nil {
		return nil, nil, err
	}
	return db, &gorm.DB{}, nil
}

func (d fakeDriver) SessionLocker(Config) lock.SessionLocker { return nil }

func TestOpenIsIdempotentOnlyForSameIdentity(t *testing.T) {
	const driverName = "fake_identity"
	if _, exists := driverRegistry[driverName]; !exists {
		Register(fakeDriver{name: driverName})
	}
	t.Cleanup(func() {
		if err := Close(); err != nil {
			t.Fatalf("dao.Close: %v", err)
		}
	})

	cfg := Config{
		Driver: driverName,
		Addr:   "127.0.0.1:3306",
		User:   "user-a",
		Pwd:    "pwd-a",
		DBName: "db-a",
	}
	first, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	second, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second Open with same config: %v", err)
	}
	if first != second {
		t.Fatalf("same config returned different gorm handles")
	}

	_, err = Open(context.Background(), Config{
		Driver: driverName,
		Addr:   "127.0.0.1:3306",
		User:   "user-a",
		Pwd:    "pwd-a",
		DBName: "db-b",
	})
	if err == nil {
		t.Fatalf("Open with different DBName succeeded, want error")
	}
	if errors.Is(err, ErrNotOpened) {
		t.Fatalf("Open with different config returned ErrNotOpened: %v", err)
	}
}
