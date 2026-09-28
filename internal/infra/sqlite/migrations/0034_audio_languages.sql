-- +goose Up
-- ADR 0022: the declared language of every audio track, '+'-joined, an
-- untagged track as 'und', empty when the file was never probed or has no
-- audio. Derived from media_info at every probe write so the list view can
-- grade language without walking JSON per file on every request.
ALTER TABLE media_files ADD COLUMN audio_languages TEXT NOT NULL DEFAULT '';
UPDATE media_files
   SET audio_languages = COALESCE((
         SELECT group_concat(
                  CASE WHEN json_type(a.value) = 'object'
                       THEN lower(COALESCE(NULLIF(json_extract(a.value, '$.language'), ''), 'und'))
                       ELSE 'und' END, '+')
           FROM json_each(media_files.media_info, '$.audio') a), '')
 WHERE media_info != ''
   AND json_valid(media_info)
   AND json_type(media_info, '$.audio') = 'array';
