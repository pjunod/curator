-- +goose Up
-- Local accounts remain authoritative when every integration is disconnected.
CREATE TABLE household_accounts (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 username TEXT NOT NULL UNIQUE,
 display_name TEXT NOT NULL,
 password_hash TEXT NOT NULL,
 legacy_salt TEXT NOT NULL DEFAULT '',
 role TEXT NOT NULL CHECK(role IN('owner','member')),
 status TEXT NOT NULL DEFAULT 'active' CHECK(status IN('active','suspended')),
 credential_generation INTEGER NOT NULL DEFAULT 1,
 revision INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL,
 initial_owner INTEGER NOT NULL DEFAULT 0 CHECK(initial_owner IN(0,1))
) STRICT;
CREATE UNIQUE INDEX household_initial_owner ON household_accounts(initial_owner) WHERE initial_owner=1;
CREATE TABLE household_sessions (
 token_digest TEXT PRIMARY KEY,
 account_id INTEGER NOT NULL REFERENCES household_accounts(id),
 credential_generation INTEGER NOT NULL,
 method TEXT NOT NULL CHECK(method IN('local','cinema')),
 link_id INTEGER NOT NULL DEFAULT 0,
 link_generation INTEGER NOT NULL DEFAULT 0,
 device TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL
) STRICT;
CREATE INDEX household_session_account ON household_sessions(account_id);
CREATE INDEX household_session_expiry ON household_sessions(expires_at);
CREATE TABLE household_invites (
 token_digest TEXT PRIMARY KEY,
 created_by INTEGER NOT NULL REFERENCES household_accounts(id),
 created_generation INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,
 consumed_by INTEGER REFERENCES household_accounts(id)
) STRICT;
CREATE INDEX household_invite_expiry ON household_invites(expires_at);
CREATE TABLE household_security (
 id INTEGER PRIMARY KEY CHECK(id=1),
 established INTEGER NOT NULL DEFAULT 0 CHECK(established IN(0,1))
) STRICT;
INSERT INTO household_security VALUES(1,0);

-- +goose Down
DROP TABLE household_invites;
DROP TABLE household_sessions;
DROP TABLE household_security;
DROP INDEX household_initial_owner;
DROP TABLE household_accounts;
