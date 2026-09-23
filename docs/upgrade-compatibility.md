# 安装身份与升级兼容性

YUB WPanel 当前支持以下两类操作：

- 在 Debian 13/Trixie 或 Ubuntu 24.04/Noble 的 amd64、arm64 服务器上全新安装；
- 对已使用当前 YUB WPanel 服务名、二进制路径和计划任务路径的安装执行修复或签名更新。

## v2.0.2 到 v2.1.0

`v2.1.0` 把发布二进制改为 `yub-wpanel-linux-amd64` 与 `yub-wpanel-linux-arm64`，同时新增 Ubuntu 24.04、ARM64 和 `b`/`B` 管理命令。`v2.0.2` 的在线更新器只查找旧的无架构后缀资产，因此这次必须从固定 `v2.1.0` Release 下载目标架构的面板三件套、安装器三件套和许可归档三件套，完成验签后运行 `bash install.sh` 并选择 repair。完整命令见[验签安装指南](verified-install.md)。

repair 只升级当前服务器上的受支持安装：它不会把 Debian 原地转换成 Ubuntu，也不会改变 CPU 架构。完成后用 `b info`（或 `B info`）确认 `v2.1.0`，再用 `b status` 检查服务。之后 `v2.1.0` 的在线更新器会按当前 `GOARCH` 选择相应架构资产。

## v2.0.1 到 v2.0.2

`v2.0.2` 不新增数据库结构迁移。升级前仍应创建服务器快照和面板数据库备份。从受支持的 `v2.0.1` 安装升级时，必须从固定的 `v2.0.2` Release 下载 `install.sh`、`yub-wpanel` 和 `yub-wpanel-third-party-licenses.tar.gz`，以及每个资产各自的 `.sha256` 和 `.sha256.sig`（共九个文件）。按[验签安装指南](verified-install.md)逐组验签后，运行本地 `bash install.sh` 并选择“继续/修复安装”。

这次升级不要使用只替换面板二进制的在线更新器，因为它不会同步 `/usr/share/doc/yub-wpanel` 中与版本绑定的许可材料。repair 会在同一事务中更新二进制与许可文档，并在失败时恢复快照。完成后运行 `/usr/local/bin/yub-wpanel --info --config /www/server/panel/config.json` 确认版本为 `v2.0.2`，确认面板服务健康，并确认 `/usr/share/doc/yub-wpanel/RELEASE_VERSION` 的内容为 `v2.0.2`。仍在 `v2.0.0` 的安装必须先按下节完成到 `v2.0.1` 的一次性安全升级，不可跳过该桥接步骤。

## v2.0.0 到 v2.0.1 的一次性安全升级

`v2.0.0` 的内置在线更新器早于本版本新增的候选版本绑定、严格回滚计划校验和独立看门狗，因此不要用它完成到 `v2.0.1` 的首跳。请先创建服务器快照和面板备份，然后从固定的 `v2.0.1` Release（不要使用 `latest` 或分支文件）下载以下九个资产到同一私有目录：

- `install.sh`、`install.sh.sha256`、`install.sh.sha256.sig`
- `yub-wpanel`、`yub-wpanel.sha256`、`yub-wpanel.sha256.sig`
- `yub-wpanel-third-party-licenses.tar.gz`、`yub-wpanel-third-party-licenses.tar.gz.sha256`、`yub-wpanel-third-party-licenses.tar.gz.sha256.sig`

按[验签安装指南](verified-install.md)中的公钥逐组验证三个资产的签名和哈希，再运行本地 `bash install.sh` 并选择“继续/修复安装”。安装器会再次验证同目录中的面板与许可归档两组三件套，并在修改服务器前检查现有配置、systemd unit 和 cron 身份。完成后用目标版本提供的管理命令确认版本。升级到后续版本时必须遵循目标 Release 的固定版本说明；需要同步非二进制安装资产的版本仍会要求 installer repair。

当前版本**不支持**把由其他发行身份创建的面板直接原地改名为 YUB WPanel。面板服务、命令行入口、WordPress 配套插件、站点密钥、PHP-FPM 环境变量、Nginx/Fail2ban 资源和数据库选项互有关联，只替换二进制或手动重命名其中一部分会产生不可恢复的半迁移状态。

为避免误操作，程序启动和安装器 repair 预检会核对以下当前身份：

- `/etc/cron.d/yub_wpanel_cron`
- `yub-wpanel`
- `/etc/systemd/system/yub-wpanel.service`
- `/usr/local/bin/yub-wpanel`

