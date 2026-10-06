package task

import (
	"context"
	"database/sql"
	"fmt"
)

const currentSchemaVersion = 4

// Migrate applies all schema changes atomically and may be called on every startup.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema version table: %w", err)
	}
	var version, minVersion, rows int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(version),0), COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&rows, &minVersion, &version)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if rows > 1 {
		if minVersion != version {
			return fmt.Errorf("inconsistent schema versions recorded: %d..%d", minVersion, version)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
			return fmt.Errorf("normalize schema version rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (?)`, version); err != nil {
			return fmt.Errorf("restore schema version row: %w", err)
		}
	}

	if version < 1 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY, status TEXT NOT NULL, prompt TEXT NOT NULL, input_file_id TEXT NOT NULL,
			output_file_id TEXT NOT NULL DEFAULT '', worker_id TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, inference_duration INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
			return fmt.Errorf("create tasks table: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (1)`); err != nil {
			return fmt.Errorf("record schema version 1: %w", err)
		}
		version = 1
	}
	if version < 2 {
		columns, err := tableColumns(ctx, tx, "tasks")
		if err != nil {
			return err
		}
		if _, exists := columns["owner_type"]; exists {
			return fmt.Errorf("migration conflict: tasks.owner_type exists before schema version 2")
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE tasks ADD COLUMN owner_type TEXT NOT NULL DEFAULT 'system'`); err != nil {
			return fmt.Errorf("add task owner type: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE tasks ADD COLUMN owner_id TEXT NOT NULL DEFAULT 'legacy'`); err != nil {
			return fmt.Errorf("add task owner id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET owner_type='system',owner_id='legacy'`); err != nil {
			return fmt.Errorf("assign legacy task owner: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS tasks_owner_created_idx ON tasks(owner_type,owner_id,created_at DESC,id DESC)`); err != nil {
			return fmt.Errorf("create task owner index: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET version=2`); err != nil {
			return fmt.Errorf("record schema version 2: %w", err)
		}
	}
	if version < 3 {
		for _, statement := range []string{
			`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, username TEXT NOT NULL, username_key TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, principal_type TEXT NOT NULL, principal_id TEXT NOT NULL, username TEXT NOT NULL DEFAULT '', csrf_token TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS sessions_principal_idx ON sessions(principal_type,principal_id)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("create identity schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET version=3`); err != nil {
			return fmt.Errorf("record schema version 3: %w", err)
		}
		version = 3
	}
	if version < 4 {
		for _, statement := range []string{
			`CREATE TABLE IF NOT EXISTS files (id TEXT PRIMARY KEY, owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS files_owner_created_idx ON files(owner_type,owner_id,created_at DESC,id DESC)`,
			`CREATE INDEX IF NOT EXISTS files_task_idx ON files(task_id)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS schema_migrations_version_idx ON schema_migrations(version)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("create file schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET version=4`); err != nil {
			return fmt.Errorf("record schema version 4: %w", err)
		}
		version = 4
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	return tx.Commit()
}

type queryExecer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func tableColumns(ctx context.Context, q queryExecer, table string) (map[string]struct{}, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	columns := make(map[string]struct{})
	for rows.Next() {
		var cid, notnull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notnull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan %s column: %w", table, err)
		}
		columns[name] = struct{}{}
	}
	return columns, rows.Err()
}
