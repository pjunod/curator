package sqlite

import (
 "context"
 "encoding/json"
)

// CommittedImportPlacements supplies candidates, never authority to reuse bytes.
func(d *DB) CommittedImportPlacements(ctx context.Context,id int64,generation,source string)([]Placement,error){
 rows,err:=d.R.QueryContext(ctx,`SELECT data FROM import_placements WHERE state IN ('committed','cleaned') AND json_extract(data,'$.download_id')=? AND json_extract(data,'$.source_generation')=? AND json_extract(data,'$.source')=? ORDER BY updated_at DESC`,id,generation,source)
 if err!=nil{return nil,err};defer rows.Close()
 var out []Placement
 for rows.Next(){var raw string;var p Placement;if err=rows.Scan(&raw);err!=nil{return nil,err};if err=json.Unmarshal([]byte(raw),&p);err!=nil{return nil,err};out=append(out,p)}
 return out,rows.Err()
}

// SatisfyImportFile keeps the original receipt and logical file generation. A
// retry receives only a per-file result, never the old publication authority.
// Caller holds the target/storage decision locks and verifies current bytes.
func(d *DB) SatisfyImportFile(ctx context.Context,key ImportAttemptKey,p Placement)error{
 tx,err:=d.W.BeginTx(ctx,nil);if err!=nil{return err};defer tx.Rollback()
 a,err:=guardImportAttempt(ctx,tx,key);if err!=nil{return err};if a.Outcome!=""{return ErrImportCancelled}
 var raw string
 if err=tx.QueryRowContext(ctx,`SELECT data FROM import_placements WHERE id=? AND state IN ('committed','cleaned')`,p.ID).Scan(&raw);err!=nil{return err}
 var prior Placement;if err=json.Unmarshal([]byte(raw),&prior);err!=nil{return err}
 if prior.Source!=p.Source||prior.Target!=p.Target||prior.SHA256!=p.SHA256||prior.DownloadID!=key.DownloadID||prior.SourceGeneration!=key.GenerationKey||prior.PublishedFileGeneration!=p.PublishedFileGeneration{return ErrPlacementGenerationChanged}
 var owned int
 if err=tx.QueryRowContext(ctx,`SELECT count(*) FROM media_file_placements r JOIN media_files f ON f.id=r.media_file_id AND f.file_generation=r.file_generation WHERE r.placement_id=? AND r.path=? AND r.sha256=? AND r.source_generation=? AND r.source_transfer=? AND r.source_payload_generation=? AND r.source_file_generation=? AND f.path=? AND f.media_item_id=? AND COALESCE(f.copy_id,0)=?`,p.ID,p.Target,p.SHA256,p.SourceGeneration,p.SourceTransfer,p.SourcePayloadGeneration,p.SourceFileGeneration,p.Target,p.ItemID,p.CopyID).Scan(&owned);err!=nil{return err}
 if owned!=1{return ErrPlacementGenerationChanged}
 if err=resolveImportFile(ctx,tx,key,p.Source,"committed",p.ID);err!=nil{return err};return tx.Commit()
}
