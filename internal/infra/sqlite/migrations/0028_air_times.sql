-- +goose Up
-- ADR 0016. A show's broadcast slot, as TVmaze publishes it: local wall-clock
-- time ("21:00"), the network's IANA timezone, and a display name. Empty
-- means unknown -- streaming shows have no broadcast instant, and the
-- calendar shows those as date-only rather than guessing midnight.
ALTER TABLE media_items ADD COLUMN airs_time     TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN airs_timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN network       TEXT NOT NULL DEFAULT '';
