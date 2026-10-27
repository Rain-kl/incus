#!/usr/bin/env python3
"""Run the actual installer with fixture OS metadata, GitHub transport and apt."""
import ast
import hashlib
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest

SCRIPT = Path(__file__).with_name('install.sh').read_text()


def evaluate_publish_condition(node, github):
    """Evaluate only boolean comparisons from the actual workflow job gate."""
    if isinstance(node, ast.Constant):
        return node.value
    if isinstance(node, ast.Name) and node.id == 'github':
        return github
    if isinstance(node, ast.Attribute) and not node.attr.startswith('_'):
        return getattr(evaluate_publish_condition(node.value, github), node.attr)
    if isinstance(node, ast.BoolOp):
        values = [evaluate_publish_condition(value, github) for value in node.values]
        if isinstance(node.op, ast.And):
            return all(values)
        if isinstance(node.op, ast.Or):
            return any(values)
    if isinstance(node, ast.Compare) and len(node.ops) == 1 and isinstance(node.ops[0], ast.Eq):
        return evaluate_publish_condition(node.left, github) == evaluate_publish_condition(node.comparators[0], github)
    raise AssertionError(f'Unsupported publish condition: {ast.dump(node)}')


class InstallerTests(unittest.TestCase):
    def run_installer(self, channel='canary', target='debian-13', arch='amd64', damage=False,
                      missing=False, bad_url=False, distro=None, draft=False, wrong_channel=False,
                      mode='--no-tui', apt_failure=False):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            binary = root / 'bin'
            binary.mkdir()
            payload = b'deb fixture bytes'
            version = '7.0.2-integration.abcdef012345' if channel == 'canary' else '7.0.2'
            name = f'payincus-incus-{version}-linux-{target}-{arch}.deb'
            checksum = hashlib.sha256(payload).hexdigest()
            (root / 'package').write_bytes(payload + (b'corrupt' if damage else b''))
            (root / 'checksum').write_text(f'{checksum}  {name}\n')
            prefix = 'https://github.com/Rain-kl/incus/releases/download/test/'
            release = {'tag_name': 'integration-7.0.2-abcdef012345' if channel == 'canary' else 'payincus-v7.0.2',
                       'prerelease': channel == 'canary', 'draft': draft, 'published_at': '2026-10-23T00:00:00Z',
                       'assets': [] if missing else [
                           {'name': name, 'browser_download_url': ('https://evil.invalid/' if bad_url else prefix) + name},
                           {'name': f'SHA256SUMS-{target}-{arch}', 'browser_download_url': prefix + 'SHA256SUMS'}]}
            if wrong_channel:
                release['prerelease'] = not release['prerelease']
            (root / 'releases.json').write_text(json.dumps([release] if channel == 'canary' else release))
            os_id, version = (distro or target).rsplit('-', 1)
            (root / 'os-release').write_text(f'ID={os_id}\nVERSION_ID={version}\nID_LIKE=debian\n')
            script = SCRIPT.replace('. /etc/os-release', f'. "{root}/os-release"')
            script = '\n'.join(line for line in script.splitlines() if not line.startswith('[[ $EUID -eq 0 ]]'))
            (root / 'install.sh').write_text(script)
            mocks = {
                'curl': '''import os,sys,shutil
from pathlib import Path
args=sys.argv[1:]; url=next(a for a in args if a.startswith('https://'))
name='releases.json' if 'api.github.com/' in url else ('checksum' if 'SHA256SUMS' in url else 'package')
shutil.copyfile(Path(os.environ['FIXTURE'])/name,args[args.index('-o')+1])
''',
                'apt-get': '''import os,sys
from pathlib import Path
with (Path(os.environ['FIXTURE'])/'apt.log').open('a') as f: f.write(' '.join(sys.argv[1:])+'\\n')
if 'install' in sys.argv and os.environ.get('TEST_APT_FAILURE') == '1':
    print('APT fixture error: package installation failed', file=sys.stderr)
    sys.exit(100)
if '--allow-downgrades' in sys.argv and os.environ.get('DEBIAN_FRONTEND') != 'noninteractive':
    print('Update unexpectedly allows interactive package prompts', file=sys.stderr)
    sys.exit(101)
if '--allow-downgrades' in sys.argv and os.environ.get('NEEDRESTART_MODE') != 'l':
    print('Update delegates unexpected restarts to needrestart', file=sys.stderr)
    sys.exit(102)
''',
                'dpkg': "import os\nprint(os.environ['TEST_ARCH'])\n",
            }
            for name_, source in mocks.items():
                path = binary / name_
                path.write_text(f'#!{sys.executable}\n' + source)
                path.chmod(0o755)
            env = {**os.environ, 'PATH': f'{binary}:' + os.environ['PATH'], 'FIXTURE': str(root), 'TEST_ARCH': arch,
                   'TEST_APT_FAILURE': '1' if apt_failure else '0'}
            result = subprocess.run(['bash', str(root/'install.sh'), channel, mode],
                                    env=env, text=True, capture_output=True, timeout=15)
            log = (root / 'apt.log').read_text() if (root / 'apt.log').exists() else ''
            return result, log

    def test_platform_and_channel_matrix(self):
        for channel in ('latest', 'canary'):
            for target in ('debian-13', 'ubuntu-24.04'):
                for arch in ('amd64', 'arm64'):
                    with self.subTest(channel=channel, target=target, arch=arch):
                        result, log = self.run_installer(channel, target, arch)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertIn(f'-linux-{target}-{arch}.deb', log)
                        self.assertIn('install -y ', log)

    def test_fail_closed_before_apt(self):
        for kwargs in ({'damage': True}, {'missing': True}, {'bad_url': True}, {'draft': True}, {'wrong_channel': True},
                       {'arch': 'riscv64'}, {'distro': 'debian-12'}, {'channel': 'work'}):
            with self.subTest(kwargs=kwargs):
                result, log = self.run_installer(**kwargs)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(log, '', result.stdout + result.stderr)

    def test_update_channel_switch_and_reinstall(self):
        for channel, target, arch in itertools.product(('latest', 'canary'), ('debian-13', 'ubuntu-24.04'), ('amd64', 'arm64')):
            with self.subTest(channel=channel, target=target, arch=arch):
                result, log = self.run_installer(channel, target, arch, mode='--update')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('--reinstall --allow-downgrades', log)
                self.assertIn('Node package installed:', result.stdout)
                self.assertNotIn('interactive terminal', result.stdout)
        result, _ = self.run_installer(mode='--update', apt_failure=True)
        self.assertEqual(result.returncode, 100)
        self.assertIn('APT fixture error:', result.stderr)
        self.assertNotIn('Node package installed:', result.stdout)

    def test_update_failures_do_not_install(self):
        for kwargs in ({'damage': True}, {'missing': True}, {'bad_url': True}, {'wrong_channel': True}, {'draft': True}):
            with self.subTest(kwargs=kwargs):
                result, log = self.run_installer(mode='--update', **kwargs)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(log, '')

    def test_publish_gate_allows_trusted_manual_canary_builds(self):
        workflow = Path(__file__).resolve().parents[2] / '.github/workflows/payincus-publish.yml'
        match = re.search(r'\n    if: >-\n((?:      .+\n)+)', workflow.read_text())
        self.assertIsNotNone(match, 'Missing integration publish job gate')
        expression = ' '.join(match[1].split()).replace('&&', ' and ').replace('||', ' or ')
        condition = ast.parse(expression, mode='eval').body
        cases = itertools.product(
            ('push', 'workflow_dispatch', 'pull_request', 'pull_request_target', 'schedule'),
            ('success', 'failure', 'cancelled'), ('canary', 'main', 'payincus-v7.0.2'),
            ('Rain-kl/incus', 'untrusted/fork'))
        for event, conclusion, branch, repository in cases:
            with self.subTest(event=event, conclusion=conclusion, branch=branch, repository=repository):
                run = SimpleNamespace(event=event, conclusion=conclusion, head_branch=branch,
                                      head_repository=SimpleNamespace(full_name=repository))
                github = SimpleNamespace(repository='Rain-kl/incus', event=SimpleNamespace(workflow_run=run))
                expected = (event in ('push', 'workflow_dispatch') and conclusion == 'success'
                            and branch == 'canary' and repository == 'Rain-kl/incus')
                self.assertEqual(evaluate_publish_condition(condition, github), expected)


if __name__ == '__main__':
    unittest.main()
