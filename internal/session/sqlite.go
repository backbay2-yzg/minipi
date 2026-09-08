package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// SQLiteStore persists sessions in a single SQLite database. SQLite is used
// through modernc.org/sqlite, so this backend does not require cgo.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) path and ensures the sessions table exists.
// The path may be ":memory:" for tests.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("session: open sqlite: %w", err)
	}
	// A single connection is important for :memory: databases and is also a
	// good default for the CLI, where writes are serialized by the event loop.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	const schema = `CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		updated_at TEXT NOT NULL,
		data_json TEXT NOT NULL
	)`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("session: create schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close releases the database. Store deliberately does not include Close in
// the interface, preserving the small persistence abstraction used by CLI.
func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Save(ctx context.Context, sess *Session) error {
	if sess == nil || sess.ID == "" {
		return errors.New("session: empty id")
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, updated_at, data_json) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET updated_at = excluded.updated_at, data_json = excluded.data_json`,
		sess.ID, sess.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"), string(data))
	if err != nil {
		return fmt.Errorf("session: save: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Load(ctx context.Context, id string) (*Session, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data_json FROM sessions WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session: load: %w", err)
	}
	var sess Session
	if err := json.Unmarshal([]byte(data), &sess); err != nil {
		return nil, fmt.Errorf("session: corrupt %s: %w", id, err)
	}
	return &sess, nil
}

func (s *SQLiteStore) List(ctx context.Context) ([]Meta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, data_json FROM sessions ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	defer rows.Close()

	var out []Meta
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, fmt.Errorf("session: list scan: %w", err)
		}
		var sess Session
		if err := json.Unmarshal([]byte(data), &sess); err != nil {
			continue // match JSONStore: one corrupt session does not hide others
		}
		if sess.ID == "" {
			sess.ID = id
		}
		out = append(out, Meta{ID: sess.ID, CreatedAt: sess.CreatedAt,
			UpdatedAt: sess.UpdatedAt, Messages: len(sess.State.Messages),
			Title: title(sess.State.Messages)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

var _ Store = (*SQLiteStore)(nil)
