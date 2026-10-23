Xray-core is licensed under the Mozilla Public License 2.0.
Source: https://github.com/XTLS/Xray-core/tree/v26.3.27
License: https://github.com/XTLS/Xray-core/blob/v26.3.27/LICENSE

manifest.json pins the release archives and executable digests.
The generated xray-linux-*.gz payloads are ignored by Git and are prepared by
scripts/payincus/prepare-xray.py before release builds. They are embedded in the
custom incusd and network-worker executables. Source builds without a payload report Xray unavailable.
