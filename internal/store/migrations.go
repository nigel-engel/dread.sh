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
`
