package sqlite

import "context"

type ImportStageCustody struct{ID string `json:"id"`;Source string `json:"source"`;Target string `json:"target"`;Stage string `json:"stage"`;State string `json:"state"`}
func(d *DB)ImportStageCustody(ctx context.Context,id int64,generation string)([]ImportStageCustody,error){
 rows,err:=d.R.QueryContext(ctx,`SELECT 'stage:'||id,json_extract(data,'$.source'),target,json_extract(data,'$.temporary'),state FROM import_placements p WHERE json_extract(data,'$.download_id')=? AND json_extract(data,'$.source_generation')=? AND state IN ('prepared','retained') AND NOT EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.placement_id=p.id AND (a.outcome='pending' OR a.resolution IN ('retained_for_review','operator_adopted')))
 UNION ALL SELECT 'stagepath:'||stage,source,target,stage,state FROM import_attempt_stages s WHERE download_id=? AND generation_key=? AND state='held' AND NOT EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE json_extract(a.payload,'$.stage')=s.stage AND (a.outcome='pending' OR a.resolution IN ('retained_for_review','operator_adopted')))`,id,generation,id,generation)
 if err!=nil{return nil,err};defer rows.Close();out:=[]ImportStageCustody{}
 for rows.Next(){var v ImportStageCustody;if err=rows.Scan(&v.ID,&v.Source,&v.Target,&v.Stage,&v.State);err!=nil{return nil,err};out=append(out,v)};return out,rows.Err()
}
