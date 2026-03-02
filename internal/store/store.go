package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dread.sh/internal/event"

	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database for event persistence.
type Store struct {
	db *sql.DB
}

// New opens (or creates) a SQLite database and runs migrations.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}

	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return &Store{db: db}, nil
}

// Insert persists an event for a given channel.
func (s *Store) Insert(channel string, e *event.Event) error {
	_, err := s.db.Exec(
		`INSERT INTO events (id, channel, source, type, summary, raw_json, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, channel, e.Source, e.Type, e.Summary, e.RawJSON, e.Timestamp.UTC(),
	)
	return err
}

// List returns events for the given channels ordered by timestamp descending with keyset pagination.
func (s *Store) List(channels []string, before time.Time, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	if len(channels) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(channels))
	args := make([]interface{}, len(channels))
	for i, ch := range channels {
		placeholders[i] = "?"
		args[i] = ch
	}
	inClause := strings.Join(placeholders, ",")

	var query string
	if before.IsZero() {
		query = fmt.Sprintf(
			`SELECT id, channel, source, type, summary, raw_json, timestamp FROM events WHERE channel IN (%s) ORDER BY timestamp DESC LIMIT ?`,
			inClause,
		)
		args = append(args, limit)
	} else {
		query = fmt.Sprintf(
			`SELECT id, channel, source, type, summary, raw_json, timestamp FROM events WHERE channel IN (%s) AND timestamp < ? ORDER BY timestamp DESC LIMIT ?`,
			inClause,
		)
		args = append(args, before.UTC(), limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []event.Event
	for rows.Next() {
		var e event.Event
		if err := rows.Scan(&e.ID, &e.Channel, &e.Source, &e.Type, &e.Summary, &e.RawJSON, &e.Timestamp); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// GetByID returns a single event by its ID.
func (s *Store) GetByID(id string) (*event.Event, error) {
	var e event.Event
	err := s.db.QueryRow(
		`SELECT id, channel, source, type, summary, raw_json, timestamp FROM events WHERE id = ?`,
		id,
	).Scan(&e.ID, &e.Channel, &e.Source, &e.Type, &e.Summary, &e.RawJSON, &e.Timestamp)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Increment atomically increments a stats counter by 1.
func (s *Store) Increment(key string) {
	s.db.Exec(
		`INSERT INTO stats (key, count) VALUES (?, 1) ON CONFLICT(key) DO UPDATE SET count = count + 1`,
		key,
	)
}

// GetStats returns all stats counters as a map.
func (s *Store) GetStats() map[string]int64 {
	rows, err := s.db.Query(`SELECT key, count FROM stats`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	stats := make(map[string]int64)
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err == nil {
			stats[key] = count
		}
	}
	return stats
}

// SaveWorkspace upserts a workspace with the given channels JSON.
func (s *Store) SaveWorkspace(id string, channelsJSON string) error {
	_, err := s.db.Exec(
		`INSERT INTO workspaces (id, channels, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET channels = excluded.channels, updated_at = excluded.updated_at`,
		id, channelsJSON, time.Now().UTC(),
	)
	return err
}

// GetWorkspace returns the channels JSON for a workspace.
func (s *Store) GetWorkspace(id string) (string, error) {
	var channels string
	err := s.db.QueryRow(`SELECT channels FROM workspaces WHERE id = ?`, id).Scan(&channels)
	if err != nil {
		return "", err
	}
	return channels, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
