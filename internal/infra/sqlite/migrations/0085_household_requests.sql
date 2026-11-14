-- +goose Up
CREATE TABLE household_requests (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 fingerprint TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN('movie','series')),
 tmdb_id INTEGER NOT NULL CHECK(tmdb_id>0),
 identity_json TEXT NOT NULL CHECK(json_valid(identity_json)),
 scope_json TEXT NOT NULL CHECK(json_valid(scope_json)),
 title TEXT NOT NULL,
 year INTEGER NOT NULL DEFAULT 0,
 poster_path TEXT NOT NULL DEFAULT '',
 overview TEXT NOT NULL DEFAULT '',
 created_by INTEGER NOT NULL REFERENCES household_accounts(id),
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN('pending','approved','acquiring','fulfilled','declined','withdrawn','needs_review')),
 public_note TEXT NOT NULL DEFAULT '',
 approved_by INTEGER REFERENCES household_accounts(id),
 policy_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(policy_json)),
 library_item_id INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX household_request_schedule ON household_requests(state,id);
CREATE UNIQUE INDEX household_active_request ON household_requests(fingerprint) WHERE state IN('pending','approved','acquiring','needs_review');
CREATE TABLE household_request_subscriptions (
 request_id INTEGER NOT NULL REFERENCES household_requests(id),
 account_id INTEGER NOT NULL REFERENCES household_accounts(id),
 active INTEGER NOT NULL DEFAULT 1 CHECK(active IN(0,1)),
 created_at INTEGER NOT NULL,
 PRIMARY KEY(request_id,account_id)
) STRICT;
CREATE TABLE household_request_notes (
 request_id INTEGER NOT NULL REFERENCES household_requests(id),
 account_id INTEGER NOT NULL REFERENCES household_accounts(id),
 note TEXT NOT NULL,
 PRIMARY KEY(request_id,account_id)
) STRICT;
CREATE TABLE household_request_targets (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 request_id INTEGER NOT NULL REFERENCES household_requests(id),
 media_item_id INTEGER NOT NULL,
 episode_id INTEGER NOT NULL DEFAULT 0,
 season INTEGER NOT NULL DEFAULT 0,
 episode INTEGER NOT NULL DEFAULT 0,
 approval_generation INTEGER NOT NULL DEFAULT 1,
 offload_epoch INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'approved' CHECK(state IN('approved','admitted','fulfilled','needs_review')),
 next_check_at INTEGER NOT NULL DEFAULT 0,
 download_id INTEGER,
 download_generation TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 UNIQUE(request_id,season,episode)
) STRICT;
-- Leaf claims make a season/episode overlap contend for the same authority.
CREATE INDEX household_target_schedule ON household_request_targets(state,next_check_at,id);
CREATE TABLE household_scope_claims (
 kind TEXT NOT NULL,
 tmdb_id INTEGER NOT NULL,
 copy_id INTEGER NOT NULL CHECK(copy_id=0),
 season INTEGER NOT NULL,
 episode INTEGER NOT NULL,
 request_id INTEGER NOT NULL REFERENCES household_requests(id),
 PRIMARY KEY(kind,tmdb_id,copy_id,season,episode)
) STRICT;

-- Decision receipts retain the local actor and reviewed policy across re-review.
CREATE TABLE household_request_decisions (
 request_id INTEGER NOT NULL REFERENCES household_requests(id),
 revision INTEGER NOT NULL,
 account_id INTEGER NOT NULL REFERENCES household_accounts(id),
 decision TEXT NOT NULL CHECK(decision IN('approved','declined')),
 policy_json TEXT NOT NULL CHECK(json_valid(policy_json)),
 created_at INTEGER NOT NULL,
 PRIMARY KEY(request_id,revision)
) STRICT;

-- Origins survive queue retention and are keyed by immutable download generation.
CREATE TABLE household_download_origins (
 download_id INTEGER NOT NULL,
 generation TEXT NOT NULL,
 target_id INTEGER NOT NULL,
 approval_generation INTEGER NOT NULL,
 scope_json TEXT NOT NULL CHECK(json_valid(scope_json)),
 policy_json TEXT NOT NULL CHECK(json_valid(policy_json)),
 PRIMARY KEY(download_id,generation),
 UNIQUE(target_id,approval_generation)
) STRICT;

-- +goose StatementBegin
CREATE TRIGGER household_no_generic_replacement BEFORE INSERT ON replacement_grabs
WHEN EXISTS(SELECT 1 FROM household_download_origins h JOIN downloads d ON d.id=h.download_id AND d.generation_key=h.generation WHERE d.id=NEW.download_id)
BEGIN
 SELECT RAISE(IGNORE);
END;
-- +goose StatementEnd

CREATE TABLE household_offload_exclusions(operation_id TEXT NOT NULL,path TEXT NOT NULL,PRIMARY KEY(operation_id,path)) STRICT;

-- +goose Down
DROP TRIGGER household_no_generic_replacement;
DROP TABLE household_offload_exclusions;
DROP TABLE household_download_origins;
DROP TABLE household_request_decisions;
DROP TABLE household_scope_claims;
DROP TABLE household_request_targets;
DROP TABLE household_request_notes;
DROP TABLE household_request_subscriptions;
DROP INDEX household_active_request;
DROP TABLE household_requests;
