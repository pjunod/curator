import unittest
from analyze import completion_evidence,shape_evidence,reclassify

PREFIX='kworker/u64:2-3471342 [014] ..... 1741133.528501: '

def line(status=1048576,count=1048576,result=1048576,inode=1310723):
 return PREFIX+f'nfs_writeback_done: error={status} fileid=00:42:{inode} fhandle=0x218c455e offset=0 count={count} res={result} stable=UNSTABLE verifier=2b2dbd6a90ce4e21'

class AnalysisTests(unittest.TestCase):
 def record(self):return {'events':[{'phase':'target-created','device':66,'inode':1310723}]}
 def test_positive_zero_and_tail_success(self):
  reasons,count=completion_evidence([line(),line(status=0),line(status=151040,count=151040,result=151040)],self.record())
  self.assertEqual(reasons,[]);self.assertEqual(count,3)
 def test_failure_short_zero_oversized_and_malformed(self):
  for value in [line(status=-5),line(result=64),line(result=0),line(result=1048577),line().replace('res=1048576 ','')]:
   self.assertTrue(completion_evidence([value],self.record())[0],value)
 def test_no_own_completion_and_wrong_identity(self):
  for lines in [[],[line(inode=7)],[PREFIX+'nfs_writeback_done: error=0']]:self.assertTrue(completion_evidence(lines,self.record())[0])
 def test_actual_shape_required(self):
  self.assertEqual(shape_evidence([{'cgroup':{'memoryMax':'536870912'}}],'512MiB'),[])
  self.assertEqual(shape_evidence([{'cgroup':{'memoryMax':'max'}}],'unlimited'),[])
  for rows in [[],[{'cgroup':None}],[{'cgroup':{}}],[{'cgroup':{'memoryMax':'max'}}],[{'cgroup':{'memoryMax':'536870912'}},{'cgroup':{'memoryMax':'max'}}]]:
   self.assertTrue(shape_evidence(rows,'512MiB'))

 def test_other_exclusions_and_masked_foreign_writes_survive_replay(self):
  record=self.record();record.update({'comparisonIneligibilityReasons':['task NFS write returned an error','trace records lost','target mount changed or is missing'],'shape':'512MiB','size':1000000,'completedPolicyWork':True,'attribution':{'foreignWriteBytes':60000},'state':{'OOMKilled':False}})
  reasons,_=reclassify(record,[line()],[{'cgroup':{'memoryMax':'536870912'}}])
  self.assertEqual(reasons,['attributed foreign writes exceed 5% of own payload','target mount changed or is missing','trace records lost'])

if __name__=='__main__':unittest.main()
