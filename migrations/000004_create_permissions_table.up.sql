-- Permissions, modelled as a classic many-to-many relationship.
--
--   users  <--->  users_permissions  <--->  permissions
--
-- `users_permissions` is the join table. Its PRIMARY KEY is the *pair* of
-- columns, which both gives us a fast lookup index and makes it impossible to
-- grant the same permission to the same user twice.

CREATE TABLE IF NOT EXISTS permissions (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    code TEXT    NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS users_permissions (
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    permission_id INTEGER NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, permission_id)
);

-- Seed the two permission codes the API uses. `OR IGNORE` makes this
-- re-runnable without blowing up on the UNIQUE constraint.
INSERT OR IGNORE INTO permissions (code) VALUES ('movies:read'), ('movies:write');
