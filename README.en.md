# Plex Local Bridge

[简体中文](README.md) | [English](README.en.md)

[![CI](https://github.com/hamhub-app/plex-local-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/hamhub-app/plex-local-bridge/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/hamhub-app/plex-local-bridge?include_prereleases)](https://github.com/hamhub-app/plex-local-bridge/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Fix Plex playback failures over the internet, 403 responses, and failures to read original media files.**

Plex Local Bridge is an open-source local proxy package for Synology NAS, developed by HamHub. It forwards playback requests from the internet to the Plex service on your NAS, addressing playback failures caused by how Plex identifies the source of a request.

Configure and enable it through the DSM WebUI, then keep using your existing public address, clients, and router port forwarding.

## What it solves

- Plex cannot play media over the internet, or media requests return 403.
- Plex Remote Access is configured and clients can connect to the server, but original media files will not play.
- You want to handle forwarding locally on your Synology NAS while keeping your existing access endpoint and port forwarding.

Default request path: `Internet client → NAS:32400 → Local proxy:13240 → Native Plex:32400`.

The proxy preserves Plex account authentication, HTTP Range requests, WebSockets, and streaming. It does not transcode, cache media, or scan entire media files.

## Getting started

Designed for the **native Plex Media Server package on DSM 7.2.2+**. Before you begin, make sure requests from the internet can already reach your NAS, your Plex server is claimed, and account authentication is enabled. Leave Plex's list of IP addresses and networks allowed without authentication empty.

1. Download the `.spk` for your NAS architecture from **Assets** on [GitHub Releases](https://github.com/hamhub-app/plex-local-bridge/releases), then select it in DSM under **Package Center → Manual Install**. **No extraction, compilation, or GitHub login is required.** Choose `x86_64.spk` for an Intel / AMD 64-bit NAS or `armv8.spk` for an ARM 64-bit NAS.
2. Start the package and open its WebUI. The default NAS entry port is **32400**, and the proxy listens on **13240**. Both ports can be changed.
3. If you only use public IPv4 access, clear “Also take over the IPv6 entry” (同时接管 IPv6 入口).
4. Click “Save and apply” (保存并应用), then “Enable / update takeover” (启用 / 更新接管), and confirm your administrator password in the **native DSM dialog**.
5. Once the page shows “Takeover active” (接管中), try playback again using your existing public address.

Each release includes SPKs for both architectures and matching `.spk.sha256` checksum files. Development builds are also available from [GitHub Actions](https://github.com/hamhub-app/plex-local-bridge/actions/workflows/ci.yml): download the `plex-local-bridge-spk` artifact, extract it, and install the SPK inside.

To restore takeover automatically after a NAS restart, select “Automatically restore takeover after NAS restart” (NAS 重启后自动恢复接管) before authorizing. Your administrator password is submitted only to DSM for verification. The package backend does not receive, log, or store it.

If the DSM firewall is enabled, allow the proxy's listening port with the same source address rules as the original Plex port. Disable IPv6 takeover on systems without `ip6table_nat.ko`.

## Compatibility and scope

| Environment | Status |
| --- | --- |
| DS220+ / x86_64 / DSM 7.4.1 | WebUI, administrator authorization, and IPv4 takeover verified. A user has confirmed successful playback over the internet. |
| armv8 | Cross-builds and isolated Linux integration tests are provided. Validation on a physical ARM NAS is pending. |
| HTTPS | Passes encrypted traffic through using Plex's existing certificates. Encrypted source headers cannot be rewritten, so results depend on the server version and connection path. |

The current version is **0.2.6-0009 (experimental)** and primarily targets the native Plex package. Access paths involving other Plex versions, Plex Relay, Docker networking, or other reverse proxies require separate validation.

## Building from source

Requires Go 1.24+ and Python 3.9+.

```sh
git clone https://github.com/hamhub-app/plex-local-bridge.git
cd plex-local-bridge
python3 scripts/build_spk.py --arch amd64
python3 scripts/build_spk.py --arch arm64
```

Build artifacts and SHA-256 checksums are written to `dist/`. `amd64` maps to x86_64, and `arm64` maps to armv8.

To publish a release, update `VERSION` to the package version and push the matching `v<VERSION>` tag, such as `v0.2.6-0009`. After tests, builds, and verification pass, CI automatically creates a prerelease and uploads the SPKs and SHA-256 files for both architectures. The tag must match `VERSION`.

For more configuration options, the authorization model, diagnostics, recovery, and test commands, see the [detailed documentation (Chinese)](doc/DETAILS.md).

## About HamHub

HamHub is an all-in-one media player supporting cloud drives, NAS, and media services such as Plex, Emby, and Jellyfin. It brings movies and TV shows together in one media library, with media playback and offline downloads.

Website: [www.hamhub.app](https://www.hamhub.app/)

## License

[MIT License](LICENSE). This project is not affiliated with Plex or Synology.
