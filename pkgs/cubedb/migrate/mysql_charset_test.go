// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package migrate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	_ "github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao/driver/mysql"
)

// Exercise the production driver's DSN rather than the fixture's raw SQL
// connection: a utf8mb4 column alone cannot accept supplementary characters
// when the client connection advertises utf8mb3.
func TestMySQLDriverUTF8MB4RoundTrip(t *testing.T) {
	env := newMySQLEnv(t)
	defer env.teardown()
	parsed, err := mysqldriver.ParseDSN(env.dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = dao.Open(ctx, dao.Config{
		Driver: "mysql", Addr: parsed.Addr, User: parsed.User,
		Pwd: parsed.Passwd, DBName: parsed.DBName,
		MaxOpenConns: 2, MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatalf("open production MySQL driver: %v", err)
	}
	defer dao.Close()
	conn, err := dao.SQL().Conn(ctx)
	if err != nil {
		t.Fatalf("reserve connection: %v", err)
	}
	defer conn.Close()
	var client, connection, results string
	if err := conn.QueryRowContext(ctx, "SELECT @@character_set_client, @@character_set_connection, @@character_set_results").Scan(&client, &connection, &results); err != nil {
		t.Fatalf("session character sets: %v", err)
	}
	if client != "utf8mb4" || connection != "utf8mb4" || results != "utf8mb4" {
		t.Fatalf("session character sets = %s/%s/%s, want utf8mb4 throughout", client, connection, results)
	}

	for _, charset := range []string{"utf8mb4", "utf8mb3"} {
		t.Run(charset, func(t *testing.T) {
			// This table belongs only to this connection and disappears on close.
			if _, err := conn.ExecContext(ctx, "CREATE TEMPORARY TABLE charset_roundtrip (id INT PRIMARY KEY, private_data VARCHAR(1024)) DEFAULT CHARSET="+charset); err != nil {
				t.Fatalf("create temporary table: %v", err)
			}
			defer conn.ExecContext(ctx, "DROP TEMPORARY TABLE charset_roundtrip")
			values := []struct{ name, value string }{
				{"empty", ""}, {"ascii", "bucket/prefix/ref-123"},
				{"bmp_1024_bytes", strings.Repeat("界", 341) + "x"},
			}
			if charset == "utf8mb4" {
				values = append(values, struct{ name, value string }{"supplementary_4_bytes", "🙂"},
					struct{ name, value string }{"supplementary_1024_bytes", strings.Repeat("🙂", 256)})
			}
			for id, tc := range values {
				t.Run(tc.name, func(t *testing.T) {
					if _, err := conn.ExecContext(ctx, "INSERT INTO charset_roundtrip (id, private_data) VALUES (?, ?)", id, tc.value); err != nil {
						t.Fatalf("insert prepared parameter: %v", err)
					}
					var got string
					if err := conn.QueryRowContext(ctx, "SELECT private_data FROM charset_roundtrip WHERE id = ?", id).Scan(&got); err != nil {
						t.Fatalf("read value: %v", err)
					}
					if got != tc.value {
						t.Fatal("round trip changed private data")
					}
				})
			}
		})
	}
}
