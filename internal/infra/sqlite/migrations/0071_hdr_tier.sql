-- +goose Up
-- ADR 0026: the measured HDR tier of every file (hdr.Tier: dv-p7-fel,
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

-- +goose Down
-- Derived from media_info, which stays; nothing older reads the column.
ALTER TABLE media_files DROP COLUMN hdr_tier;
