-- POSTGRES → SQLITE NOTES
--
--  * The book uses the `citext` extension for case-insensitive email storage,
--    so that Alice@example.com and alice@example.com are the same user.
--    SQLite has no extensions to install — it has *collations*, and
--    `COLLATE NOCASE` on the column gives us the same behaviour for both the
--    UNIQUE index and for `WHERE email = ?` lookups.
--
--    (Caveat worth knowing: NOCASE only folds ASCII A–Z. That's fine for
--    practically all email addresses, but it is not full Unicode case folding.)
--
--  * `bytea` becomes `BLOB` for the bcrypt hash. We store the hash as raw bytes
--    rather than a string so there's no chance of an encoding round-trip
--    mangling it.
--
--  * `bool` becomes `BOOLEAN`. SQLite stores it as 0/1 in an INTEGER, but
--    declaring the type as BOOLEAN lets the driver hand us a real Go bool.

CREATE TABLE IF NOT EXISTS users (
    id            INTEGER   PRIMARY KEY AUTOINCREMENT,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    name          TEXT      NOT NULL,
    email         TEXT      NOT NULL UNIQUE COLLATE NOCASE,
    password_hash BLOB      NOT NULL,
    activated     BOOLEAN   NOT NULL DEFAULT 0,
    version       INTEGER   NOT NULL DEFAULT 1
);
