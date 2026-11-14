#!/usr/bin/env python3
"""Verify the actual deb layout and installed text/binary payloads."""
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

PACKAGE = Path(sys.argv[1]).resolve() if len(sys.argv) == 2 else None

class PackageLayoutTests(unittest.TestCase):
    def test_actual_package_has_minimal_neutral_layout(self):
        self.assertIsNotNone(PACKAGE, 'Pass a built .deb package')
        self.assertEqual(subprocess.check_output(['dpkg-deb','--field',str(PACKAGE),'Package'],text=True).strip(),'incus-lts')
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            subprocess.run(['dpkg-deb','--raw-extract',str(PACKAGE),directory],check=True,stdout=subprocess.DEVNULL)
            files=[p for p in root.rglob('*') if p.is_file()]
            for path in files:
                relative=str(path.relative_to(root))
                self.assertIsNone(re.search(r'payincus|incudal',relative,re.I),relative)
                # Also scan Go symbol/source metadata and embedded third-party notice payloads.
                self.assertIsNone(re.search(rb'payincus|incudal',path.read_bytes(),re.I),relative)
            docs=root/'usr/share/doc/incus'
            self.assertEqual(sorted(p.name for p in docs.iterdir()),['copyright'])
            for name in ['FEATURES.md','INSTALL.md','RELEASE-NOTES.md','AGENTS.md']:
                self.assertFalse(any(p.name==name for p in files),name)
            self.assertTrue((root/'usr/libexec/incus/incus-agent').is_file(),'upstream guest agent must retain its native path')
            self.assertTrue((root/'usr/libexec/incus/agent/incus-agent').is_file(),'node connector must have a separate path')
            unit=(root/'usr/lib/systemd/system/incus-agent.service').read_text()
            self.assertIn('ExecStart=/usr/libexec/incus/agent/incus-agent -config /etc/incus/agent/config.yaml',unit)
            self.assertTrue((root/'usr/libexec/incus/node-upgrade.sh').is_file())
            self.assertTrue((root/'usr/libexec/incus/node-uninstall.sh').is_file())
            self.assertTrue((root/'usr/share/incus/manifest.json').is_file())

if __name__=='__main__':unittest.main(argv=[sys.argv[0]])
