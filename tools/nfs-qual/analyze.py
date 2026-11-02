#!/usr/bin/env python3
"""Revision 2 analysis of finalized, immutable campaign trial evidence.

Preserves the frozen controller's manifest and original decisions. The kernel's
nfs_writeback_done error field is signed task->tk_status: nonnegative means
success, including positive byte counts. This revision adds explicit completion
and actual cgroup shape checks without weakening any other original exclusion.
"""
import argparse
import collections
import hashlib
import json
import os
from pathlib import Path
import re

REVISION = 'nfs-analysis-2'
SOURCES = [
 'https://github.com/torvalds/linux/blob/v7.0/fs/nfs/nfstrace.h#L1343',
 'https://github.com/torvalds/linux/blob/v7.0/fs/nfs/write.c#L1385',
 'https://github.com/torvalds/linux/blob/v7.0/fs/nfs/write.c#L1447',
]


def completion_evidence(lines,record):
    created=next((e for e in record['events'] if e['phase']=='target-created'),None)
    if not created:return ['completion object identity missing'],0
    reasons=[];own=0
    identity=re.compile(r'fileid=([0-9a-f]+):([0-9a-f]+):(\d+)')
    fields=re.compile(r'nfs_writeback_done: error=(-?\d+) fileid=([0-9a-f]+):([0-9a-f]+):(\d+) fhandle=0x[0-9a-f]+ offset=(\d+) count=(\d+) res=(\d+) stable=\S+ verifier=\S+')
    for line in lines:
        if 'nfs_writeback_done:' not in line:continue
        match=identity.search(line)
        if not match:
            reasons.append('malformed completion identity');continue
        major,minor,inode=match.groups()
        if os.makedev(int(major,16),int(minor,16))!=created['device'] or int(inode)!=created['inode']:continue
        match=fields.search(line)
        if not match:
            reasons.append('malformed own write completion');continue
        own+=1
        status,count,result=int(match[1]),int(match[6]),int(match[7])
        if status<0:reasons.append('task NFS write returned an error')
        if count<=0 or result<=0 or result>count:reasons.append('invalid or zero-progress own write result')
        elif result<count:reasons.append('short/retried own write outside clean class')
    if not own:reasons.append('own write completion evidence missing')
    return sorted(set(reasons)),own


def shape_evidence(samples,shape):
    expected={'512MiB':'536870912','unlimited':'max'}.get(shape)
    usable=[s['cgroup'].get('memoryMax') for s in samples if s.get('cgroup')]
    if not expected or not usable or any(v is None for v in usable):return ['actual memory.max evidence missing']
    if any(str(v)!=expected for v in usable):return ['actual memory.max differs from declared shape']
    return []


def reclassify(record,lines,samples):
    reasons=[r for r in record['comparisonIneligibilityReasons'] if r!='task NFS write returned an error']
    completion,count=completion_evidence(lines,record);reasons+=completion
    reasons+=shape_evidence(samples,record['shape'])
    if record.get('attribution',{}).get('foreignWriteBytes',0)/max(1,record['size'])>0.05:reasons.append('attributed foreign writes exceed 5% of own payload')
    if not record.get('completedPolicyWork'):reasons.append('workload did not durably complete')
    if record.get('state',{}).get('OOMKilled'):reasons.append('container OOM killed')
    return sorted(set(reasons)),count


def digest(path):return hashlib.sha256(path.read_bytes()).hexdigest()


def analyze(root,output):
    root=Path(root);manifest_path=root/'manifest.json'
    manifest_raw=manifest_path.read_bytes();manifest=json.loads(manifest_raw)
    results=[];inputs={'manifest.json':hashlib.sha256(manifest_raw).hexdigest()}
    for record in manifest['trials']:
        state=record.get('state',{})
        if not all(k in record for k in ('events','comparisonIneligibilityReasons','completedPolicyWork','attribution','comparisonEligible')) or record.get('phase') not in ('cleaned','exit_verified') or state.get('Running') is not False or state.get('Pid')!=0 or state.get('Status')!='exited' or not record.get('traceDone'):continue
        i=record['index'];paths=[root/f'trial-{i}{suffix}' for suffix in ('.trace','.events.jsonl','.samples.jsonl','.trace.ready.json','.trace.done.json')]
        if not all(p.is_file() for p in paths):raise RuntimeError(f'finalized trial {i} evidence is missing')
        hashes={p.name:digest(p) for p in paths}
        lines=paths[0].read_text().splitlines();samples=[json.loads(l) for l in paths[2].read_text().splitlines()]
        # Remove only the specifically incorrect status reason. Keep trace
        # loss, temporal, identity, mount, outage and workload exclusions.
        original=record['comparisonIneligibilityReasons']
        reasons,count=reclassify(record,lines,samples)
        for p in paths:
            if digest(p)!=hashes[p.name]:raise RuntimeError(f'trial {i} changed during analysis')
        inputs.update(hashes)
        results.append({'index':i,'policy':record['policy'],'window':record['window'],'shape':record['shape'],'size':record['size'],'round':record['round'],'stop':record.get('stop'),'phaseCoverageValid':record.get('phaseCoverageValid'),
          'originalEligible':record.get('comparisonEligible'),'originalReasons':dict(collections.Counter(original)),
          'comparisonEligible':not reasons,'reasons':sorted(set(reasons)),'ownCompletionEvents':count,'actualBytes':record.get('actualBytes'),
          'events':record['events'],'exit':state})
    report={'revision':REVISION,'parserSHA256':digest(Path(__file__)),'sources':SOURCES,'runId':manifest['runId'],'sourceEvidenceSHA256':inputs,
      'scope':'finalized trial snapshot; memory.max=max does not prove ancestor limits absent; no NAS failover or application qualification',
      'trials':results}
    output=Path(output)
    snapshot=output.with_name(output.name+'.manifest-snapshot.json')
    with snapshot.open('xb') as out:out.write(manifest_raw)
    report['manifestSnapshot']=snapshot.name
    with output.open('x') as out:json.dump(report,out,indent=2);out.write('\n')
    print(json.dumps({'output':str(output),'finalized':len(results),'eligible':sum(t['comparisonEligible'] for t in results)}))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root');p.add_argument('output');a=p.parse_args();analyze(a.root,a.output)
