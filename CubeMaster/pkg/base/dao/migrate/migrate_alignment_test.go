// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/migrate"
)

// bookkeepingTables are goose / fingerprint bookkeeping objects that exist
// on both dialects but are not part of the logical application schema.
var bookkeepingTables = map[string]bool{
	"goose_db_version":                   true,
	"t_cubemaster_migration_fingerprint": true,
}

// TestSchemaAlignment_MySQL_vs_Postgres migrates both engines to HEAD inside
// throwaway dockertest containers and asserts their normalized logical schemas
// are equivalent. This is the primary defence against dialect drift.
func TestSchemaAlignment_MySQL_vs_Postgres(t *testing.T) {
	mysqlEnv := newMySQLEnv(t)
	defer mysqlEnv.teardown()
	pgEnv := newPostgresEnv(t)
	defer pgEnv.teardown()

	mysqlDB := openMySQLDB(t, mysqlEnv.dsn)
	defer mysqlDB.Close()
	pgDB := openPostgresDB(t, pgEnv.dsn)
	defer pgDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	if err := migrate.Run(ctx, mysqlDB, "mysql", testSessionLocker()); err != nil {
		t.Fatalf("migrate.Run mysql: %v", err)
	}
	if err := migrate.Run(ctx, pgDB, "postgres", pgTestSessionLocker()); err != nil {
		t.Fatalf("migrate.Run postgres: %v", err)
	}

	mysqlSchema := extractMySQLSchema(ctx, t, mysqlDB)
	pgSchema := extractPostgresSchema(ctx, t, pgDB)

	assertSchemasAligned(t, mysqlSchema, pgSchema)
}

// normalizedSchema is the dialect-independent model used for cross-engine
// comparison.
type normalizedSchema struct {
	tables map[string]normalizedTable
}

type normalizedTable struct {
	columns map[string]normalizedColumn
	indexes map[string]normalizedIndex // key = index signature string
}

type normalizedColumn struct {
	nullable     bool
	typeCategory string
	// defaultKind classifies the default: "none", "literal", "current_timestamp", "sequence".
	defaultKind string
	// defaultValue is the normalized literal for kind=="literal" (e.g. "0", "false", "").
	// Empty for non-literal kinds.
	defaultValue string
}

type normalizedIndex struct {
	columns []string // ordered
	unique  bool
	primary bool
}

func (idx normalizedIndex) signature() string {
	return fmt.Sprintf("cols=%s|unique=%t|primary=%t",
		strings.Join(idx.columns, ","), idx.unique, idx.primary)
}

func assertSchemasAligned(t *testing.T, mysql, pg normalizedSchema) {
	t.Helper()

	mysqlTables := sortedMapKeys(mysql.tables)
	pgTables := sortedMapKeys(pg.tables)

	// Step 1: table set — catches missing tables like runtime_active quickly.
	if !stringSlicesEqual(mysqlTables, pgTables) {
		t.Errorf("table set mismatch:\n  mysql only: %v\n  postgres only: %v",
			diffStrings(mysqlTables, pgTables), diffStrings(pgTables, mysqlTables))
	}

	for _, name := range mysqlTables {
		pgTable, ok := pg.tables[name]
		if !ok {
			continue
		}
		mysqlTable := mysql.tables[name]

		mysqlCols := sortedMapKeys(mysqlTable.columns)
		pgCols := sortedMapKeys(pgTable.columns)
		if !stringSlicesEqual(mysqlCols, pgCols) {
			t.Errorf("%s: column set mismatch:\n  mysql only: %v\n  postgres only: %v",
				name, diffStrings(mysqlCols, pgCols), diffStrings(pgCols, mysqlCols))
		}
		for _, col := range mysqlCols {
			mc, mok := mysqlTable.columns[col]
			pc, pok := pgTable.columns[col]
			if !mok || !pok {
				continue
			}
			if mc.nullable != pc.nullable {
				t.Errorf("%s.%s: nullable mismatch mysql=%v postgres=%v",
					name, col, mc.nullable, pc.nullable)
			}
			if mc.typeCategory != pc.typeCategory {
				t.Errorf("%s.%s: type category mismatch mysql=%q postgres=%q",
					name, col, mc.typeCategory, pc.typeCategory)
			}
			if !defaultsCompatible(col, mc, pc) {
				t.Errorf("%s.%s: default mismatch mysql=%s(%q) postgres=%s(%q)",
					name, col, mc.defaultKind, mc.defaultValue, pc.defaultKind, pc.defaultValue)
			}
		}

		mysqlIdx := sortedMapKeys(mysqlTable.indexes)
		pgIdx := sortedMapKeys(pgTable.indexes)
		if !stringSlicesEqual(mysqlIdx, pgIdx) {
			t.Errorf("%s: index signature mismatch:\n  mysql only: %v\n  postgres only: %v",
				name, diffStrings(mysqlIdx, pgIdx), diffStrings(pgIdx, mysqlIdx))
		}
	}
}

