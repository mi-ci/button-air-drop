package storage

import (
	"database/sql"
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
CREATE TABLE IF NOT EXISTS email_codes (
	email TEXT NOT NULL,
	code TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_email_codes_email ON email_codes(email);

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
`

	_, err := db.Exec(schema)
	return err
}
