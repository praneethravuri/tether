-- tether schema v1. Applied on every daemon start, every statement idempotent.
-- Times are Unix milliseconds (INTEGER).

CREATE TABLE IF NOT EXISTS agents (
    workspace     TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    harness       TEXT    NOT NULL DEFAULT 'unknown',
    session_id    TEXT    NOT NULL DEFAULT '',
    cwd           TEXT    NOT NULL,
    pid           INTEGER NOT NULL DEFAULT 0,
    pid_start     INTEGER NOT NULL DEFAULT 0,
    dropped       INTEGER NOT NULL DEFAULT 0,
    registered_at INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL,
    last_kind     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (workspace, name)
) STRICT;

CREATE TABLE IF NOT EXISTS messages (
    id           TEXT    PRIMARY KEY,
    thread_id    TEXT    NOT NULL,
    reply_to     TEXT    NOT NULL DEFAULT '',
    from_name    TEXT    NOT NULL,
    from_ws      TEXT    NOT NULL,
    to_name      TEXT    NOT NULL,
    to_ws        TEXT    NOT NULL,
    kind         TEXT    NOT NULL DEFAULT 'note'
                 CHECK (kind IN ('note','handoff','question','answer')),
    body         TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    delivered_at INTEGER,
    acked_at     INTEGER,
    dead         INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_inbox
    ON messages (to_ws, to_name, acked_at, id);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- A claim is a lease on a caller-supplied key (typically a file path) within
-- a workspace. owner_pid/owner_pid_start is the live process holding it --
-- self-healing, the same pairing agents use for presence; lease_id is
-- opaque and re-minted on every acquisition (see internal/store/claims.go);
-- lease_holder/leased_at are diagnostic labels only, never load-bearing.
CREATE TABLE IF NOT EXISTS claims (
    workspace       TEXT    NOT NULL,
    key             TEXT    NOT NULL,
    owner_pid       INTEGER NOT NULL DEFAULT 0,
    owner_pid_start INTEGER NOT NULL DEFAULT 0,
    lease_id        TEXT    NOT NULL,
    lease_holder    TEXT    NOT NULL DEFAULT '',
    leased_at       INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    PRIMARY KEY (workspace, key)
) STRICT;
