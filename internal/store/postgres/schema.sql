-- Server-side Postgres schema (source of record + cross-machine merge point).
-- Applied idempotently at server startup.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- A single sequence drives the sync cursor. Every insert AND every update bumps
-- a row's seq, so clients that pull "since <seq>" also receive finalized and
-- tombstoned records, not just brand-new ones.
CREATE SEQUENCE IF NOT EXISTS records_seq;

CREATE TABLE IF NOT EXISTS records (
    id          UUID        PRIMARY KEY,            -- client-generated, idempotent upsert key
    command     TEXT        NOT NULL,
    cwd         TEXT,
    hostname    TEXT,
    session     TEXT,
    shell       TEXT,
    username    TEXT,
    exit_code   INTEGER,                            -- NULL until the command finishes
    start_time  TIMESTAMPTZ NOT NULL,
    duration_ms BIGINT,                             -- NULL until the command finishes
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted     BOOLEAN     NOT NULL DEFAULT false, -- tombstone
    executor    TEXT,                               -- who/what ran it (NULL/'' = human)
    corr_id     TEXT,                               -- cross-tool correlation key (reserved)
    repo_root   TEXT,                               -- git repo root of cwd at capture (NULL off-repo/imported)
    branch      TEXT,                               -- git branch at capture (NULL off-repo/detached/imported)
    seq         BIGINT      NOT NULL DEFAULT nextval('records_seq'),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Additive migrations for databases created before these columns existed.
ALTER TABLE records ADD COLUMN IF NOT EXISTS executor  TEXT;
ALTER TABLE records ADD COLUMN IF NOT EXISTS corr_id   TEXT;
ALTER TABLE records ADD COLUMN IF NOT EXISTS repo_root TEXT;
ALTER TABLE records ADD COLUMN IF NOT EXISTS branch    TEXT;

-- Pull ordering / cursor lookups.
CREATE UNIQUE INDEX IF NOT EXISTS idx_records_seq ON records(seq);
-- Trigram index for fast substring/fuzzy command search (server query API, M6+).
CREATE INDEX IF NOT EXISTS idx_records_command_trgm ON records USING gin (command gin_trgm_ops);

-- Bump seq + updated_at on every update so changes re-pull.
CREATE OR REPLACE FUNCTION records_bump_seq() RETURNS trigger AS $$
BEGIN
    NEW.seq := nextval('records_seq');
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS records_bump_seq ON records;
CREATE TRIGGER records_bump_seq BEFORE UPDATE ON records
    FOR EACH ROW EXECUTE FUNCTION records_bump_seq();
