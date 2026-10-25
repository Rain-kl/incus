#!/usr/bin/env python3
"""Run the actual installer with fixture OS metadata, GitHub transport and apt."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('install.sh').read_text()


class InstallerTests(unittest.TestCase):
    def run_installer(self, channel='canary', target='debian-13', arch='amd64', damage=False,
                      missing=False, bad_url=False, distro=None, draft=False, wrong_channel=False):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            binary = root / 'bin'
            binary.mkdir()
            payload = b'deb fixture bytes'
            name = f'payincus-incus-7.0.2-integration.abcdef012345-linux-{target}-{arch}.deb'
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
''',
                'dpkg': "import os\nprint(os.environ['TEST_ARCH'])\n",
            }
            for name_, source in mocks.items():
                path = binary / name_
                path.write_text(f'#!{sys.executable}\n' + source)
                path.chmod(0o755)
            env = {**os.environ, 'PATH': f'{binary}:' + os.environ['PATH'], 'FIXTURE': str(root), 'TEST_ARCH': arch}
            result = subprocess.run(['bash', str(root/'install.sh'), channel, '--no-tui'],
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


if __name__ == '__main__':
    unittest.main()
