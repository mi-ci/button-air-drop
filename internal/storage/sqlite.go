package storage

import (
	"database/sql"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(time.Hour)

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
	email TEXT PRIMARY KEY,
	nickname TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ranking_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ranking_date TEXT NOT NULL,
	email TEXT NOT NULL,
	masked_email TEXT NOT NULL,
	duration_ms INTEGER NOT NULL,
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ranking_entries_date_duration
	ON ranking_entries(ranking_date, duration_ms DESC, created_at ASC);

CREATE TABLE IF NOT EXISTS current_rounds (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	ranking_date TEXT NOT NULL,
	leader_email TEXT NOT NULL,
	leader_since TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS daily_click_usage (
	ranking_date TEXT NOT NULL,
	email TEXT NOT NULL,
	click_count INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (ranking_date, email)
);

CREATE TABLE IF NOT EXISTS auth_request_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ip_address TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auth_request_log_ip_created
	ON auth_request_log(ip_address, created_at);
`

	if _, err := db.Exec(schema); err != nil {
		return err
	}

	if _, err := db.Exec(`ALTER TABLE ranking_entries ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN auth_provider TEXT NOT NULL DEFAULT 'email'`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN kakao_id TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN contact_email TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN contact_email_consent INTEGER NOT NULL DEFAULT 0`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN contact_email_consent_at TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN nickname_changed_at TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN contact_email_changed_at TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_kakao_id ON users(kakao_id)`); err != nil {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE users DROP COLUMN login_email`); err != nil && !isMissingColumnError(err) && !isUnsupportedAlterError(err) {
		return err
	}

	return nil
}

func isDuplicateColumnError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name:")
}

func isMissingColumnError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such column:")
}

func isUnsupportedAlterError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "syntax error")
}
