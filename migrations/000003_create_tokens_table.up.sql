-- Stateful authentication / activation tokens.
--
-- We store only the SHA-256 *hash* of each token, never the plaintext. If the
-- database leaks, the attacker still can't authenticate, because they can't
-- reverse the hash back into the bearer token the client holds.
--
-- POSTGRES → SQLITE NOTES
--
--  * `ON DELETE CASCADE` works in SQLite, but only when foreign-key
--    enforcement is switched on — and unlike Postgres, SQLite has it OFF by
--    default, per connection. We enable it via `_pragma=foreign_keys(1)` in
--    the DSN (see internal/db/db.go). Forgetting that pragma is one of the
--    classic SQLite footguns: your FKs silently do nothing.

CREATE TABLE IF NOT EXISTS tokens (
    hash    BLOB      PRIMARY KEY,
    user_id INTEGER   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expiry  TIMESTAMP NOT NULL,
    scope   TEXT      NOT NULL
);
