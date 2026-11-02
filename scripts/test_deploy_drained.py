import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('deploy_drained',Path(__file__).with_name('deploy-drained.py'))
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)

class Docker:
 def __init__(self):
  self.calls=[];self.running=True;self.current='old';self.policy='unless-stopped';self.label='v1';self.wait_timeout=False;self.zombie=False;self.lost_reply=False;self.drain_pending=False;self.daemon='daemon';self.generation='generation';self.aborts=0
 def __call__(self,args,**kw):
  self.calls.append(args)
  if args==['docker','context','show']:out='context'
  else:
   command=args[3:]
   if command[:2]==['info','--format']:out=self.daemon
   elif command[0]=='inspect':
    out=json.dumps([{'Id':command[1], 'Config':{'Labels':{'org.pjunod.curator.deployment-drain':self.label}},'HostConfig':{'RestartPolicy':{'Name':self.policy}},'State':{'Running':self.running,'Pid':123 if self.running or self.zombie else 0,'Status':'running' if self.running else 'exited','ExitCode':0,'FinishedAt':'2026-11-01T00:00:00Z'}}])
   elif command[0]=='compose':
    if command[3]=='ps':out=self.current
    elif command[3]=='build':out=''
    elif command[3]=='up':
     if self.running or self.zombie:raise AssertionError('replacement before actual exit')
     self.current='new';out=''
    else:raise AssertionError(command)
   elif command[0]=='exec':
    op=command[-1]
    if op=='abort':self.aborts+=1
    if self.lost_reply and op=='drain':self.lost_reply=False;raise subprocess.TimeoutExpired(args,20)
    out=json.dumps({'phase':'draining' if self.drain_pending else 'drained','generation':self.generation,'active':1 if self.drain_pending else 0,'elapsedSeconds':1,'work':[]})
   elif command[0]=='update':self.policy=command[1].split('=',1)[1];out=''
   elif command[0]=='kill':self.running=False;out=''
   elif command[0]=='wait':
    if self.wait_timeout:raise subprocess.TimeoutExpired(args,10)
    out='0'
   else:raise AssertionError(command)
  return subprocess.CompletedProcess(args,0,stdout=out,stderr='')

class DeploymentTests(unittest.TestCase):
 def setUp(self):
  self.directory=tempfile.TemporaryDirectory();self.addCleanup(self.directory.cleanup)
  self.receipt=Path(self.directory.name)/'receipt.json';self.compose=Path(self.directory.name)/'compose.yaml'
  self.docker=Docker();self.now=0
  self.deploy=module.Deployment(self.receipt,run=self.docker,clock=lambda:self.now,sleep=self.sleep)
 def sleep(self,_):self.now+=1
 def prepare(self):self.deploy.prepare(self.compose,'monarr',20,2)
 def replaced(self):return any('up' in c for c in self.docker.calls)
 def test_normal_deploy_checks_actual_exit_and_disables_restart_before_term(self):
  self.prepare();self.deploy.drain_and_replace();self.assertTrue(self.replaced())
  calls=self.docker.calls
  disable=next(i for i,c in enumerate(calls) if '--restart=no' in c);term=next(i for i,c in enumerate(calls) if '--signal=TERM' in c);wait=next(i for i,c in enumerate(calls) if 'wait' in c);up=next(i for i,c in enumerate(calls) if 'up' in c)
  self.assertLess(disable,term);self.assertLess(term,wait);self.assertLess(wait,up)
  self.assertEqual(json.loads(self.receipt.read_text())['phase'],'replaced');self.assertEqual(self.receipt.stat().st_mode&0o777,0o600)
  self.assertFalse(any('--force' in c or '--signal=KILL' in c for c in calls))
 def test_wait_timeout_and_zombie_inspection_never_replace(self):
  for mode in ('timeout','zombie'):
   with self.subTest(mode=mode):
    self.setUp();self.prepare();self.docker.wait_timeout=mode=='timeout';self.docker.zombie=mode=='zombie'
    with self.assertRaises((subprocess.TimeoutExpired,RuntimeError)):self.deploy.drain_and_replace()
    self.assertFalse(self.replaced());self.assertEqual(json.loads(self.receipt.read_text())['phase'],'term_requested')
 def test_lost_drain_reply_can_resume_same_owner(self):
  self.prepare();owner=self.deploy.record['ownerToken'];self.docker.lost_reply=True
  with self.assertRaises(subprocess.TimeoutExpired):self.deploy.drain_and_replace()
  self.deploy.load();self.deploy.drain_and_replace();self.assertEqual(self.deploy.record['ownerToken'],owner);self.assertTrue(self.replaced())
 def test_resume_recovers_crash_before_term_delivery(self):
  self.prepare();self.deploy.record['generation']=self.docker.generation;self.docker.policy='no';self.deploy.save('term_requested')
  self.deploy.load();self.deploy.drain_and_replace()
  self.assertTrue(self.replaced());self.assertTrue(any('--signal=TERM' in c for c in self.docker.calls))
 def test_old_image_is_refused_before_build_or_exec(self):
  self.docker.label=None
  with self.assertRaises(RuntimeError):self.prepare()
  self.assertFalse(any('exec' in c or 'build' in c or 'kill' in c for c in self.docker.calls))
 def test_drain_timeout_stays_paused_and_explicit_abort_restores_restart(self):
  self.prepare();self.docker.drain_pending=True
  with self.assertRaises(TimeoutError):self.deploy.drain_and_replace()
  self.assertFalse(self.replaced());self.assertFalse(any('kill' in c for c in self.docker.calls))
  self.docker.policy='no';self.deploy.abort();self.assertEqual(self.docker.policy,'unless-stopped');self.assertEqual(self.docker.aborts,1)
 def test_identity_change_and_terminal_abort_refused(self):
  self.prepare();self.docker.current='other'
  with self.assertRaises(RuntimeError):self.deploy.drain_and_replace()
  self.assertFalse(self.replaced());self.docker.current='old';self.deploy.record['phase']='term_requested'
  with self.assertRaises(RuntimeError):self.deploy.abort()
  self.assertEqual(self.docker.aborts,0)

if __name__=='__main__':unittest.main()