func extractMySQLSchema(ctx context.Context, t *testing.T, db *sql.DB) normalizedSchema {
	t.Helper()
	tables := listMySQLTables(ctx, t, db)
	out := normalizedSchema{tables: map[string]normalizedTable{}}
	for _, table := range tables {
		if bookkeepingTables[table] {
			continue
		}
		out.tables[table] = normalizedTable{
			columns: extractMySQLColumns(ctx, t, db, table),
			indexes: extractMySQLIndexes(ctx, t, db, table),
		}
	}
	return out
}

func extractPostgresSchema(ctx context.Context, t *testing.T, db *sql.DB) normalizedSchema {
	t.Helper()
	tables := listPostgresTables(ctx, t, db)
	out := normalizedSchema{tables: map[string]normalizedTable{}}
	for _, table := range tables {
		if bookkeepingTables[table] {
			continue
		}
		out.tables[table] = normalizedTable{
			columns: extractPostgresColumns(ctx, t, db, table),
			indexes: extractPostgresIndexes(ctx, t, db, table),
		}
	}
	return out
}

func listMySQLTables(ctx context.Context, t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("list mysql tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan mysql table: %v", err)
		}
		out = append(out, name)
	}
	return out
}

func listPostgresTables(ctx context.Context, t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables
		  WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("list postgres tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan postgres table: %v", err)
		}
		out = append(out, name)
	}
	return out
}