任一配置字段不匹配时，操作会以 `config_distribution_identity_mismatch` 失败。repair 还要求现有 systemd unit 与本发行版生成内容完全一致、由 root 安全持有且没有 service drop-in；检查失败会在持久化改动前停止。不要通过编辑 `config.json`、复制发行资产、drop-in 或手工重命名服务来绕过这些检查。

如需迁移不同发行身份下的既有安装，请先保留完整服务器快照、面板 SQLite 备份、网站文件与数据库、配置文件及证书，并等待经过签名、具备逐阶段回滚能力的专用迁移工具。当前仓库尚未提供该工具。

> “网站搬家”仅用于相同版本的 YUB WPanel 服务器之间迁移网站，不是面板发行身份迁移工具。

## Installation identity and upgrade compatibility

YUB WPanel currently supports fresh installs on Debian 13/Trixie or Ubuntu 24.04/Noble for amd64 and arm64, repairs of installations that already use the current YUB WPanel identity, and signed updates within that identity.

## v2.0.2 to v2.1.0

`v2.1.0` publishes `yub-wpanel-linux-amd64` and `yub-wpanel-linux-arm64`, adds Ubuntu 24.04 and ARM64 support, and installs the `b`/`B` management commands. The `v2.0.2` updater only knows the former architecture-neutral asset name, so this transition must use the fixed `v2.1.0` Release installer in repair mode after verifying the installer, architecture-specific binary, and license archive bundles. See the [verified installation guide](verified-install.md) for the complete command.

Repair upgrades the supported installation on the current host; it does not convert Debian to Ubuntu or change the CPU architecture. Confirm `v2.1.0` with `b info` (or `B info`) and then run `b status`. Subsequent updates from `v2.1.0` select the release asset matching the current `GOARCH`.

## v2.0.1 to v2.0.2

`v2.0.2` does not add a database schema migration. Take a server snapshot and panel database backup before updating. To upgrade a supported `v2.0.1` installation, download `install.sh`, `yub-wpanel`, and `yub-wpanel-third-party-licenses.tar.gz` from the fixed `v2.0.2` Release together with each asset's `.sha256` and `.sha256.sig` files (nine files total). Verify all three bundles as shown in the [verified installation guide](verified-install.md), run the local `bash install.sh`, and select repair.

Do not use the binary-only online updater for this upgrade because it does not synchronize the version-bound material under `/usr/share/doc/yub-wpanel`. Repair updates the binary and installed license documentation in one transaction and restores the snapshot on failure. Afterwards, run `/usr/local/bin/yub-wpanel --info --config /www/server/panel/config.json` to confirm `v2.0.2`, verify that the panel service is healthy, and confirm that `/usr/share/doc/yub-wpanel/RELEASE_VERSION` contains `v2.0.2`. An installation still on `v2.0.0` must first complete the one-time safe upgrade to `v2.0.1` described below; do not skip that bridge.

## One-time safe upgrade from v2.0.0 to v2.0.1

The updater shipped in `v2.0.0` predates the candidate-version binding, strict rollback-plan validation, and isolated watchdog added here. Do not use that updater for the first hop to `v2.0.1`. Take a server snapshot and panel backup, then download all nine fixed-version assets from the `v2.0.1` Release—not from `latest` or a branch—into one private directory: `install.sh`, `yub-wpanel`, and `yub-wpanel-third-party-licenses.tar.gz`, together with each asset's `.sha256` and `.sha256.sig` files.

Verify all three asset signatures and checksums with the public key in the [verified installation guide](verified-install.md), run the local `bash install.sh`, and select the repair option. The installer verifies the colocated panel and license-archive bundles again and checks the existing configuration, systemd unit, and cron identity before changing persistent state. Confirm the version with the management command supplied by the target release. For later upgrades, follow the fixed-version instructions for the target Release; releases that synchronize non-binary installed assets still require installer repair.

An in-place rename from an installation created under a different distribution identity is not currently supported. Service units, CLI entry points, the managed WordPress plugin, site secrets, PHP-FPM variables, Nginx/Fail2ban resources, and database options are interdependent. Replacing only the binary or manually renaming a subset of those resources can leave the server partially migrated.

Startup and repair preflight therefore require the four configuration identities listed above. A mismatch fails closed with `config_distribution_identity_mismatch`. Repair also requires the existing systemd unit to exactly match the root-owned unit generated by this distribution and rejects service drop-ins. Failures stop before persistent repair changes begin. Do not bypass these guards by editing `config.json`, copying release assets, adding drop-ins, or renaming services manually.

Before any future cross-identity migration, preserve a full server snapshot, the panel SQLite database, site files and databases, configuration files, and certificates. This repository does not yet ship the dedicated signed, rollback-capable migration tool required for that operation.

The site-migration feature moves sites between YUB WPanel servers on the same version; it does not migrate the panel distribution itself.
