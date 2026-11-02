#!/usr/bin/env python3
"""Qualify the real app in owned scratch Compose projects; never production.

Requires a completed prototype campaign and reviewed app/helper binaries. The
source/target directories are freshly and exclusively created. Each HTTP call
executes a loopback-only helper in the exact owned container, with no network.
Timeouts preserve the running writer and prevent the next scenario.
"""
import argparse
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import re
import threading
import sqlite3
import stat
import subprocess
import sys
import time
import uuid

from qualify import cgroup_snapshot,memory,mounts,production_imports


def command(*args,timeout=30,body=None):
 return subprocess.run(args,input=body,text=True,capture_output=True,timeout=timeout,check=True).stdout.strip()


def save(path,value):
 temporary=path.with_suffix('.new')
 with temporary.open('w') as out:
  os.fchmod(out.fileno(),0o600);json.dump(value,out,indent=2);out.flush();os.fsync(out.fileno())
 os.replace(temporary,path)


class Campaign:
 def __init__(self,args):
  self.root=Path(args.results).resolve();self.root.mkdir(mode=0o700,parents=True,exist_ok=False)
  self.id=uuid.uuid4().hex;self.args=args;self.followers=[]
  if not re.fullmatch(r'sha256:[0-9a-f]{64}',args.image):raise RuntimeError('immutable Docker image ID required')
  frozen=self.root/'frozen';frozen.mkdir(mode=0o700)
  artifacts={}
  for name,source in {'app':args.app,'helper':args.helper,'deploy.py':args.deploy,'qualify.py':str(Path(__file__).with_name('qualify.py')),'controller.py':str(Path(__file__)),'prototype-manifest.json':args.prototype_manifest,'build-receipt.json':args.build_receipt}.items():
   target=frozen/name;shutil.copyfile(source,target);target.chmod(0o500 if name in ('app','helper') else 0o400);artifacts[name]=hashlib.sha256(target.read_bytes()).hexdigest()
  self.artifacts=artifacts;self.frozen=frozen
  build=json.loads((frozen/'build-receipt.json').read_text())
  if build['appSHA256']!=artifacts['app'] or build['helperSHA256']!=artifacts['helper'] or build['imageId']!=args.image:raise RuntimeError('binary/image do not match build receipt')
  save(frozen/'provenance.json',{'artifactSHA256':artifacts,'build':build,'imageId':args.image})

  args.app=str(frozen/'app');args.helper=str(frozen/'helper');args.deploy=str(frozen/'deploy.py');args.prototype_manifest=str(frozen/'prototype-manifest.json')

  context=command('docker','context','show');endpoint=command('docker','context','inspect',context,'--format','{{.Endpoints.docker.Host}}')
  if not endpoint.startswith('unix://') or command('docker','info','--format','{{.Name}}').split('.')[0]!=os.uname().nodename.split('.')[0]:raise RuntimeError('local host Docker required')
  os.environ['DOCKER_CONTEXT']=context
  manifest=json.loads(Path(args.prototype_manifest).read_text())
  if manifest['phase']!='measurements_complete_not_policy_qualification':raise RuntimeError('prototype still owns its campaign; do not add interference')
  self.old_source=Path(manifest['source'])/'payload.bin';self.source=Path(args.source_parent)/('.codex-app-qual-'+self.id);self.target=Path(args.target_parent)/('.codex-app-qual-'+self.id)
  for p in (self.source,self.target):
   if os.statvfs(p.parent).f_bavail*os.statvfs(p.parent).f_frsize<40*1024**3:raise RuntimeError('less than 40GiB scratch space available')
   p.mkdir(mode=0o700,exist_ok=False);save(p/'owner.json',{'id':self.id,'results':str(self.root)})
  self.report={'id':self.id,'frozenSHA256':self.artifacts,'phase':'source_preparation','appSHA256':self.hash(Path(args.app)),'helperSHA256':self.hash(Path(args.helper)),'baseImage':args.image,'controllerSHA256':self.hash(Path(__file__)),'source':str(self.source),'target':str(self.target),'scenarios':[],'policy':'whole-file staging; measured-grace fallback; no candidate optimization is qualified by this run'}
  self.checkpoint()
  self.inputs={}
  for size in (1_400_000_000,10*1024**3):
   path=self.source/f'Qualification.{size}.2026.1080p.WEB-DL.mkv';h=hashlib.sha256();remaining=size
   with self.old_source.open('rb') as inp,path.open('xb') as out:
    while remaining:
     block=inp.read(min(4*1024**2,remaining))
     if not block:raise RuntimeError('prototype source truncated')
     out.write(block);h.update(block);remaining-=len(block)
    out.flush();os.fsync(out.fileno())
   if h.hexdigest()!=manifest['sourceSHA256'][str(size)]:raise RuntimeError('source digest differs from prototype receipt')
   path.chmod(0o400);self.inputs[size]=path.name
  self.report['inputs']={str(size):{'name':name,'sha256':manifest['sourceSHA256'][str(size)],'size':size,'device':(self.source/name).stat().st_dev,'inode':(self.source/name).stat().st_ino} for size,name in self.inputs.items()}
  self.report['phase']='application_measurements';self.checkpoint()
 def hash(self,path):
  h=hashlib.sha256()
  with path.open('rb') as inp:
   for block in iter(lambda:inp.read(1024**2),b''):h.update(block)
  return h.hexdigest()
 def checkpoint(self):save(self.root/'manifest.json',self.report)
 def inspect(self,cid):
  row=json.loads(command('docker','inspect',cid))[0]
  if row['Image']!=self.args.image or row['Id']!=cid or row['Config'].get('Labels',{}).get('com.pjunod.codex.app-qualification')!=self.id:raise RuntimeError('owned container identity changed')
  mounted={m['Destination']:m for m in row['Mounts']}
  for destination,source in (('/monarr',self.args.app),('/qual-http',self.args.helper)):
   if destination not in mounted or mounted[destination]['RW'] or Path(mounted[destination]['Source']).resolve()!=Path(source).resolve():raise RuntimeError('qualification binary mount differs from frozen provenance')
  return row
 def logs(self,record,cid):
  result=subprocess.run(['docker','logs',cid],text=True,capture_output=True,timeout=30,check=True)
  prefix=self.root/f'scenario-{record["index"]}'/cid[:12]
  Path(str(prefix)+'.stdout.log').write_text(result.stdout);Path(str(prefix)+'.stderr.log').write_text(result.stderr)
 def join_logs(self,cid):
  for follower in self.followers:
   if follower['cid']!=cid or follower.get('joined'):continue
   code=follower['process'].wait(timeout=20)
   follower['stdout'].close();follower['stderr'].close();follower['joined']=True
   if code!=0:raise RuntimeError('owned log collector did not finish cleanly')
 def interrupted_stage(self,record):
  databases=list((self.root/f'scenario-{record["index"]}'/'data').glob('*.db'))
  if len(databases)!=1:return {'confirmed':False,'reason':'local database identity ambiguous'}
  observations=[]
  with closing(sqlite3.connect('file:'+str(databases[0])+'?mode=ro',uri=True)) as db:
   for raw, in db.execute("SELECT data FROM import_placements WHERE state='prepared'"):
    placement=json.loads(raw);temporary=Path(placement['temporary']);relative=temporary.relative_to('/target');path=self.target/f'scenario-{record["index"]}'/relative
    try:
     info=path.lstat();partial=stat.S_ISREG(info.st_mode) and 0<info.st_size<placement['size']
     observations.append({'placement':placement['id'],'stage':str(path),'size':info.st_size,'expectedSize':placement['size'],'device':info.st_dev,'inode':info.st_ino,'partial':partial})
    except FileNotFoundError:observations.append({'placement':placement['id'],'stage':str(path),'missing':True})
  return {'confirmed':any(o.get('partial') for o in observations),'observations':observations}
 def api(self,record,method,path,body=None,expected=200):
  self.inspect(record['containerId'])
  if self.hash(Path(self.args.helper))!=self.artifacts['helper']:raise RuntimeError('frozen helper changed')
  result=json.loads(command('docker','exec','-i',record['containerId'],'/qual-http',method,'/api/v1'+path,body=json.dumps(body) if body is not None else '',timeout=12))
  record['maxAPISeconds']=max(record.get('maxAPISeconds',0),result['seconds'])
  with (self.root/f'scenario-{record["index"]}'/'api.jsonl').open('a') as out:out.write(json.dumps({'at':time.time(),'monotonic':time.monotonic(),'method':method,'path':path,'response':result})+'\n')
  allowed=expected if isinstance(expected,tuple) else (expected,)
  if result['status'] not in allowed:raise RuntimeError(f'{method} {path} returned {result["status"]}: {result["body"]}')
  return result['body']
 def create(self,record,restart=False):
  for name in ('app','helper','deploy.py'):
   if self.hash(self.frozen/name)!=self.artifacts[name]:raise RuntimeError('frozen artifact changed')
  scenario_dir=self.root/f'scenario-{record["index"]}';data=scenario_dir/'data';data.mkdir(parents=True,exist_ok=True)
  target=self.target/f'scenario-{record["index"]}';(target/'library').mkdir(parents=True,exist_ok=True)
  compose=scenario_dir/'compose.json'
  service={'image':self.args.image,'user':f'{os.getuid()}:{os.getgid()}','read_only':True,'network_mode':'none','restart':'no','labels':{'com.pjunod.codex.app-qualification':self.id,'org.pjunod.curator.deployment-drain':'v1'},'environment':{'MONARR_DATA_DIR':'/data','MONARR_PORT':'7676'},'volumes':[f'{data}:/data',f'{self.source}:/source:ro',f'{target}:/target',f'{Path(self.args.app).resolve()}:/monarr:ro',f'{Path(self.args.helper).resolve()}:/qual-http:ro'],'tmpfs':['/tmp:size=16m']}
  if record['shape']=='512MiB':service.update({'mem_limit':'512m','memswap_limit':'512m'})
  save(compose,{'name':f'codex-app-{self.id[:10]}-{record["index"]}','services':{'monarr':service}})
  record['compose']=str(compose)
  command('docker','compose','-f',str(compose),'up','-d','--no-build','monarr',timeout=120)
  cid=command('docker','compose','-f',str(compose),'ps','-q','monarr');record['containerId']=cid
  inspected=self.inspect(cid);record.setdefault('starts',[]).append({'containerId':cid,'state':inspected['State'],'memoryLimit':inspected['HostConfig']['Memory'],'cgroup':cgroup_snapshot(inspected['State']['Pid'])})
  expected='536870912' if record['shape']=='512MiB' else 'max'
  cgroup=record['starts'][-1]['cgroup']
  if not cgroup or cgroup['memoryMax']!=expected:raise RuntimeError('actual cgroup memory shape is unverified')
  logPrefix=scenario_dir/f'{cid[:12]}-start-{len(record["starts"])}'
  stdout=Path(str(logPrefix)+'.stdout.log').open('w');stderr=Path(str(logPrefix)+'.stderr.log').open('w')
  follower=subprocess.Popen(['docker','logs','--follow',cid],stdout=stdout,stderr=stderr)
  self.followers.append({'cid':cid,'process':follower,'stdout':stdout,'stderr':stderr})
  record.setdefault('logFollowers',[]).append({'containerId':cid,'pid':follower.pid,'prefix':str(logPrefix)})
  self.checkpoint()
  deadline=time.monotonic()+60
  while True:
   try:self.api(record,'GET','/system/status');break
   except subprocess.SubprocessError:
    if time.monotonic()>deadline:raise
    time.sleep(.25)
  if restart:return
  root=self.api(record,'POST','/rootfolders',{'kind':'movie','path':'/target/library'},201)
  record['rootFolder']=root
 def add_import(self,record,index,size):
  title=f'Qualification {record["index"]} {index}';folder=f'/target/library/{title}'
  # Only an exclusively owned scenario's library tree is changed here.
  (self.target/f'scenario-{record["index"]}'/'library'/title).mkdir(exist_ok=False)
  item=self.api(record,'POST','/library/manual',{'kind':'movie','title':title,'year':2026,'path':folder},201)
  source='/source/'+self.inputs[size]
  queued=self.api(record,'POST','/import/manual',{'path':source,'paths':[source],'mediaItemId':item['id']},202)
  return queued['downloadId'],item['id']
 def wait(self,record,predicate,seconds=900):
  deadline=time.monotonic()+seconds
  while time.monotonic()<deadline:
   state=self.inspect(record['containerId'])['State']
   if not state['Running']:record['unexpectedExit']=state;self.checkpoint();raise RuntimeError('application exited before scenario completion')
   rows=self.api(record,'GET','/queue')
   if predicate(rows):return rows
   time.sleep(.25)
  self.checkpoint();raise TimeoutError('actual work remains pending; retain writer and stop campaign')
 def stop(self,record):
  owner=secrets.token_hex(32);status=self.api(record,'POST','/system/deployment-drain',{'ownerToken':owner})
  deadline=time.monotonic()+900
  while status['phase']!='drained':
   if time.monotonic()>deadline:raise TimeoutError('drain still pending; writer retained')
   time.sleep(.25);status=self.api(record,'GET','/system/deployment-drain')
  cid=record['containerId'];start=time.monotonic();command('docker','kill','--signal=TERM',cid)
  state=self.require_exit(record,cid)
  record.setdefault('postDrainExitSeconds',[]).append(record['lastExitVerifiedMonotonic']-start);record['exit']=state;self.checkpoint()
 def sample(self,record,stop):
  path=self.root/f'scenario-{record["index"]}'/'environment.jsonl'
  while not stop.is_set():
   sample={'at':time.time(),'monotonic':time.monotonic()}
   try:
    inspected=self.inspect(record['containerId']);sample.update({'state':inspected['State'],'cgroup':cgroup_snapshot(inspected['State']['Pid']) if inspected['State']['Pid'] else None,'memory':memory(),'mounts':mounts(),'productionImportOperations':production_imports()})
   except Exception as error:sample['error']=str(error)
   with path.open('a') as out:out.write(json.dumps(sample)+'\n')
   stop.wait(.5)
 def copy_sample(self,record,ids):
  if time.monotonic()-record.get('_copyContextAt',0)>1:
   record['_copyRows']=self.wait(record,lambda rows:all(any(r['id']==i and r.get('importState')=='running' for r in rows) for i in ids))
   record['_copyInspected']=self.inspect(record['containerId']);record['_copyContextAt']=time.monotonic()
  rows=record['_copyRows']
  databases=list((self.root/f'scenario-{record["index"]}'/'data').glob('*.db'))
  if len(databases)!=1:raise RuntimeError('local database identity ambiguous')
  stages={}
  with closing(sqlite3.connect('file:'+str(databases[0])+'?mode=ro',uri=True)) as db:
   placements=db.execute("SELECT p.data FROM import_placements p JOIN import_attempt_controls c ON json_extract(p.data,'$.download_id')=c.download_id AND json_extract(p.data,'$.source_generation')=c.generation_key AND json_extract(p.data,'$.import_attempt_epoch')=c.attempt_epoch WHERE p.state='prepared' AND c.outcome=''").fetchall()
  inspected=record['_copyInspected'];state=inspected['State'];pid=state['Pid']
  proc=Path(f'/proc/{pid}');starttime=(proc/'stat').read_text().rsplit(')',1)[1].split()[19]
  identity={'container':record['containerId'],'pid':pid,'startedAt':state['StartedAt'],'procStarttime':starttime}
  for raw, in placements:
   p=json.loads(raw);identifier=p.get('download_id')
   row=next((r for r in rows if r['id']==identifier and identifier in ids),None)
   if row is None:continue
   control=row.get('importControl',{})
   if p.get('source_generation')!=control.get('generationKey') or str(p.get('import_attempt_epoch'))!=control.get('epoch'):continue
   relative=Path(p['temporary']).relative_to('/target')
   if '..' in relative.parts:raise RuntimeError('stage escaped exclusive target')
   for fd in (proc/'fd').iterdir():
    try:
     if os.readlink(fd)!=p['temporary']:continue
     rawInfo=(proc/'fdinfo'/fd.name).read_text();values=dict(line.split(':',1) for line in rawInfo.splitlines() if ':' in line)
     inode=int(values['ino']);position=int(values['pos']);flags=int(values['flags'].strip(),8);mount=int(values['mnt_id'])
     if flags&os.O_ACCMODE!=os.O_WRONLY or not 0<position<p['size']:continue
     if p.get('temporary_object_identity')!='linux-stable-object-v1:'+str(inode):continue
     again=dict(line.split(':',1) for line in (proc/'fdinfo'/fd.name).read_text().splitlines() if ':' in line)
     if os.readlink(fd)!=p['temporary'] or any(again[key]!=values[key] for key in ('ino','mnt_id','flags')):continue
     latest=int(again['pos'])
     if latest<position or latest>=p['size']:continue
     position=latest
    except FileNotFoundError:continue
    if identifier in stages:raise RuntimeError('multiple active writer descriptors for one download')
    stages[identifier]={'path':p['temporary'],'fd':fd.name,'inode':inode,'mount':mount,'flags':flags,'bytes':position,'expected':p['size'],'placement':p['id'],'epoch':control['epoch'],'generation':control['generationKey']}
  if (proc/'stat').read_text().rsplit(')',1)[1].split()[19]!=starttime:raise RuntimeError('application PID reused')
  sample={'at':time.monotonic(),'queue':rows,'stages':stages,'process':identity,'meaning':'userspace writable descriptor progress; not NAS durability'}
  with (self.root/f'scenario-{record["index"]}'/'copy-samples.jsonl').open('a') as out:out.write(json.dumps(sample)+'\n')
  return sample
 def observe_copy(self,record,ids):
  deadline=time.monotonic()+900
  while time.monotonic()<deadline:
   first=self.copy_sample(record,ids);time.sleep(.01);second=self.copy_sample(record,ids)
   a,b=first['stages'],second['stages']
   if first['process']==second['process'] and len(a)==len(ids) and len(b)==len(ids) and all(b[i]['bytes']>a[i]['bytes'] and all(a[i][key]==b[i][key] for key in ('path','fd','inode','mount','flags','placement','epoch','generation','expected')) for i in ids):
    record.setdefault('copyObservations',[]).append({'firstAt':first['at'],'first':first,'secondAt':second['at'],'second':second});self.checkpoint();return second['queue']
  raise TimeoutError('advancing copy phase not observed; no phase qualification')
 def require_exit(self,record,cid,timeout=600):
  code=int(command('docker','wait',cid,timeout=timeout));state=self.inspect(cid)['State'];record['lastExitVerifiedMonotonic']=time.monotonic()
  self.join_logs(cid);self.logs(record,cid)
  if state['Running'] or state['Pid'] or state['Status']!='exited' or state['ExitCode']!=code:raise RuntimeError('actual exit not established')
  if code!=0 or state.get('OOMKilled'):record['unexpectedExit']=state;self.checkpoint();raise RuntimeError('nonzero/OOM exit is failed qualification evidence')
  return state
 def verify_library(self,record,itemIDs,size):
  expected=self.hash(self.source/self.inputs[size]);evidence=[]
  for itemID in itemIDs:
   item=self.api(record,'GET',f'/library/{itemID}');files=item.get('files',[])
   if len(files)!=1:raise RuntimeError('completed item does not have one expected file')
   path=Path(files[0]['path']);relative=path.relative_to('/target');physical=self.target/f'scenario-{record["index"]}'/relative
   if physical.is_symlink() or physical.stat().st_size!=size or self.hash(physical)!=expected:raise RuntimeError('final library bytes do not match input')
   evidence.append({'itemId':itemID,'path':str(physical),'size':size,'sha256':expected})
  record['libraryByteEvidence']=evidence;self.checkpoint()
 def scenario(self,shape,kind):
  record={'index':len(self.report['scenarios']),'shape':shape,'kind':kind,'phase':'starting'};self.report['scenarios'].append(record);self.checkpoint()
  self.create(record)
  sampleStop=threading.Event();sampler=threading.Thread(target=self.sample,args=(record,sampleStop),daemon=True);sampler.start();self.sampling=(sampleStop,sampler)
  count=2 if kind=='two-writer' else 1;size=10*1024**3 if kind=='cancel-restart' else 1_400_000_000
  pairs=[self.add_import(record,i,size) for i in range(count)];ids=[pair[0] for pair in pairs];itemIDs=[pair[1] for pair in pairs];record['downloads']=ids
  rows=self.observe_copy(record,ids)
  record['actualRunningWriters']=count;self.checkpoint()
  if kind=='direct-term':
   cid=record['containerId'];started=time.monotonic();record['preSignalObservationGapSeconds']=started-record['copyObservations'][-1]['secondAt'];command('docker','kill','--signal=TERM',cid);state=self.require_exit(record,cid)
   record['interruptedCopyEvidence']=self.interrupted_stage(record)
   record['signalToActualExitSeconds']=record['lastExitVerifiedMonotonic']-started;record['signalCopyObservation']=rows;record['exit']=state
  elif kind=='cancel-restart':
   row=next(r for r in rows if r['id']==ids[0]);identity={k:row['importControl'][k] for k in ('generationKey','epoch')}
   result=self.api(record,'POST',f'/queue/{ids[0]}/cancel-import',identity,(202,204));record['cancellationResponse']=result
   self.wait(record,lambda rows:any(r['id']==ids[0] and r.get('importControl',{}).get('outcome')=='cancelled' and not r.get('importState') for r in rows))
   review=self.api(record,'GET',f'/queue/{ids[0]}/import-review');record['cancelledReview']=review
   self.stop(record);self.create(record,restart=True);time.sleep(5)
   rows=self.api(record,'GET','/queue');row=next(r for r in rows if r['id']==ids[0])
   if row['importControl']['epoch']!=identity['epoch'] or row['importControl']['outcome']!='cancelled' or row.get('importState'):raise RuntimeError('restart lost durable cancellation')
   record['restartSuppressionVerified']=True;self.stop(record)
  else:
   owner=secrets.token_hex(32);status=self.api(record,'POST','/system/deployment-drain',{'ownerToken':owner})
   record['initialDrain']=status
   self.api(record,'POST','/import/manual',{'path':'/source/forbidden','mediaItemId':1},503)
   self.api(record,'DELETE','/system/deployment-drain',{'ownerToken':owner,'generation':status['generation']})
   record['resumeVerified']=True
   receipt=self.root/f'deployment-{record["index"]}.json'
   # This exercises the actual exact-ID drain/TERM/wait/replacement command.
   command(sys.executable,self.args.deploy,'deploy','--compose-file',record['compose'],'--receipt',str(receipt),'--stop-wait','600',timeout=1200)
   deployment=json.loads(receipt.read_text());record['deployment']=deployment
   timing=deployment.get('termToVerifiedExitSeconds')
   if timing is None or timing<0:raise RuntimeError('full monotonic deployment exit timing unavailable')
   record.setdefault('postDrainExitUpperBoundSeconds',[]).append(timing)
   if deployment['exit']['ExitCode']!=0 or deployment['exit'].get('OOMKilled'):raise RuntimeError('deployment exit was nonzero/OOM')
   old=record['containerId'];self.join_logs(old);record['oldContainerId']=old;record['containerId']=deployment['replacementId'];self.inspect(record['containerId'])
   rows=self.api(record,'GET','/queue')
   if not all(any(r['id']==i and r['state']=='imported' for r in rows) for i in ids):raise RuntimeError('drain returned before imports completed')
   record['actualReplacementAfterExitVerified']=True;self.verify_library(record,itemIDs,size)
   # Capture replacement logs before its final graceful stop too.
   prefix=self.root/f'scenario-{record["index"]}'/'replacement'
   out=Path(str(prefix)+'.stdout.log').open('w');err=Path(str(prefix)+'.stderr.log').open('w');follower=subprocess.Popen(['docker','logs','--follow',record['containerId']],stdout=out,stderr=err)
   self.followers.append({'cid':record['containerId'],'process':follower,'stdout':out,'stderr':err});record.setdefault('logFollowers',[]).append({'containerId':record['containerId'],'pid':follower.pid,'prefix':str(prefix)})
   self.stop(record)
  sampleStop.set();sampler.join(timeout=15)
  if sampler.is_alive():raise RuntimeError('sampler still owns metadata work; stop campaign')
  record['phase']='completed';self.checkpoint()
 def run(self):
  for shape in ('unlimited','512MiB'):
   for kind in ('drain-resume','cancel-restart','two-writer','direct-term'):
    try:self.scenario(shape,kind)
    except Exception as error:
     if hasattr(self,'sampling'):
      self.sampling[0].set();self.sampling[1].join(timeout=15)
      if self.sampling[1].is_alive():raise RuntimeError('sampler still owns work; campaign pending')
     record=self.report['scenarios'][-1];record['error']=str(error);record['phase']='failed';self.checkpoint()
     if self.inspect(record['containerId'])['State']['Running']:
      self.report['phase']='pending_actual_work_preserved';self.checkpoint();raise
     # Even a failed shape cannot release custody on an observed leader exit.
     # Prove Docker wait + stopped PID before another scenario may start.
     cid=record['containerId'];code=int(command('docker','wait',cid,timeout=600));state=self.inspect(cid)['State']
     if state['Running'] or state['Pid'] or state['Status']!='exited' or state['ExitCode']!=code:raise RuntimeError('failed writer exit not proved')
     record['failedActualExit']=state
     self.join_logs(cid);self.logs(record,cid)
  self.report['phase']='application_measurements_complete';self.checkpoint()


if __name__=='__main__':
 p=argparse.ArgumentParser(description=__doc__)
 for name in ('results','prototype-manifest','source-parent','target-parent','app','helper','image','deploy','build-receipt'):p.add_argument('--'+name,required=True)
 args=p.parse_args();campaign=Campaign(args);campaign.run()
