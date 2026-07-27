-- Indexes to support the listing endpoint's sorting and the token lookups.
--
-- POSTGRES → SQLITE NOTE — FULL-TEXT SEARCH
--
-- The book creates GIN indexes over `to_tsvector('simple', title)` and
-- `genres`, giving real full-text search. SQLite's equivalent is the FTS5
-- extension, which needs a separate virtual table kept in sync by triggers.
--
-- To keep the moving parts down, this project searches titles with a plain
-- case-insensitive LIKE instead (see internal/data/movies.go). That is a table
-- scan and won't use the index below for a leading-wildcard match — which is
-- completely fine for a few thousand movies, and honest about the trade-off.
-- The README has a worked example of upgrading to FTS5 if you want it.
--
-- The title index below IS used for `ORDER BY title`, which is a real win.

CREATE INDEX IF NOT EXISTS idx_movies_title   ON movies (title COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_movies_year    ON movies (year);
CREATE INDEX IF NOT EXISTS idx_movies_runtime ON movies (runtime);

-- Tokens are looked up and deleted by user_id + scope, so index that pair.
CREATE INDEX IF NOT EXISTS idx_tokens_user_scope ON tokens (user_id, scope);
