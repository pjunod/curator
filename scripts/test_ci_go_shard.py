import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('shards', Path(__file__).with_name('ci-go-shard.py'))
shards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shards)


class ShardIntegrity(unittest.TestCase):
    def fixtures(self, root, profiles=None):
        for index in range(2):
            folder = root/f'go-coverage-{index}'; folder.mkdir()
            selected = [['pkg', ['TestFirst', 'Example'][index]]]
            plan = {'commit': 'head', 'go': 'go version pinned', 'goos': 'linux', 'goarch': 'amd64', 'index': index, 'count': 2, 'tests': [['pkg', 'TestFirst'], ['pkg', 'Example']], 'selected': selected}
            raw = json.dumps(plan).encode(); profile = (profiles or ['mode: atomic\nf.go:1.1,2.1 3 0\n', 'mode: atomic\nf.go:1.1,2.1 3 2\nf.go:3.1,4.1 2 0\n'])[index].encode()
            (folder/'plan.json').write_bytes(raw); (folder/'coverage.out').write_bytes(profile)
            (folder/'complete.json').write_text(json.dumps({'planSHA256': hashlib.sha256(raw).hexdigest(), 'profileSHA256': hashlib.sha256(profile).hexdigest(), 'executed': selected}))

    @patch.object(shards.subprocess, 'check_output', return_value='head\n')
    def test_merge_keeps_zero_blocks_and_sums_shared_blocks(self, _):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); self.fixtures(root); output = root/'merged.out'
            shards.merge(root, 2, output)
            self.assertEqual(output.read_text(), 'mode: atomic\nf.go:1.1,2.1 3 2\nf.go:3.1,4.1 2 0\n')

    @patch.object(shards.subprocess, 'check_output', return_value='head\n')
    def test_missing_altered_and_inconsistent_evidence_cannot_pass(self, _):
        for corruption in ('missing', 'profile', 'execution', 'statements'):
            with self.subTest(corruption=corruption), tempfile.TemporaryDirectory() as directory:
                root = Path(directory); self.fixtures(root)
                folder = root/'go-coverage-1'
                if corruption == 'missing': (folder/'complete.json').unlink()
                elif corruption == 'profile': (folder/'coverage.out').write_text('mode: atomic\nf.go:1.1,2.1 3 100\n')
                elif corruption == 'execution':
                    p = folder/'complete.json'; data = json.loads(p.read_text()); data['executed'] = []; p.write_text(json.dumps(data))
                else:
                    raw = b'mode: atomic\nf.go:1.1,2.1 9 2\n'; (folder/'coverage.out').write_bytes(raw)
                    p = folder/'complete.json'; data = json.loads(p.read_text()); data['profileSHA256'] = hashlib.sha256(raw).hexdigest(); p.write_text(json.dumps(data))
                with self.assertRaises((RuntimeError, FileNotFoundError)): shards.merge(root, 2, root/'merged.out')

    def test_bare_example_fuzz_and_duplicate_names_are_in_inventory(self):
        names = [('a', 'Example'), ('a', 'FuzzSeeds'), ('a', 'TestShared'), ('b', 'TestShared')]
        listing = [{'Package': p, 'Action': 'output', 'Output': n+'\n'} for p, n in names]
        def events(arguments, log):
            if '-list' in arguments: return listing
            profile = next(a.split('=', 1)[1] for a in arguments if a.startswith('-coverprofile=')); Path(profile).write_text('mode: atomic\nf.go:1.1,2.1 1 1\n')
            return [{'Package': p, 'Action': a, 'Test': n} for p, n in names for a in ('run', 'pass')]
        with tempfile.TemporaryDirectory() as directory, patch.object(shards, 'go_events', side_effect=events), patch.object(shards.subprocess, 'check_output', return_value='fixture\n'):
            root = Path(directory)/'shard'; shards.shard(0, 1, root)
            self.assertEqual(json.loads((root/'plan.json').read_text())['tests'], [list(t) for t in sorted(names)])


if __name__ == '__main__': unittest.main()
