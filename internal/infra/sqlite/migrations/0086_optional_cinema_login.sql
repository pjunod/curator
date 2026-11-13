-- +goose Up
ALTER TABLE household_sessions ADD COLUMN integration_grant TEXT NOT NULL DEFAULT '';
CREATE TABLE cinema_connection(id INTEGER PRIMARY KEY CHECK(id=1),base_url TEXT NOT NULL,issuer TEXT NOT NULL,client_id TEXT NOT NULL,client_secret TEXT NOT NULL,redirect TEXT NOT NULL,generation INTEGER NOT NULL DEFAULT 1) STRICT;
CREATE TABLE cinema_links(id INTEGER PRIMARY KEY AUTOINCREMENT,account_id INTEGER NOT NULL REFERENCES household_accounts(id),issuer TEXT NOT NULL,subject TEXT NOT NULL,connection_generation INTEGER NOT NULL,generation INTEGER NOT NULL DEFAULT 1,active INTEGER NOT NULL DEFAULT 1 CHECK(active IN(0,1))) STRICT;
CREATE UNIQUE INDEX cinema_active_subject ON cinema_links(issuer,subject) WHERE active=1;
CREATE UNIQUE INDEX cinema_active_account ON cinema_links(account_id) WHERE active=1;
CREATE TABLE cinema_handoffs(state_digest TEXT PRIMARY KEY,mode TEXT NOT NULL CHECK(mode IN('link','signin','native')),account_id INTEGER NOT NULL DEFAULT 0,credential_generation INTEGER NOT NULL DEFAULT 0,session_digest TEXT NOT NULL DEFAULT '',connection_generation INTEGER NOT NULL,verifier TEXT NOT NULL,cookie_digest TEXT NOT NULL DEFAULT '',native_challenge TEXT NOT NULL DEFAULT '',browser_opened INTEGER NOT NULL DEFAULT 0,claim TEXT NOT NULL DEFAULT '',grant_secret TEXT NOT NULL DEFAULT '',ready_link INTEGER NOT NULL DEFAULT 0,expires_at INTEGER NOT NULL) STRICT;
CREATE INDEX cinema_handoff_expiry ON cinema_handoffs(expires_at);
CREATE TABLE cinema_revocations(id INTEGER PRIMARY KEY AUTOINCREMENT,base_url TEXT NOT NULL,client_id TEXT NOT NULL,client_secret TEXT NOT NULL,grant_secret TEXT NOT NULL,created_at INTEGER NOT NULL) STRICT;
-- +goose Down
ALTER TABLE household_sessions DROP COLUMN integration_grant;
DROP TABLE cinema_revocations;
DROP TABLE cinema_handoffs;
DROP INDEX cinema_active_account;
DROP INDEX cinema_active_subject;
DROP TABLE cinema_links;
DROP TABLE cinema_connection;
