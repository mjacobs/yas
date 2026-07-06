-- Local SQLite replica schema (client side).
-- Times are stored as Unix milliseconds (INTEGER) for compact, sortable indexes.
-- Applied idempotently at agent startup.

CREATE TABLE IF NOT EXISTS records (
    id          TEXT    PRIMARY KEY,        -- UUIDv7, global dedup key
    command     TEXT    NOT NULL,
    cwd         TEXT,
    hostname    TEXT,
    session     TEXT,
    shell       TEXT,
    username    TEXT,
    exit_code   INTEGER,                    -- NULL until the command finishes
    start_time  INTEGER NOT NULL,           -- unix ms
    duration_ms INTEGER,                    -- NULL until the command finishes
    created_at  INTEGER NOT NULL,           -- unix ms
    deleted     INTEGER NOT NULL DEFAULT 0, -- tombstone (0/1)
    executor    TEXT,                       -- who/what ran it (NULL/'' = human)
    corr_id     TEXT,                       -- cross-tool correlation key (reserved)
    repo_root   TEXT,                       -- git repo root of cwd at capture (NULL off-repo/imported)
    branch      TEXT,                       -- git branch at capture (NULL off-repo/detached/imported)
    synced      INTEGER NOT NULL DEFAULT 0  -- 0 = not yet pushed to server
);

CREATE INDEX IF NOT EXISTS idx_records_start  ON records(start_time);
CREATE INDEX IF NOT EXISTS idx_records_synced ON records(synced) WHERE synced = 0;

-- Full-text search over the command (and cwd) via FTS5 external-content,
-- mirroring the records table by rowid.
CREATE VIRTUAL TABLE IF NOT EXISTS records_fts USING fts5(
    command,
    cwd,
    content='records',
    content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS records_ai AFTER INSERT ON records BEGIN
    INSERT INTO records_fts(rowid, command, cwd) VALUES (new.rowid, new.command, new.cwd);
END;
CREATE TRIGGER IF NOT EXISTS records_ad AFTER DELETE ON records BEGIN
    INSERT INTO records_fts(records_fts, rowid, command, cwd) VALUES ('delete', old.rowid, old.command, old.cwd);
END;
CREATE TRIGGER IF NOT EXISTS records_au AFTER UPDATE ON records BEGIN
    INSERT INTO records_fts(records_fts, rowid, command, cwd) VALUES ('delete', old.rowid, old.command, old.cwd);
    INSERT INTO records_fts(rowid, command, cwd) VALUES (new.rowid, new.command, new.cwd);
END;

-- Key/value sync state, e.g. ('last_pulled_seq', <n>).
CREATE TABLE IF NOT EXISTS sync_state (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);
