package sqlite

// A browsing observation depends on identity, scope and file/episode ownership,
// not ratings, synopsis, monitoring or probe-quality updates. The broad lifecycle
// revisions still fence scan/verification writes made around filesystem IO.
// ORDER BY inside each input subquery makes equivalent facts serialize equally.
const presenceFingerprintSQL = `json_array(m.kind,m.tmdb_id,m.tvdb_id,m.imdb_id,m.olid,m.isbn13,m.asin,m.root_folder_id,m.path,m.offloaded_at,
 (SELECT json_group_array(json_array(id,COALESCE(copy_id,0),path,size,file_generation)) FROM (SELECT id,copy_id,path,size,file_generation FROM media_files WHERE media_item_id=m.id ORDER BY id)),
 (SELECT json_group_array(json_array(id,season_number,episode_number,air_date,offloaded_at)) FROM (SELECT id,season_number,episode_number,air_date,offloaded_at FROM episodes WHERE media_item_id=m.id ORDER BY id)),
 (SELECT json_group_array(json_array(media_file_id,episode_id)) FROM (SELECT fe.media_file_id,fe.episode_id FROM media_file_episodes fe JOIN media_files f ON f.id=fe.media_file_id WHERE f.media_item_id=m.id ORDER BY fe.media_file_id,fe.episode_id)),
 (SELECT json_group_array(json_array(id,COALESCE(root_folder_id,0),path,book_type)) FROM (SELECT id,root_folder_id,path,book_type FROM media_copies WHERE media_item_id=m.id ORDER BY id)),
 (SELECT json_group_array(json_array(number,offloaded_at)) FROM (SELECT number,offloaded_at FROM seasons WHERE media_item_id=m.id ORDER BY number)))`
