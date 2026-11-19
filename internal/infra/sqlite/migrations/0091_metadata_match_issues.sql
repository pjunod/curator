-- +goose Up
CREATE TABLE metadata_match_issues (
    item_id INTEGER PRIMARY KEY REFERENCES media_items(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_id INTEGER NOT NULL
);
CREATE TABLE activity_acknowledgments (
    download_id INTEGER NOT NULL REFERENCES downloads(id) ON DELETE CASCADE,
    generation_key TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    state TEXT NOT NULL,
    PRIMARY KEY(download_id,generation_key)
);
-- +goose Down
DROP TABLE activity_acknowledgments;
DROP TABLE metadata_match_issues;
