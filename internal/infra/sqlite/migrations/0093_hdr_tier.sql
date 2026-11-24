-- +goose Up
-- ADR 0031: the measured HDR tier of every file (hdr.Tier: dv-p7-fel,
-- dv-p7-mel, dv-p7, dv-p8, dv-p5, dv, hdr10plus, hdr10, hlg, sdr), empty when
-- the file was never measured. Written beside media_info at every probe by
-- mediainfo.Info.HDRTier, so the list view can grade HDR without walking JSON
-- per file on every request — the same reason audio_languages exists (0034).
--
-- The backfill applies Info.HDRTier to the records already stored. None of
-- them has a Dolby Vision record yet (that is what prober revision 2 adds),
-- so Dolby Vision backfills as "dv", profile unknown; the next scan re-probes
-- those HEVC files and the probe write replaces it with the exact tier.
ALTER TABLE media_files ADD COLUMN hdr_tier TEXT NOT NULL DEFAULT '';
UPDATE media_files
   SET hdr_tier = CASE
         WHEN EXISTS (SELECT 1 FROM json_each(media_files.media_info, '$.video.hdr') h WHERE h.value = 'dv') THEN 'dv'
         WHEN EXISTS (SELECT 1 FROM json_each(media_files.media_info, '$.video.hdr') h WHERE h.value = 'hdr10plus') THEN 'hdr10plus'
         WHEN EXISTS (SELECT 1 FROM json_each(media_files.media_info, '$.video.hdr') h WHERE h.value = 'hdr10') THEN 'hdr10'
         WHEN EXISTS (SELECT 1 FROM json_each(media_files.media_info, '$.video.hdr') h WHERE h.value = 'hlg') THEN 'hlg'
         ELSE 'sdr' END
 WHERE media_info != ''
   AND json_valid(media_info)
   AND json_type(media_info, '$.video') = 'object'
   AND COALESCE(json_extract(media_info, '$.video.width'), 0) > 0
   AND COALESCE(json_extract(media_info, '$.video.height'), 0) > 0
   AND quality_provenance != 'failed';

-- The title's disc master, as far as measurement has shown it: 'fel' once
-- any Dolby Vision profile 7 file or pre-flight probe of this title measured
-- a full enhancement layer, 'mel' while only minimal ones were seen, empty
-- before any. FEL never reverts: a MEL remux of a title known to be FEL on
-- disc is a different master, not evidence the disc changed (ADR 0031 §6).
ALTER TABLE media_items ADD COLUMN disc_el TEXT NOT NULL DEFAULT '';

-- +goose Down
-- Derived from media_info, which stays; nothing older reads the column.
ALTER TABLE media_files DROP COLUMN hdr_tier;
-- Derived from measurements, which re-derive it on the next probe.
ALTER TABLE media_items DROP COLUMN disc_el;
