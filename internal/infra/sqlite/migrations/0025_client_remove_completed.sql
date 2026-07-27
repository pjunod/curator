-- +goose Up
-- Clean up after ourselves.
--
-- Monarr never removed a payload from the download client after importing it.
-- Every grab left a second full copy behind — and because monarr falls back to
-- copying when the client's directory and the library are on different
-- filesystems (the normal arrangement: fast local disk for downloads, big
-- array for the library), that second copy is real bytes, not a hardlink.
-- A user found 923 GB of it.
--
-- The default is set per client TYPE rather than globally, because the right
-- answer genuinely differs:
--
--   * Usenet has no obligation after the download. The payload is scratch
--     space and deleting it is simply tidying up, so those default to ON.
--   * A torrent is still seeding. Deleting its data mid-seed breaks a ratio
--     the user may care about a great deal, and monarr cannot currently tell
--     "done seeding" from "seeding happily". Those default to OFF and stay a
--     deliberate choice.
ALTER TABLE download_clients ADD COLUMN remove_completed INTEGER NOT NULL DEFAULT 0;

UPDATE download_clients
SET remove_completed = 1
WHERE type IN ('sabnzbd', 'nzbget', 'nzbd');

-- Whether the payload behind an already-imported download has been cleaned up.
--
-- Separate from the download's state because they answer different questions:
-- `state` is how the transfer ended, this is whether its bytes are still
-- taking up room. It exists so the cleanup sweep is idempotent — without it
-- every pass would ask the client to delete every download it has ever seen.
--
-- Existing rows start at 0, which is what drains a backlog: turn the setting
-- on and the first sweep collects everything already imported.
ALTER TABLE downloads ADD COLUMN payload_removed INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE download_clients DROP COLUMN remove_completed;
ALTER TABLE downloads DROP COLUMN payload_removed;
