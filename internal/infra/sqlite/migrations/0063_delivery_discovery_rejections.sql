-- +goose Up
-- Invalid manifests are retained at Runner; this journal records why Curator
-- skipped their sequence without granting acceptance or cleanup authority.
CREATE TABLE runner_delivery_discovery_rejections (
 client_id INTEGER NOT NULL,
 sequence INTEGER NOT NULL,
 delivery_id TEXT NOT NULL,
 reason TEXT NOT NULL,
 updated_at INTEGER NOT NULL,
 PRIMARY KEY(client_id,sequence)
) STRICT;

-- +goose Down
DROP TABLE runner_delivery_discovery_rejections;
