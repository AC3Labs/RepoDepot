package server

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const dbTime = "2006-01-02 15:04:05"

func nowDB() string { return time.Now().UTC().Format(dbTime) }

func parseDB(s string) time.Time {
	t, _ := time.Parse(dbTime, s)
	return t
}

type Store struct {
	db *sql.DB
}

// migrations run in order; each runs once (tracked by PRAGMA user_version).
var migrations = []string{
	`CREATE TABLE users (
		id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL UNIQUE COLLATE NOCASE,
		password TEXT NOT NULL, created_at TEXT NOT NULL);
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		csrf TEXT NOT NULL, expires_at TEXT NOT NULL);
	CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE machines (
		id INTEGER PRIMARY KEY, name TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL,
		token TEXT NOT NULL, use_tls INTEGER NOT NULL DEFAULT 0,
		os TEXT NOT NULL DEFAULT '', arch TEXT NOT NULL DEFAULT '', hostname TEXT NOT NULL DEFAULT '',
		agent_version TEXT NOT NULL DEFAULT '', docker INTEGER NOT NULL DEFAULT 0, admin INTEGER NOT NULL DEFAULT 0,
		apps_dir TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'unknown', last_seen TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL);
	CREATE TABLE apps (
		id INTEGER PRIMARY KEY, repo TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
		html_url TEXT NOT NULL DEFAULT '', slug TEXT NOT NULL,
		machine_id INTEGER NOT NULL REFERENCES machines(id), method TEXT NOT NULL,
		track TEXT NOT NULL, branch TEXT NOT NULL DEFAULT '', dir TEXT NOT NULL, location TEXT NOT NULL DEFAULT '',
		installed_ref TEXT NOT NULL DEFAULT '', installed_label TEXT NOT NULL DEFAULT '', installed_at TEXT NOT NULL DEFAULT '',
		latest_ref TEXT NOT NULL DEFAULT '', latest_label TEXT NOT NULL DEFAULT '', latest_url TEXT NOT NULL DEFAULT '',
		latest_date TEXT NOT NULL DEFAULT '', checked_at TEXT NOT NULL DEFAULT '', check_error TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'installing', notified_ref TEXT NOT NULL DEFAULT '',
		auto_update INTEGER NOT NULL DEFAULT 0, notify INTEGER NOT NULL DEFAULT 1,
		options TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		UNIQUE (machine_id, slug));
	CREATE TABLE jobs (
		id INTEGER PRIMARY KEY, app_id INTEGER REFERENCES apps(id) ON DELETE SET NULL,
		app_label TEXT NOT NULL, machine_id INTEGER REFERENCES machines(id) ON DELETE SET NULL,
		machine_label TEXT NOT NULL, action TEXT NOT NULL, ref TEXT NOT NULL DEFAULT '', target TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
		agent_job TEXT NOT NULL DEFAULT '', log TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '');
	CREATE INDEX jobs_app ON jobs(app_id);
	CREATE TABLE repo_meta (
		repo TEXT PRIMARY KEY, logo TEXT NOT NULL DEFAULT '', logo_checked TEXT NOT NULL DEFAULT '');
	CREATE TABLE http_cache (
		url TEXT PRIMARY KEY, etag TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL,
		body BLOB NOT NULL, fetched_at TEXT NOT NULL);`,
	// 0.3: users with roles and email invites, update controls, audit log.
	`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin';
	ALTER TABLE users ADD COLUMN invite_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN invite_expires TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN invited_by TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN reset_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN reset_expires TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN last_login TEXT NOT NULL DEFAULT '';
	ALTER TABLE apps ADD COLUMN skipped_ref TEXT NOT NULL DEFAULT '';
	ALTER TABLE apps ADD COLUMN held INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE apps ADD COLUMN previous_ref TEXT NOT NULL DEFAULT '';
	ALTER TABLE apps ADD COLUMN previous_label TEXT NOT NULL DEFAULT '';
	ALTER TABLE apps ADD COLUMN auto_tried_ref TEXT NOT NULL DEFAULT '';
	ALTER TABLE jobs ADD COLUMN started_by TEXT NOT NULL DEFAULT '';
	CREATE TABLE events (
		id INTEGER PRIMARY KEY, at TEXT NOT NULL, user_label TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '');`,
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return nil, fmt.Errorf("can't open the database at %s: %w", path, err)
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		for _, stmt := range strings.Split(migrations[i], ";\n") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) exec(q string, args ...any) (int64, error) {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}
