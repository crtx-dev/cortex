package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type migration struct {
	version int
	name    string
	sql     string
}

var databaseMigrations = []migration{
	{
		version: 1,
		name:    "sqlite foundation",
		sql: `CREATE TABLE application_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
	},
	{
		version: 2,
		name:    "durable conversations",
		sql: `CREATE TABLE conversations (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			workspace TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			opencode_session_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'idle',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			archived_at INTEGER
		);
		CREATE INDEX conversations_updated_idx ON conversations(archived_at, updated_at DESC);
		CREATE TABLE conversation_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			kind TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE(conversation_id, sequence)
		);
		CREATE TABLE agent_runs (
			id TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			state TEXT NOT NULL,
			prompt TEXT NOT NULL,
			started_at INTEGER NOT NULL,
			finished_at INTEGER,
			error TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			estimated_cost_usd REAL NOT NULL DEFAULT 0
		);
		CREATE INDEX agent_runs_conversation_idx ON agent_runs(conversation_id, started_at DESC);`,
	},
	{
		version: 3,
		name:    "image attachment names",
		sql:     `ALTER TABLE conversation_events ADD COLUMN name TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 4,
		name:    "server-owned agent run events and outcomes",
		sql: `CREATE TABLE agent_run_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			kind TEXT NOT NULL,
			text TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			UNIQUE(run_id, sequence)
		);
		CREATE INDEX agent_run_events_run_idx ON agent_run_events(run_id, sequence);
		ALTER TABLE conversations ADD COLUMN current_run_id TEXT NOT NULL DEFAULT '';
		ALTER TABLE agent_runs ADD COLUMN diagnostics TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 5,
		name:    "durable task run identity",
		sql:     `ALTER TABLE conversation_events ADD COLUMN run_id TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 6,
		name:    "embedded instance launcher",
		sql: `CREATE TABLE launcher_instances (
			id TEXT PRIMARY KEY,
			position INTEGER NOT NULL UNIQUE,
			name TEXT NOT NULL,
			domain TEXT NOT NULL,
			port INTEGER CHECK(port BETWEEN 1 AND 65535)
		);`,
	},
	{
		version: 7,
		name:    "at most one running agent run per conversation",
		sql:     `CREATE UNIQUE INDEX agent_runs_one_running ON agent_runs(conversation_id) WHERE state='running';`,
	},
}

func openDatabase(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "cortex.db")
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("mode", "rwc")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "FULL")
	q.Set("_defensive", "true")
	q.Set("_dqs", "false")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetConnMaxLifetime(0)
	db.SetMaxIdleConns(4)
	db.SetMaxOpenConns(8)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open Cortex database: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, fmt.Errorf("protect Cortex database: %w", err)
	}
	if err := migrateDatabase(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrateDatabase(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read database schema: %w", err)
	}
	if current > len(databaseMigrations) {
		return fmt.Errorf("Cortex database schema %d is newer than supported schema %d", current, len(databaseMigrations))
	}
	for _, m := range databaseMigrations {
		if m.version <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err = tx.ExecContext(ctx, m.sql); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)", m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano))
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}
