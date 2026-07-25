-- +goose Up
-- 0018: measured media info on library files (ADR 0013).
--
-- Until now a file's quality came from one place: parsing its name. When the
-- name said nothing -- the normal state of an adopted library -- nothing was
-- recorded, and "no quality recorded" was indistinguishable from "no file",
-- which is how a 17 GB file on disk got hunted as missing.
--
-- `quality` stays the canonical (source, resolution) string that every
-- decision reads. What is new is the row saying where that string came from,
-- how sure we are, and what the bytes actually contained.
--

-- The full measured record as JSON: container, video codec/dimensions/bit
-- depth/HDR/interlacing, audio codecs and channels, duration, bitrate. Read
-- by the UI for the facts pill and by inference for the source verdict.
-- Empty string = never probed (which is what every existing row is).
ALTER TABLE media_files ADD COLUMN media_info TEXT NOT NULL DEFAULT '';

-- Where `quality` came from: probe | filename | release | manual | failed.
-- Empty = pre-0018 row, provenance unknown -- treated as filename, since that
-- is the only thing that could have written it.
ALTER TABLE media_files ADD COLUMN quality_provenance TEXT NOT NULL DEFAULT '';

-- How sure the inferred source is: high | medium | low. Empty when the source
-- was not inferred (measured resolution with no source verdict, or a value a
-- human set). Resolution is never a guess once a probe succeeds, so this
-- confidence is about the source axis alone.
ALTER TABLE media_files ADD COLUMN quality_confidence TEXT NOT NULL DEFAULT '';

-- Unix millis of the last successful probe attempt; 0 = never probed. Paired
-- with `size` this is the cache key: a file whose size changed is a different
-- file wearing the same path, and gets re-probed.
ALTER TABLE media_files ADD COLUMN probed_at INTEGER NOT NULL DEFAULT 0;
