-- The movies table is the core resource of the API.
--
-- POSTGRES → SQLITE NOTES
--
--  * `bigserial PRIMARY KEY` becomes `INTEGER PRIMARY KEY AUTOINCREMENT`.
--    In SQLite, a column declared exactly `INTEGER PRIMARY KEY` is an alias for
--    the internal rowid, and auto-assigns. Adding AUTOINCREMENT additionally
--    guarantees ids are never *reused* after a delete — which matters for a
--    public API, because otherwise a newly created movie could silently inherit
--    the URL of a deleted one.
--
--  * `timestamp(0) with time zone` becomes `TIMESTAMP`. SQLite has no dedicated
--    date type; it stores dates as TEXT/INTEGER/REAL. We rely on the *declared*
--    type being TIMESTAMP, because the modernc.org/sqlite driver inspects the
--    declared type and automatically converts such columns to/from Go's
--    time.Time. CURRENT_TIMESTAMP produces UTC.
--
--  * `text[]` (a native Postgres array) becomes `TEXT` holding a JSON array.
--    See internal/data/genres.go for the Valuer/Scanner that does the encoding.
--
--  * The book adds CHECK constraints in a second migration with
--    `ALTER TABLE ... ADD CONSTRAINT`. SQLite's ALTER TABLE cannot add
--    constraints to an existing table, so we declare them inline here instead.
--    (These are a safety net; the real user-facing validation lives in
--    internal/data/movies.go so we can return friendly 422 messages.)

CREATE TABLE IF NOT EXISTS movies (
    id         INTEGER   PRIMARY KEY AUTOINCREMENT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    title      TEXT      NOT NULL CHECK (length(title) > 0 AND length(title) <= 500),
    year       INTEGER   NOT NULL CHECK (year BETWEEN 1888 AND 2999),
    runtime    INTEGER   NOT NULL CHECK (runtime > 0),
    genres     TEXT      NOT NULL DEFAULT '[]'
                         CHECK (json_valid(genres) AND json_array_length(genres) BETWEEN 1 AND 5),
    version    INTEGER   NOT NULL DEFAULT 1
);
