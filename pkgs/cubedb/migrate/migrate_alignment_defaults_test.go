// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"database/sql"
	"testing"
)

// Table-driven adversarial tests for default-value comparison. No Docker.
// These lock in the rules that prevent "same column name, different default"
// from being silently treated as aligned.

func TestNormalizeDefaultLiteral(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"0", "false"},
		{"1", "true"},
		{"false", "false"},
		{"TRUE", "true"},
		{"b'0'", "false"},
		{"'pending'", "pending"},
		{"'pending'::character varying", "pending"},
		{"''::text", ""},
		{"0::integer", "false"},
		{"42", "42"},
		{"'done'", "done"},
	}
	for _, c := range cases {
		if got := normalizeDefaultLiteral(c.in); got != c.want {
			t.Errorf("normalizeDefaultLiteral(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestClassifyMySQLDefault_EmptyStringIsLiteral(t *testing.T) {
	// Valid empty string must NOT be collapsed to "none" — that was the
	// root cause of DEFAULT '' being invisible to the comparator.
	kind, val := classifyMySQLDefault(sql.NullString{String: "", Valid: true}, "")
	if kind != "literal" || val != "" {
		t.Fatalf("DEFAULT '' → kind=%q val=%q, want literal/\"\"", kind, val)
	}
	kind, val = classifyMySQLDefault(sql.NullString{Valid: false}, "")
	if kind != "none" || val != "" {
		t.Fatalf("no default → kind=%q val=%q, want none/\"\"", kind, val)
	}
	kind, val = classifyMySQLDefault(sql.NullString{Valid: false}, "auto_increment")
	if kind != "sequence" {
		t.Fatalf("auto_increment → kind=%q, want sequence", kind)
	}
}

func TestClassifyPostgresDefault_NullIsNone(t *testing.T) {
	kind, val := classifyPostgresDefault(sql.NullString{String: "NULL::character varying", Valid: true})
	if kind != "none" || val != "" {
		t.Fatalf("DEFAULT NULL::varchar → kind=%q val=%q, want none/\"\"", kind, val)
	}
	kind, val = classifyPostgresDefault(sql.NullString{String: "NULL", Valid: true})
	if kind != "none" {
		t.Fatalf("DEFAULT NULL → kind=%q, want none", kind)
	}
	// String literal 'null' must remain a literal, not be collapsed to none.
	kind, val = classifyPostgresDefault(sql.NullString{String: "'null'::text", Valid: true})
	if kind != "literal" || val != "null" {
		t.Fatalf("'null'::text → kind=%q val=%q, want literal/\"null\"", kind, val)
	}
}

func TestDefaultsCompatible_Adversarial(t *testing.T) {
	textNN := func(kind, val string) normalizedColumn {
		return normalizedColumn{
			nullable: false, typeCategory: "text",
			defaultKind: kind, defaultValue: val,
		}
	}
	intNN := func(kind, val string) normalizedColumn {
		return normalizedColumn{
			nullable: false, typeCategory: "int32",
			defaultKind: kind, defaultValue: val,
		}
	}
	boolNN := func(kind, val string) normalizedColumn {
		return normalizedColumn{
			nullable: false, typeCategory: "bool",
			defaultKind: kind, defaultValue: val,
		}
	}

	cases := []struct {
		name string
		col  string
		mc   normalizedColumn
		pc   normalizedColumn
		want bool
	}{
		// Same literal values → compatible.
		{"same_int_zero", "n", intNN("literal", "false"), intNN("literal", "false"), true},
		{"same_bool_false", "flag", boolNN("literal", "false"), boolNN("literal", "false"), true},
		{"same_string", "status", textNN("literal", "pending"), textNN("literal", "pending"), true},

		// Different literal values → MUST fail (was previously invisible).
		{"int_0_vs_1", "n", intNN("literal", "false"), intNN("literal", "true"), false},
		{"string_pending_vs_done", "status", textNN("literal", "pending"), textNN("literal", "done"), false},
		{"bool_false_vs_true", "flag", boolNN("literal", "false"), boolNN("literal", "true"), false},

		// none ↔ literal DEFAULT 0 on int → MUST fail (global exemption removed).
		{"int_none_vs_zero", "retry_count", intNN("none", ""), intNN("literal", "false"), false},
		{"int_zero_vs_none", "retry_count", intNN("literal", "false"), intNN("none", ""), false},

		// none ↔ literal non-empty string → MUST fail.
		{"text_none_vs_pending", "status", textNN("none", ""), textNN("literal", "pending"), false},

		// Narrow whitelist: NOT NULL text, none ↔ DEFAULT '' → allowed.
		{"text_none_vs_empty", "request_json", textNN("none", ""), textNN("literal", ""), true},
		{"text_empty_vs_none", "request_json", textNN("literal", ""), textNN("none", ""), true},

		// Whitelist must NOT apply to nullable text.
		{"nullable_text_none_vs_empty", "note", normalizedColumn{
			nullable: true, typeCategory: "text", defaultKind: "none",
		}, normalizedColumn{
			nullable: true, typeCategory: "text", defaultKind: "literal", defaultValue: "",
		}, false},

		// Whitelist must NOT apply to int none↔empty (empty isn't a valid int default anyway).
		{"int_none_vs_empty_literal", "n", intNN("none", ""), intNN("literal", ""), false},

		// id sequence asymmetry → allowed.
		{"id_seq_vs_none", "id",
			normalizedColumn{nullable: false, typeCategory: "int64", defaultKind: "sequence"},
			normalizedColumn{nullable: false, typeCategory: "int64", defaultKind: "none"},
			true},
		{"id_none_vs_seq", "id",
			normalizedColumn{nullable: false, typeCategory: "int64", defaultKind: "none"},
			normalizedColumn{nullable: false, typeCategory: "int64", defaultKind: "sequence"},
			true},

		// Non-id sequence↔none → fail.
		{"other_seq_vs_none", "job_id",
			normalizedColumn{nullable: false, typeCategory: "int64", defaultKind: "sequence"},
			normalizedColumn{nullable: false, typeCategory: "int64", defaultKind: "none"},
			false},

		// current_timestamp both sides → ok.
		{"both_now", "created_at",
			normalizedColumn{nullable: false, typeCategory: "timestamp", defaultKind: "current_timestamp"},
			normalizedColumn{nullable: false, typeCategory: "timestamp", defaultKind: "current_timestamp"},
			true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := defaultsCompatible(c.col, c.mc, c.pc)
			if got != c.want {
				t.Fatalf("defaultsCompatible(%q)=%v, want %v (mysql=%s(%q) pg=%s(%q))",
					c.col, got, c.want, c.mc.defaultKind, c.mc.defaultValue, c.pc.defaultKind, c.pc.defaultValue)
			}
		})
	}
}
