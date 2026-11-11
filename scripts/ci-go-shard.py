#!/usr/bin/env python3
"""Partition complete top-level Go tests; prove execution before aggregation."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess


def go_events(arguments, log):
    events = []
    with log.open('w') as output:
        process = subprocess.Popen(['go', 'test', '-json', *arguments, './...'], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        for line in process.stdout:
            output.write(line)
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                pass
        code = process.wait()
    if code:
        print(log.read_text()[-20000:])
        raise RuntimeError(f'Go command failed ({code}); full output: {log}')
    return events


def shard(index, count, root):
    root.mkdir(parents=True, exist_ok=False)
    common = ['-race', '-covermode=atomic', '-coverpkg=./...', '-count=1']
    listing = go_events([*common, '-list', '^(Test|Example|Fuzz)'], root/'listing.jsonl')
    tests = sorted({(e['Package'], e['Output'].strip()) for e in listing if e.get('Action') == 'output' and re.fullmatch(r'(?:Test|Example|Fuzz)\w*', e.get('Output', '').strip())})
    if not tests:
        raise RuntimeError('test inventory is empty')
    # NAME, not package/name: one global -run pattern runs every package.
    def owner(name):
        return int.from_bytes(hashlib.sha256(name.encode()).digest()[:8], 'big') % count
    selected = [(package, name) for package, name in tests if owner(name) == index]
    if count == 1:
        # One shard owns the whole inventory. Naming every test would put
        # ~1900 names in one argument, which Linux caps at 128 KiB per
        # string; the listing's own prefix selects the same set, and the
        # execution check below still proves it.
        pattern = '^(?:Test|Example|Fuzz)'
    elif selected:
        pattern = '^(?:'+'|'.join(re.escape(name) for name in sorted({name for _, name in selected}))+')$'
    else:
        pattern = '^__NO_GO_TEST_IN_THIS_SHARD__$'
    metadata = {'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip(), 'go': subprocess.check_output(['go', 'version'], text=True).strip(), 'goos': subprocess.check_output(['go', 'env', 'GOOS'], text=True).strip(), 'goarch': subprocess.check_output(['go', 'env', 'GOARCH'], text=True).strip(), 'index': index, 'count': count, 'tests': tests, 'selected': selected}
    (root/'plan.json').write_text(json.dumps(metadata, indent=2))
    events = go_events([*common, '-timeout=45m', '-run', pattern, '-coverprofile='+str(root/'coverage.out')], root/'tests.jsonl')
    ran = {(e['Package'], e['Test']) for e in events if e.get('Action') == 'run' and '/' not in e.get('Test', '')}
    ended = {(e['Package'], e['Test']) for e in events if e.get('Action') in ('pass', 'skip') and e.get('Test') and '/' not in e['Test']}
    if ran != set(selected) or ended != set(selected):
        raise RuntimeError(f'execution differs from plan: missing={set(selected)-ended}, unexpected={ran-set(selected)}')
    (root/'complete.json').write_text(json.dumps({'planSHA256': hashlib.sha256((root/'plan.json').read_bytes()).hexdigest(), 'profileSHA256': hashlib.sha256((root/'coverage.out').read_bytes()).hexdigest(), 'executed': sorted(ended)}))
    print(f'shard {index}/{count}: {len(selected)} complete top-level tests')


def merge(root, count, output):
    plans = []
    blocks = {}
    all_selected = set()
    for index in range(count):
        folder = root/f'go-coverage-{index}'
        plan_bytes = (folder/'plan.json').read_bytes()
        profile_bytes = (folder/'coverage.out').read_bytes()
        plan = json.loads(plan_bytes); complete = json.loads((folder/'complete.json').read_text())
        if plan['index'] != index or plan['count'] != count or complete['planSHA256'] != hashlib.sha256(plan_bytes).hexdigest() or complete['profileSHA256'] != hashlib.sha256(profile_bytes).hexdigest():
            raise RuntimeError('shard provenance mismatch')
        if plans and any(plan[k] != plans[0][k] for k in ('commit', 'go', 'goos', 'goarch', 'tests')):
            raise RuntimeError('shards used different source/toolchain/inventory')
        expected = {tuple(t) for t in plan['selected']}
        if {tuple(t) for t in complete['executed']} != expected or all_selected & expected:
            raise RuntimeError('missing or duplicated shard execution')
        all_selected |= expected; plans.append(plan)
        lines = profile_bytes.decode().splitlines()
        if not lines or lines[0] != 'mode: atomic':
            raise RuntimeError('atomic coverage profile required')
        for line in lines[1:]:
            location, statements, hits = line.split(); statements, hits = int(statements), int(hits)
            if hits < 0 or statements < 0:
                raise RuntimeError('negative coverage data')
            previous = blocks.setdefault(location, [statements, 0])
            if previous[0] != statements:
                raise RuntimeError('coverage source blocks disagree')
            previous[1] += hits
    if all_selected != {tuple(t) for t in plans[0]['tests']} or not blocks:
        raise RuntimeError('incomplete test partition or empty profile')
    if plans[0]['commit'] != subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip():
        raise RuntimeError('aggregate checkout differs from tested source')
    output.write_text('mode: atomic\n'+''.join(f'{loc} {n} {hits}\n' for loc, (n, hits) in sorted(blocks.items())))
    print(f'merged {count} successful shards: {len(all_selected)} top-level tests, {len(blocks)} source blocks')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=['run', 'merge']); parser.add_argument('--count', type=int, default=8); parser.add_argument('--index', type=int); parser.add_argument('--root', type=Path, required=True); parser.add_argument('--output', type=Path, default=Path('coverage.out'))
    args = parser.parse_args()
    if args.count < 1 or (args.operation == 'run' and (args.index is None or not 0 <= args.index < args.count)):
        parser.error('valid shard index/count required')
    if args.operation == 'run':
        shard(args.index, args.count, args.root)
    else:
        merge(args.root, args.count, args.output)
