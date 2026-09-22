# Changelog

## v2.0.1 — 2026-09-22

Security and distribution hardening release.

- Added Ed25519 and SHA-256 verification for both installers and the panel binary before execution or deployment.
- Added Debian 13 amd64 fail-closed checks, private random work directories, bounded downloads, candidate preflight execution, and a signed-binary minimum-version floor.
- Pinned the PHP repository keyring package by exact version, SHA-256, package name, and architecture before allowing package installation.
- Isolated build, signing, and publishing jobs; pinned GitHub Actions and removed live frontend downloads from release builds.
- Pinned the release toolchain to Go 1.26.8 and upgraded affected Go dependencies; `govulncheck` reports no reachable known vulnerabilities for the Linux release target.
- Added signed Release assets for `install.sh` and `install-cn.sh` alongside the panel binary.
- Added a signed third-party license archive containing reviewed browser notices and the exact license files for linked Go modules.
- Bound online updates to a canonical stable Release tag and the version reported by the signed candidate binary, with a bounded preflight using the active configuration path and a strict one-record checksum manifest.
- Refused startup and repair when service, binary, or cron identities do not match the current YUB WPanel distribution.
- Added bounded HTTP header/idle handling and graceful shutdown.
- Fixed runtime telemetry settings persistence and added fail-closed custom HTTPS endpoint, dial, and redirect controls.
- Completed the GPL-3.0 license text, modification notice, verified-install guide, and explicit security/upgrade boundaries.

This release does not provide automatic migration from an installation created under another distribution identity. See [`docs/upgrade-compatibility.md`](docs/upgrade-compatibility.md).

Because the updater shipped in v2.0.0 predates this release's version binding and hardened watchdog, the v2.0.0 to v2.0.1 transition must use the verified fixed-version `install.sh` and `yub-wpanel` Release bundles in repair mode. The protected online-update path applies from v2.0.1 onward.

## v2.0.0 — 2026-09-21

Initial YUB WPanel independent distribution release.
