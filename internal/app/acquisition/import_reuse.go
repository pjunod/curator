package acquisition

import (
 "context"
 "fmt"
 "os"
 "errors"
 "path/filepath"

 "github.com/pjunod/monarr/internal/domain"
 "github.com/pjunod/monarr/internal/infra/sqlite"
)

func(s *Service) reuseCommittedImport(ctx context.Context,dl sqlite.Download,item domain.MediaItem,scope importScope,src string)(placement,bool,error){
 key,bound:=importAttemptFrom(ctx);if !bound{return placement{},false,nil}
 candidates,err:=s.db.CommittedImportPlacements(ctx,dl.ID,dl.GenerationKey,src);if err!=nil{return placement{},false,err}
 for _,p:=range candidates {
  if p.ItemID!=item.ID||p.CopyID!=scope.CopyID||p.SourceTransfer!=scope.Transfer||p.SourcePayloadGeneration!=acceptedPublicationGeneration(ctx)||p.SourceFileGeneration!=acceptedSourceGeneration(ctx,src)||p.ReplacementOperationID!=scope.ReplacementOperationID||p.PublishedFileGeneration==""||!within(scope.Dest,p.Target){continue}
  digest,size,identity,err:=fileDigestSnapshot(ctx,p.Target);if err!=nil{return placement{},false,fmt.Errorf("verify prior import target: %w",err)}
  if digest!=p.SHA256||size!=p.Size||identity!=p.PublishedFileGeneration{return placement{},false,sqlite.ErrPlacementGenerationChanged}
  sourceDigest,sourceSize,err:=fileDigest(ctx,src)
  if errors.Is(err,os.ErrNotExist) && ctx.Value(deliveryAttemptPlanKey{})==true {
    if expected,ok:=ctx.Value(expectedSourceDigestsKey{}).(map[string]string);ok && expected[filepath.Clean(src)]==p.SHA256 {sourceDigest=p.SHA256;sourceSize=p.Size;err=nil}
  }
  if err!=nil{return placement{},false,err};if sourceDigest!=p.SHA256||sourceSize!=p.Size{return placement{},false,sqlite.ErrPlacementGenerationChanged}
  decision,release:=s.lockStorageDecision(ctx)
  info,err:=os.Lstat(p.Target)
  if err==nil&&deliveryFileGeneration(info)!=identity{err=sqlite.ErrPlacementGenerationChanged}
  if err==nil{err=s.db.SatisfyImportFile(decision,key,p)}
  release();if err!=nil{return placement{},false,importControlError(err)}
  files,err:=s.db.ListFilesForItem(ctx,item.ID);if err!=nil{return placement{},false,err}
  for _,f:=range files{if filepath.Clean(f.Path)==filepath.Clean(p.Target)&&f.CopyID==scope.CopyID{return placement{Path:p.Target,PlacementID:p.ID,FileGeneration:f.FileGeneration},true,nil}}
  return placement{},false,sqlite.ErrPlacementGenerationChanged
 }
 return placement{},false,nil
}
