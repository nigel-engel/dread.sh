package store

const createTableSQL = `
CREATE TABLE IF NOT EXISTS events (
    id        TEXT PRIMARY KEY,
    channel   TEXT NOT NULL,
    source    TEXT NOT NULL,
    type      TEXT NOT NULL,
    summary   TEXT NOT NULL,
    raw_json  TEXT NOT NULL,
    timestamp DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_channel_timestamp ON events (channel, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_events_source ON events (source);

CREATE TABLE IF NOT EXISTS stats (
    key   TEXT PRIMARY KEY,
    count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS workspaces (
    id         TEXT PRIMARY KEY,
    channels   TEXT NOT NULL,
    sound      TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS unique_installs (
    ip         TEXT PRIMARY KEY,
    first_seen DATETIME NOT NULL,
    last_seen  DATETIME NOT NULL,
    count      INTEGER NOT NULL DEFAULT 1
);
`
