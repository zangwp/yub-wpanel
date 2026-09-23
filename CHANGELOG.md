# Changelog

## v2.0.2 — 2026-09-23

Security, reliability, and bounded-resource maintenance release.

- Added a version-bound runtime health gate to fresh installs and repairs, including process/listener ownership checks. Fresh installs are enabled only after passing the gate and are stopped and disabled on failure. Repair now stops an originally active panel immediately before its snapshot, keeps it stopped through deployment, and restores its original runtime state after success or rollback.
- Removed administrator and database passwords from process arguments during installation and removed the legacy plaintext-password `--passwd` reset path; use `yubw password` (the random `--reset-admin` flow) instead.
- Hardened public site-migration endpoints with authentication before body reads where possible, bounded body-read time, and concurrency limits against slow-request exhaustion.
- Bounded task admission and completed-task retention, redacted retained task state, and synchronized task status updates.
- Made Cron mutations transactional across database, managed WP-Cron markers, and system-Cron rendering; bounded command output and log retention, terminated full process groups on timeout, and moved execution locks into a verified root-private runtime directory.
- Changed alert evaluation to preserve firing alerts when a check is unknown because its database query or row scan failed, preventing false recovery notifications.
- Standardized the project license display name as **YUB WPanel Open Source License** with the SPDX identifier `GPL-3.0-only`.
- Added regression tests for installation rollback and secret handling, migration request guards, task queue limits/retention, and alert evaluation failures.

For supported installations already running `v2.0.1`, take a server snapshot and panel backup, then download the fixed `v2.0.2` installer, panel binary, and license archive together with each asset's SHA-256 manifest and Ed25519 signature (nine files). Verify all three bundles, run the local installer, and select repair. The binary-only online updater does not refresh `/usr/share/doc/yub-wpanel` and must not be used for this upgrade. Confirm `v2.0.2`, a healthy service, and `RELEASE_VERSION=v2.0.2` under the installed documentation directory. This release does not add a database schema migration. See [`docs/upgrade-compatibility.md`](docs/upgrade-compatibility.md).

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
- Completed the GPL-3.0-only license text, YUB WPanel project notice, verified-install guide, and explicit security/upgrade boundaries.

This release does not provide automatic migration from an installation created under another distribution identity. See [`docs/upgrade-compatibility.md`](docs/upgrade-compatibility.md).

Because the updater shipped in v2.0.0 predates this release's version binding and hardened watchdog, the v2.0.0 to v2.0.1 transition must use the verified fixed-version `install.sh` and `yub-wpanel` Release bundles in repair mode. Later releases may also require installer repair when non-binary installed assets must be synchronized; follow the fixed-version instructions for the target release.

## v2.0.0 — 2026-09-21

Initial YUB WPanel independent distribution release.
