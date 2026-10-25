#!/usr/bin/env python3
"""Collect license notices from the isolated TUI's downloaded dependencies."""
from pathlib import Path
import sys

source, destination = map(Path, sys.argv[1:])
notices = []
for line in source.read_text().splitlines():
    fields = line.split(maxsplit=2)
    if len(fields) != 3:
        continue
    name, version, folder = fields
    root = Path(folder)
    licenses = [p for p in root.iterdir() if p.is_file() and p.name.lower() in
                ('license', 'license.md', 'license.txt', 'licence', 'copying')]
    if not licenses:
        readme = root / 'README.md'
        # go-localereader declares its MIT license and author in the README.
        if readme.is_file() and '## License\n\nMIT' in readme.read_text():
            licenses = [readme]
        else:
            sys.exit(f'Missing license for terminal UI dependency {name} {version}')
    extras = [root / name for name in ('NOTICE', 'PATENTS') if (root / name).is_file()]
    text = '\n'.join(p.read_text() for p in licenses + extras)
    notices.append(f'{name} {version}\n{text}')
if not notices:
    sys.exit('No terminal UI dependency licenses collected')
destination.write_text('\n\n'.join(notices))
