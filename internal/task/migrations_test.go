package task

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrationCreatesSchemaAndIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second migration: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, `SELECT version FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("schema version = %d, want 4", version)
	}
	for _, table := range []string{"users", "sessions", "files"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s missing (count=%d, err=%v)", table, count, err)
		}
	}
	for _, column := range []string{"owner_type", "owner_id"} {
		var count int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name=?`, column).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("column %s missing (count=%d, err=%v)", column, count, err)
		}
	}
}

func TestMigrationAssignsLegacyOwnerToExistingTasks(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `CREATE TABLE tasks (
		id TEXT PRIMARY KEY, status TEXT NOT NULL, prompt TEXT NOT NULL, input_file_id TEXT NOT NULL,
		output_file_id TEXT NOT NULL DEFAULT '', worker_id TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, inference_duration INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO tasks(id,status,prompt,input_file_id,created_at) VALUES ('old','SUCCESS','legacy','input','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var ownerType, ownerID string
	if err := db.QueryRowContext(ctx, `SELECT owner_type,owner_id FROM tasks WHERE id='old'`).Scan(&ownerType, &ownerID); err != nil {
		t.Fatal(err)
	}
	if ownerType != "system" || ownerID != "legacy" {
		t.Fatalf("legacy owner = %s/%s, want system/legacy", ownerType, ownerID)
	}
}

func TestMigrationFailureDoesNotAdvanceVersionOrPartiallyAlterSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `CREATE TABLE tasks (id TEXT PRIMARY KEY, owner_type TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("expected migration conflict")
	}
	var tableCount int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tableCount)
	if tableCount != 0 {
		t.Fatalf("schema_migrations survived failed migration: count=%d", tableCount)
	}
	var ownerIDCount int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='owner_id'`).Scan(&ownerIDCount)
	if ownerIDCount != 0 {
		t.Fatalf("owner_id was partially added: count=%d", ownerIDCount)
	}
}

func TestMigrationNormalizesRepeatedVersionRows(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE schema_migrations(version INTEGER NOT NULL)`,
		`INSERT INTO schema_migrations(version) VALUES(3),(3)`,
		`CREATE TABLE tasks(id TEXT PRIMARY KEY,status TEXT NOT NULL,prompt TEXT NOT NULL,input_file_id TEXT NOT NULL,output_file_id TEXT NOT NULL DEFAULT '',worker_id TEXT NOT NULL DEFAULT '',error_message TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,started_at TEXT,finished_at TEXT,inference_duration INTEGER NOT NULL DEFAULT 0,owner_type TEXT NOT NULL DEFAULT 'system',owner_id TEXT NOT NULL DEFAULT 'legacy')`,
		`CREATE TABLE users(id TEXT PRIMARY KEY,username TEXT NOT NULL,username_key TEXT NOT NULL UNIQUE,password_hash TEXT NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE TABLE sessions(token_hash TEXT PRIMARY KEY,principal_type TEXT NOT NULL,principal_id TEXT NOT NULL,username TEXT NOT NULL DEFAULT '',csrf_token TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count, version int
	if err := db.QueryRow(`SELECT COUNT(*),MAX(version) FROM schema_migrations`).Scan(&count, &version); err != nil {
		t.Fatal(err)
	}
	if count != 1 || version != 4 {
		t.Fatalf("schema version rows=%d version=%d", count, version)
	}
}
