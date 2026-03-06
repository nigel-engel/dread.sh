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

	// Add sound column if missing (migration for existing databases).
	db.Exec(`ALTER TABLE workspaces ADD COLUMN sound TEXT NOT NULL DEFAULT ''`)

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

// TrackUniqueInstall records or updates an install by IP.
func (s *Store) TrackUniqueInstall(ip string) {
	now := time.Now().UTC()
	s.db.Exec(
		`INSERT INTO unique_installs (ip, first_seen, last_seen, count) VALUES (?, ?, ?, 1)
		 ON CONFLICT(ip) DO UPDATE SET last_seen = ?, count = count + 1`,
		ip, now, now, now,
	)
}

// GetStats returns all stats counters as a map, including unique install count.
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

	var uniqueCount int64
	s.db.QueryRow(`SELECT COUNT(*) FROM unique_installs`).Scan(&uniqueCount)
	stats["unique_installs"] = uniqueCount

	return stats
}

// SaveWorkspace upserts a workspace with the given channels JSON and sound.
func (s *Store) SaveWorkspace(id string, channelsJSON string, sound string) error {
	_, err := s.db.Exec(
		`INSERT INTO workspaces (id, channels, sound, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET channels = excluded.channels, sound = excluded.sound, updated_at = excluded.updated_at`,
		id, channelsJSON, sound, time.Now().UTC(),
	)
	return err
}

// Workspace holds the data returned by GetWorkspace.
type Workspace struct {
	Channels string
	Sound    string
}

// GetWorkspace returns the workspace data for a given ID.
func (s *Store) GetWorkspace(id string) (*Workspace, error) {
	var ws Workspace
	err := s.db.QueryRow(`SELECT channels, sound FROM workspaces WHERE id = ?`, id).Scan(&ws.Channels, &ws.Sound)
	if err != nil {
		return nil, err
	}
	return &ws, nil
}

// EventCount returns the total number of events in the database.
func (s *Store) EventCount() int64 {
	var count int64
	s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&count)
	return count
}

// Purge deletes events older than maxAge.
func (s *Store) Purge(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge)
	result, err := s.db.Exec(`DELETE FROM events WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DigestStats returns aggregate stats for the given channels since the given time.
func (s *Store) DigestStats(channels []string, since time.Time) (total int64, bySrc map[string]int64, top []event.Event, err error) {
	if len(channels) == 0 {
		return 0, nil, nil, nil
	}

	placeholders := make([]string, len(channels))
	args := make([]interface{}, len(channels))
	for i, ch := range channels {
		placeholders[i] = "?"
		args[i] = ch
	}
	inClause := strings.Join(placeholders, ",")

	// Total count
	var countArgs []interface{}
	countArgs = append(countArgs, args...)
	countArgs = append(countArgs, since.UTC())
	s.db.QueryRow(
		fmt.Sprintf(`SELECT COUNT(*) FROM events WHERE channel IN (%s) AND timestamp >= ?`, inClause),
		countArgs...,
	).Scan(&total)

	// By source
	bySrc = make(map[string]int64)
	var srcArgs []interface{}
	srcArgs = append(srcArgs, args...)
	srcArgs = append(srcArgs, since.UTC())
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT source, COUNT(*) FROM events WHERE channel IN (%s) AND timestamp >= ? GROUP BY source ORDER BY COUNT(*) DESC`, inClause),
		srcArgs...,
	)
	if err != nil {
		return total, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var cnt int64
		if rows.Scan(&src, &cnt) == nil {
			bySrc[src] = cnt
		}
	}

	// Top 10 events
	var topArgs []interface{}
	topArgs = append(topArgs, args...)
	topArgs = append(topArgs, since.UTC())
	rows2, err := s.db.Query(
		fmt.Sprintf(`SELECT id, channel, source, type, summary, raw_json, timestamp FROM events WHERE channel IN (%s) AND timestamp >= ? ORDER BY timestamp DESC LIMIT 10`, inClause),
		topArgs...,
	)
	if err != nil {
		return total, bySrc, nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var e event.Event
		if rows2.Scan(&e.ID, &e.Channel, &e.Source, &e.Type, &e.Summary, &e.RawJSON, &e.Timestamp) == nil {
			top = append(top, e)
		}
	}

	return total, bySrc, top, nil
}

// LastEventPerChannel returns the most recent event for each of the given channels.
func (s *Store) LastEventPerChannel(channels []string) (map[string]*event.Event, error) {
	if len(channels) == 0 {
		return nil, nil
	}

	result := make(map[string]*event.Event, len(channels))
	for _, ch := range channels {
		var e event.Event
		err := s.db.QueryRow(
			`SELECT id, channel, source, type, summary, raw_json, timestamp FROM events WHERE channel = ? ORDER BY timestamp DESC LIMIT 1`,
			ch,
		).Scan(&e.ID, &e.Channel, &e.Source, &e.Type, &e.Summary, &e.RawJSON, &e.Timestamp)
		if err == nil {
			result[ch] = &e
		}
	}
	return result, nil
}

// LiveStats returns platform-wide stats for the landing page.
type LiveStats struct {
	ActiveChannels int64    // channels with events in last hour
	EventsToday   int64    // events in last 24h
	TopSources    []string // unique sources seen, most common first
}

func (s *Store) LiveStats() LiveStats {
	var stats LiveStats

	oneHourAgo := time.Now().UTC().Add(-1 * time.Hour)
	s.db.QueryRow(`SELECT COUNT(DISTINCT channel) FROM events WHERE timestamp >= ?`, oneHourAgo).Scan(&stats.ActiveChannels)

	oneDayAgo := time.Now().UTC().Add(-24 * time.Hour)
	s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE timestamp >= ?`, oneDayAgo).Scan(&stats.EventsToday)

	rows, err := s.db.Query(`SELECT source, COUNT(*) as cnt FROM events WHERE timestamp >= ? GROUP BY source ORDER BY cnt DESC LIMIT 10`, oneDayAgo)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var src string
			var cnt int64
			if rows.Scan(&src, &cnt) == nil && src != "" {
				stats.TopSources = append(stats.TopSources, src)
			}
		}
	}

	return stats
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