func extractMySQLColumns(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]normalizedColumn {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME, IS_NULLABLE, DATA_TYPE, COLUMN_TYPE, COLUMN_DEFAULT, EXTRA
		   FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		  ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		t.Fatalf("mysql columns for %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]normalizedColumn{}
	for rows.Next() {
		var name, nullable, dataType, columnType, extra string
		var def sql.NullString
		if err := rows.Scan(&name, &nullable, &dataType, &columnType, &def, &extra); err != nil {
			t.Fatalf("scan mysql column: %v", err)
		}
		kind, val := classifyMySQLDefault(def, extra)
		out[name] = normalizedColumn{
			nullable:     nullable == "YES",
			typeCategory: normalizeMySQLType(dataType, columnType),
			defaultKind:  kind,
			defaultValue: val,
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate mysql columns for %s: %v", table, err)
	}
	return out
}

func extractPostgresColumns(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]normalizedColumn {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT column_name, is_nullable, data_type, udt_name,
		        character_maximum_length, column_default
		   FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = $1
		  ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("postgres columns for %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]normalizedColumn{}
	for rows.Next() {
		var name, nullable, dataType, udtName string
		var charLen sql.NullInt64
		var def sql.NullString
		if err := rows.Scan(&name, &nullable, &dataType, &udtName, &charLen, &def); err != nil {
			t.Fatalf("scan postgres column: %v", err)
		}
		kind, val := classifyPostgresDefault(def)
		out[name] = normalizedColumn{
			nullable:     nullable == "YES",
			typeCategory: normalizePostgresType(dataType, udtName, charLen),
			defaultKind:  kind,
			defaultValue: val,
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate postgres columns for %s: %v", table, err)
	}
	return out
}

// normalizeMySQLType maps MySQL DATA_TYPE/COLUMN_TYPE to a shared category.
func normalizeMySQLType(dataType, columnType string) string {
	dt := strings.ToLower(dataType)
	ct := strings.ToLower(columnType)
	switch dt {
	case "bigint":
		return "int64"
	case "int", "integer", "mediumint":
		return "int32"
	case "smallint":
		return "int16"
	case "tinyint":
		// tinyint(1) is the conventional boolean encoding.
		if strings.Contains(ct, "tinyint(1)") {
			return "bool"
		}
		return "int16"
	case "boolean", "bool":
		return "bool"
	case "varchar":
		// Preserve length: varchar(128) stays varchar(128).
		if i := strings.Index(ct, "varchar("); i >= 0 {
			end := strings.Index(ct[i:], ")")
			if end > 0 {
				return ct[i : i+end+1]
			}
		}
		return "varchar"
	case "char":
		return "varchar" // treat fixed-width as varchar category for alignment
	case "text", "mediumtext", "longtext", "tinytext":
		return "text"
	case "json":
		return "json"
	case "datetime", "timestamp":
		return "timestamp"
	case "float":
		return "float32"
	case "double", "double precision":
		return "float64"
	case "decimal", "numeric":
		return "decimal"
	case "blob", "mediumblob", "longblob", "tinyblob", "binary", "varbinary":
		return "bytes"
	default:
		return dt
	}
}

func normalizePostgresType(dataType, udtName string, charLen sql.NullInt64) string {
	dt := strings.ToLower(dataType)
	udt := strings.ToLower(udtName)
	switch {
	case udt == "int8" || dt == "bigint":
		return "int64"
	case udt == "int4" || dt == "integer":
		return "int32"
	case udt == "int2" || dt == "smallint":
		return "int16"
	case udt == "bool" || dt == "boolean":
		return "bool"
	case dt == "character varying" || dt == "varchar":
		if charLen.Valid {
			return fmt.Sprintf("varchar(%d)", charLen.Int64)
		}
		return "varchar"
	case dt == "character" || dt == "char":
		if charLen.Valid {
			return fmt.Sprintf("varchar(%d)", charLen.Int64)
		}
		return "varchar"
	case dt == "text":
		return "text"
	case udt == "json" || udt == "jsonb" || dt == "json" || dt == "jsonb":
		return "json"
	case dt == "timestamp without time zone" || dt == "timestamp with time zone" ||
		dt == "timestamp" || dt == "date":
		return "timestamp"
	case udt == "float4" || dt == "real":
		return "float32"
	case udt == "float8" || dt == "double precision":
		return "float64"
	case dt == "numeric" || dt == "decimal":
		return "decimal"
	case dt == "bytea":
		return "bytes"
	default:
		if udt != "" {
			return udt
		}
		return dt
	}
}

// classifyMySQLDefault returns (kind, normalizedValue).
// COLUMN_DEFAULT NULL (Valid=false) means no explicit default → "none".
// COLUMN_DEFAULT ” (Valid=true, empty) means DEFAULT ” → literal "".
func classifyMySQLDefault(def sql.NullString, extra string) (kind, value string) {
	if strings.Contains(strings.ToLower(extra), "auto_increment") {
		return "sequence", ""
	}
	if !def.Valid {
		return "none", ""
	}
	s := strings.TrimSpace(def.String)
	lower := strings.ToLower(s)
	if strings.Contains(lower, "current_timestamp") || lower == "now()" {
		return "current_timestamp", ""
	}
	return "literal", normalizeDefaultLiteral(s)
}

func classifyPostgresDefault(def sql.NullString) (kind, value string) {
	if !def.Valid {
		return "none", ""
	}
	s := strings.TrimSpace(def.String)
	lower := strings.ToLower(s)
	if strings.Contains(lower, "nextval(") {
		return "sequence", ""
	}
	if strings.Contains(lower, "current_timestamp") || strings.Contains(lower, "now()") {
		return "current_timestamp", ""
	}
	// Bare NULL / NULL::typename is an explicit null default — equivalent to
	// MySQL COLUMN_DEFAULT IS NULL. Do NOT treat the string literal 'null' as none.
	if lower == "null" || strings.HasPrefix(lower, "null::") {
		return "none", ""
	}
	return "literal", normalizeDefaultLiteral(s)
}

// normalizeDefaultLiteral strips dialect-specific quoting/casts so MySQL "0"
// and Postgres "0" / "false" / "'x'::character varying" compare meaningfully.
func normalizeDefaultLiteral(raw string) string {
	s := strings.TrimSpace(raw)
	lower := strings.ToLower(s)

	// Postgres typed literal: 'foo'::character varying / 0::integer / false
	if i := strings.Index(lower, "::"); i >= 0 {
		s = strings.TrimSpace(s[:i])
		lower = strings.ToLower(s)
	}

	switch lower {
	case "0", "false", "b'0'", `b"0"`:
		return "false"
	case "1", "true", "b'1'", `b"1"`:
		return "true"
	}

	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			inner := s[1 : len(s)-1]
			return strings.ReplaceAll(inner, "''", "'")
		}
	}
	return s
}

// defaultsCompatible decides whether two columns' defaults are logically
// equivalent across dialects. Deliberately narrow — no global none↔literal.
func defaultsCompatible(col string, mc, pc normalizedColumn) bool {
	if mc.defaultKind == pc.defaultKind {
		if mc.defaultKind == "literal" {
			return mc.defaultValue == pc.defaultValue
		}
		return true
	}

	// AUTO_INCREMENT / bigserial on "id": allow sequence↔none asymmetry.
	if col == "id" {
		ok := map[string]bool{"sequence": true, "none": true}
		if ok[mc.defaultKind] && ok[pc.defaultKind] {
			return true
		}
	}

	// Narrow whitelist only: NOT NULL text/varchar where one side has no
	// default and the other has DEFAULT '' (documented PG habit for columns
	// like request_json). Any other none↔literal (e.g. DEFAULT 0 vs none) fails.
	if !mc.nullable && !pc.nullable && isTextLikeType(mc.typeCategory) && isTextLikeType(pc.typeCategory) {
		if (mc.defaultKind == "none" && pc.defaultKind == "literal" && pc.defaultValue == "") ||
			(pc.defaultKind == "none" && mc.defaultKind == "literal" && mc.defaultValue == "") {
			return true
		}
	}
	return false
}

func isTextLikeType(cat string) bool {
	return cat == "text" || strings.HasPrefix(cat, "varchar")
}

func extractMySQLIndexes(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]normalizedIndex {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME
		   FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		  ORDER BY INDEX_NAME, SEQ_IN_INDEX`, table)
	if err != nil {
		t.Fatalf("mysql indexes for %s: %v", table, err)
	}
	defer rows.Close()

	type acc struct {
		cols    []string
		unique  bool
		primary bool
	}
	byName := map[string]*acc{}
	for rows.Next() {
		var indexName, colName string
		var nonUnique, seq int
		if err := rows.Scan(&indexName, &nonUnique, &seq, &colName); err != nil {
			t.Fatalf("scan mysql index: %v", err)
		}
		a, ok := byName[indexName]
		if !ok {
			a = &acc{
				unique:  nonUnique == 0,
				primary: strings.EqualFold(indexName, "PRIMARY"),
			}
			byName[indexName] = a
		}
		a.cols = append(a.cols, colName)
	}

	out := map[string]normalizedIndex{}
	for _, a := range byName {
		idx := normalizedIndex{columns: a.cols, unique: a.unique, primary: a.primary}
		out[idx.signature()] = idx
	}
	return out
}

func extractPostgresIndexes(ctx context.Context, t *testing.T, db *sql.DB, table string) map[string]normalizedIndex {
	t.Helper()
	// pg_index + pg_attribute gives ordered columns; indisunique / indisprimary
	// cover UNIQUE and PRIMARY KEY. Exclude expression indexes (attname NULL).
	rows, err := db.QueryContext(ctx, `
		SELECT
			i.relname AS index_name,
			ix.indisunique,
			ix.indisprimary,
			a.attname AS column_name,
			array_position(ix.indkey, a.attnum) AS ord
		FROM pg_class t
		JOIN pg_index ix ON t.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY (ix.indkey)
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = current_schema()
		  AND t.relname = $1
		  AND t.relkind = 'r'
		ORDER BY i.relname, ord`, table)
	if err != nil {
		t.Fatalf("postgres indexes for %s: %v", table, err)
	}
	defer rows.Close()

	type acc struct {
		cols    []string
		unique  bool
		primary bool
	}
	byName := map[string]*acc{}
	for rows.Next() {
		var indexName, colName string
		var unique, primary bool
		var ord sql.NullInt64
		if err := rows.Scan(&indexName, &unique, &primary, &colName, &ord); err != nil {
			t.Fatalf("scan postgres index: %v", err)
		}
		a, ok := byName[indexName]
		if !ok {
			a = &acc{unique: unique, primary: primary}
			byName[indexName] = a
		}
		a.cols = append(a.cols, colName)
	}

	out := map[string]normalizedIndex{}
	for _, a := range byName {
		idx := normalizedIndex{columns: a.cols, unique: a.unique, primary: a.primary}
		out[idx.signature()] = idx
	}
	return out
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diffStrings(have, against []string) []string {
	set := map[string]bool{}
	for _, s := range against {
		set[s] = true
	}
	var only []string
	for _, s := range have {
		if !set[s] {
			only = append(only, s)
		}
	}
	return only
}
