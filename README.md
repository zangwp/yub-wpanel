# YUB WPanel

<p><img src="static/logo.png" alt="YUB WPanel" width="120"></p>

WordPress 专用服务器管理面板。面向 Debian 13 与 Ubuntu 24.04 LTS 的纯净服务器，支持 amd64 和 arm64。

YUB WPanel 遵循 GNU GPL v3.0 only（SPDX：`GPL-3.0-only`）。源码、安装脚本和已签名发行版位于 [zangwp/yub-wpanel](https://github.com/zangwp/yub-wpanel)。

WordPress server management panel for Debian 13 and Ubuntu 24.04 LTS VPS environments on amd64 or arm64, focused on site isolation, SSL, backups, security, and day-to-day WordPress hosting operations.

## English Documentation

The full English project guide is available here: [README.en.md](README.en.md).

[![License](https://img.shields.io/badge/license-GPL--3.0--only-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev/)

---

## 🚀 快速安装

> **支持范围：Debian 13 (Trixie) / Ubuntu 24.04 LTS (Noble)，amd64 / arm64。** 使用 `root` 用户执行：

```bash
bash <(curl -fsSL https://wpanel.zangyubin.top/install)
```

短域名入口固定到 `v2.1.1` Release。Cloudflare Worker 会先验证 `bootstrap.sh` 的 Ed25519 签名和 SHA-256；引导脚本随后安装缺少的基础依赖，再次验签固定版本的 `install.sh`，最后才启动安装。它不会执行 GitHub `main` 分支上的可变脚本。

系统尚未安装 `curl` 时，先执行 `apt-get update && apt-get install -y curl`。需要在执行任何远程脚本前自行验签，或进行国内网络、离线安装时，请使用 **[完整验签安装指南](docs/verified-install.md)**。

## 定位

通用 Linux 面板臃肿、复杂、与 WordPress 无关的功能太多。

YUB WPanel 只做一件事：**在 VPS 上高效管理 WordPress 网站**。不做 Docker、不做邮件系统、不做 FTP、不做 Java/Python/Node 运行环境。

## 功能模块

| 模块 | 说明 |
|------|------|
| **网站管理** | 一键创建 WordPress 网站，也可暂停、启用、删除或重装；每个网站相互隔离，一个网站出问题时不容易影响其他网站 |
| **网站搬家** | 在两台相同版本的 YUB WPanel 服务器之间搬迁网站，可一次选择多个网站，查看各自进度并重试失败任务 |
| **WordPress 更新管理** | 在更新核心、插件或主题前先确认内容并自动备份；更新后检查网站是否正常，失败时自动恢复，插件还可批量更新 |
| **WordPress 站点总览** | 在一个页面查看所有 WordPress 网站的版本、插件、主题和待更新项目，不必逐个登录后台检查 |
| **WordPress 维护保护** | 更新或维护网站时自动显示维护页面，结束后恢复访问；临时维护到期后也会自动恢复，减少忘记开启网站的风险 |
| **SSL 证书** | Let's Encrypt 自动申请、到期前 30 天自动续签、手动替换、自签名证书 |
| **网站加速** | 提供页面缓存，并可从 WordPress 后台一键清理；上传图片时也可按设置自动优化，减少图片占用和加载时间 |
| **安全防御** | 自动拦截常见的登录爆破、恶意扫描和高频爬虫，并整理可疑访问记录，方便判断是否需要处理 |
| **密码找回保护** | 可按网站决定是否允许找回密码，也可以只禁止管理员账号通过公开页面找回，降低账号被试探的风险 |
| **数据库管理** | 修改数据库密码，手动或自动备份数据库，并可上传备份进行恢复；需要临时管理时可按需开启管理工具 |
| **计划任务** | 用页面管理定时任务，可用更可靠的系统任务替代 WordPress 自带定时任务，并查看备份等任务是否正常运行 |
| **文件管理器** | 像使用电脑文件管理器一样上传、下载、复制、移动、压缩、解压和搜索文件；大文件支持断点续传 |
| **仪表盘** | CPU/内存/磁盘/负载实时监控、24h/7d/15d 历史趋势图 |
| **系统稳定性防护** | 内存不足时自动增加缓冲空间；重要服务异常、内存耗尽或资源使用异常时保留线索并发出提醒 |
| **网站异常监测** | 关注管理员账号、内容、重要设置、应用密码和异常文件变化，帮助尽早发现网站被篡改的迹象 |
| **AI 诊断** | 一键汇总网站日志和服务状态，用对话方式继续追问问题；只提供分析建议，不会自行修改网站 |
| **告警通知** | 可通过邮件接收资源不足、服务异常、证书到期、网站到期和可用更新等提醒，每类提醒可单独开关 |
| **软件与运行环境** | 在面板中管理 PHP、Nginx、MariaDB 和 Redis，查看日志，并按网站调整 PHP 或 Nginx 设置 |
| **面板安全** | 使用不公开的登录入口和两次登录验证；连续输错密码或频繁扫描错误地址时会自动限制来源 |
| **安全更新** | 面板和当前 Debian/Ubuntu 系统软件都可检查更新；面板更新会验签并在健康检查失败时尝试回滚 |
| **备份与异地保存** | 自动备份网站和面板数据，并可把网站备份同步到另一台服务器或对象存储，减少单机故障造成的损失 |

## 验签安装说明

生产服务器请从 GitHub Release 下载安装器、SHA-256 清单和 Ed25519 签名，验签成功后再以 root 执行。不要把可变分支脚本直接通过管道交给 shell。国内入口同样必须先验签，并支持管理员明确配置的 HTTPS GitHub 反代。

完整可复制命令、公钥与本地发布包说明见 **[验签安装指南](docs/verified-install.md)**。

> **升级提示：** v2.0.0 到 v2.0.1，以及 v2.0.1 到 v2.0.2，都必须从目标版本的固定 Release 下载 `install.sh`、`yub-wpanel`、第三方许可归档及各自的 SHA-256 清单和 Ed25519 签名（共九个文件），逐组验签后运行本地 `install.sh` 并选择 repair。v2.0.2 此路径还会同步 `/usr/share/doc/yub-wpanel`；不要使用只替换二进制的面板在线更新器完成这次升级。详见[升级兼容性说明](docs/upgrade-compatibility.md)。

> **v2.1.0 升级提示：** v2.0.2 的在线更新器不认识新的架构化资产名。升级到 v2.1.0 时同样必须使用固定 Release 安装器 repair，并下载与服务器匹配的 `yub-wpanel-linux-amd64` 或 `yub-wpanel-linux-arm64` 三件套。

安装完成后输出面板地址和两层登录凭据（BasicAuth + Web 登录）。

> 初始自签名证书只能加密连接，不能替你确认服务器身份。首次登录前应通过 SSH 核对证书 SHA-256 指纹；公网长期使用时请换成可信证书并限制管理端口来源。

> 由其他发行身份创建的旧安装不能直接通过改名或 repair 迁移。请先阅读 **[安装身份与升级兼容性](docs/upgrade-compatibility.md)**。

## 网站搬家

YUB WPanel 支持在两台相同版本的面板之间搬迁 WordPress 或通用 PHP 网站。升级两台服务器后，从「网站管理」进入「网站搬家」，建立面板连接并选择需要迁移的网站。

- 可迁移网站文件、数据库、域名与别名、SSL 证书，以及主要的 PHP、Nginx、监控、计划任务和 WordPress 运行设置。
- 备份历史、访问日志、安全事件历史、服务器级远程备份凭据和自定义命令类计划任务不会迁移。
- 搬家期间源网站会进入 HTTP 503 维护状态，避免迁移过程中继续产生新数据。
- 接收端不会覆盖同域名网站；迁移完成后仍需管理员自行检查业务并调整 DNS/CDN 解析。
- 搬家不是同一瞬间完成的整机快照。建议选择访问量较低的时段操作，并在切换域名解析前检查前台、后台、表单和订单等关键功能。

## 安全性

**一句话：随机入口、两层登录和自动限速能明显提高公网扫描与口令猜测的成本，但不能替代可信终端、强密码、及时更新、访问控制和可恢复备份。**

这是因为正常登录必须同时知道每台服务器独有的随机入口，并依次通过浏览器弹窗和网页登录。反复寻找入口或猜测密码还会触发自动限制。没有任何联网软件能承诺绝对不会被攻破，但 YUB WPanel 不会把安全只押在一个密码上。

---

更详细的安全机制：

**访问防护**
- 每台服务器都有独立的随机入口，能提高陌生扫描者找到登录页的难度
- 直接使用扫描工具试探面板，或在一分钟内访问 10 个不同的错误地址，会触发自动限制
- 即使找到入口，仍需依次通过浏览器弹窗和网页登录
- 登录过程使用 HTTPS 加密，错误提示也会尽量避免暴露服务器内部信息

**防爆破**
- 浏览器弹窗或网页登录在短时间内连续失败 5 次，会限制该来源 24 小时
- 网站登录和 SSH 也有独立保护；重复攻击时限制时间会逐步延长，最长 7 天

**站点隔离**
- 每个网站运行在独立的系统用户和 PHP-FPM Pool 下
- 每个网站使用独立的 MariaDB 数据库
- 独立用户和 PHP-FPM Pool 可降低单站故障横向影响；内核、数据库及宿主机资源仍是共享边界

**WordPress 专项防护**
- 自动识别反复尝试登录、批量寻找常见敏感文件和短时间访问大量不存在页面的行为
- 拒绝使用陌生域名访问服务器，减少网站和证书信息被探测的机会
- 将可疑访问按风险高低整理，并给出可疑来源、访问目标和处理建议；默认只分析，不会仅凭分析结果自动封禁
- 监测网站目录中新出现的可疑 PHP 文件和异常高频访问，留下安全事件记录
- 可单独限制高频爬虫，尽量减少对普通访客和日常后台操作的影响

**AI 运维诊断**
- 一次收集与网站故障有关的日志和运行状态，并可继续追问，减少新手来回寻找信息的困难
- 诊断只给出分析和排查建议，不会自动修改文件、数据库或服务器设置

**备份与异地保存**
- 网站备份可同步到另一台服务器或 S3 兼容存储；同步失败时可按设置保留本地副本

**更新安全**
- 更新前会使用 YUB WPanel 的独立 Ed25519 公钥验证安装包，避免使用被替换或损坏的文件
- 候选二进制报告的规范稳定版本必须与 Release 标签一致，避免旧的合法签名包被包装成更高版本重放
- 替换前会备份当前二进制和面板数据库；健康检查失败时会尝试回滚，但回滚并非整机快照，也可能失败

**代码透明**
- 100% 开源（`GPL-3.0-only`），代码可审查
- 运行时遥测默认关闭且没有预设端点；启用自定义端点后发送稳定伪匿名 ID 与版本，不发送业务内容
- 面板自身版本元数据默认来自 GitHub；WordPress 更新及管理员启用的外部功能会连接各自上游
- 无 Web Shell、无在线代码编辑功能
- 面板登录密码使用 bcrypt；运行所需的数据库和第三方服务凭据可能保存在 root-only 配置或数据库中

### 📖 安全深度解读

- **[安装脚本安全透明化报告](security/yub-wpanel-install-security.md)** — 逐段拆解 install.sh，回应"篡改密码、删除 Nginx、黑掉 WordPress"等指控
- **[运行时安全：多层防护机制](security/yub-wpanel-runtime-security.md)** — 源码层面解析六层纵深防御、更新签名校验、软件漏洞管理

## 安全测试

欢迎白帽和安全研究人员对本项目进行安全测试。如果你发现安全漏洞，请通过以下方式反馈：

- **公开反馈**：提交 [GitHub Issue](https://github.com/zangwp/yub-wpanel/issues)，在标题标注 `[安全]`
- **私下反馈**：通过 GitHub Security 标签页提交 Private Vulnerability Report
- 有效漏洞会在修复后于 Release Notes 中向报告者致谢

## 系统要求

| 项目 | 要求 |
|------|------|
| 操作系统 | Debian 13 (Trixie) 或 Ubuntu 24.04 LTS (Noble)；暂不自动延伸到其他大版本 |
| CPU | 1 核及以上 |
| 内存 | 1 GB 及以上（物理内存不超过 8 GB、未启用 Swap 且磁盘条件满足时，安装器可能创建 2 GB Swap） |
| 架构 | amd64/x86_64 或 arm64/aarch64（内核与 dpkg 用户空间架构必须一致） |

> 各云厂商定制镜像可能带来兼容性差异。请先保存日志并排查网络、APT、签名和系统版本。第三方重装项目不由 YUB WPanel 维护；重装系统会清除数据，只应在新机或已验证完整快照后使用。

## 为什么选择这些技术方案

**为什么锁定 Debian 13 与 Ubuntu 24.04 LTS？**

安装器会修改软件源、安装并配置 PHP、Nginx、MariaDB、Redis、Fail2ban 与 systemd 服务，因此兼容性必须按发行版版本验证。当前只接受 Debian 13/Trixie 和 Ubuntu 24.04/Noble，不会因为同属 Debian/Ubuntu 家族就放宽到未经测试的旧版或新版。Ubuntu 使用 Noble 自带的 PHP 8.3；Debian 使用经过固定 keyring 校验的 PHP 源。

**为什么锁定 PHP 8.3？**

WordPress 官方推荐 PHP 8.3 或更高版本。8.3 在 WordPress 生态中经过了最广泛的生产环境验证，拥有活跃支持周期，性能与安全性持续改进。固定版本意味着所有用户运行相同的 PHP 环境，问题可复现、可排查，避免因 PHP 版本差异导致的兼容性怪病。

**为什么是 MariaDB 而非 MySQL？**

WordPress 官方推荐 MariaDB 10.6 或更高版本。当前支持的 Debian 与 Ubuntu 系统源提供兼容版本。MariaDB 是由社区驱动的 GPL 分支，兼容 MySQL，并可直接获得发行版软件源提供的安全更新，无需添加第三方数据库仓库。

**为什么是自己编的 Go 二进制，不用 Docker/PM2？**

面板应用以单个静态 Go 二进制分发并由 `systemd` 守护，不需要 Docker/PM2。完整功能仍依赖安装器列出的系统服务和命令。它不与 Nginx 共用管理端口，也没有容器运行时开销。

## 运行组件

下表中的服务器栈组件通过 APT 安装；面板二进制来自已签名 GitHub Release，WordPress 与 WP-CLI 使用各自上游分发渠道：

| 组件 | 说明 |
|------|------|
| PHP 8.3 | Debian 使用经校验的 Ondřej Surý 源；Ubuntu 使用 Noble 原生包；独立 FPM Pool 隔离 |
| MariaDB | 当前发行版系统源 |
| Nginx | 当前发行版系统源 |
| Redis | 当前发行版系统源 |
| Fail2ban + nftables | 当前发行版系统源 |

## 技术架构

- **后端**：Go + Gin Web 框架，SQLite (WAL 模式)，端口 8443 (HTTPS/TLS)
- **前端**：HTML 模板 + TailwindCSS + Alpine.js + Chart.js
- **分发**：单一二进制文件（前端资源通过 `//go:embed` 编译内嵌），约 20 MB
- **安全**：面板不与 Nginx 反向代理耦合，独立 TLS 加密

## SSH 管理命令

从 `v2.1.0` 起，安装后面板提供 `b` 命令行工具，并兼容完全相同的大写入口 `B`：

| 命令 | 说明 |
|------|------|
| `b` 或 `B` | 查看面板信息 |
| `b restart` | 重启面板 |
| `b password` | 一键重置管理员账号密码 |
| `b info` | 查看版本/端口/入口 |
| `b status` | 查看运行状态 |
| `b unban` | 清空所有 IP 封禁（管理员被误封时紧急恢复） |

## 面板数据库备份与恢复

面板使用 SQLite 存储数据，每天凌晨 2:30 自动备份到 `/www/server/panel/backups/panel-db/`，保留最近 7 份。

### 面板正常时

在「面板设置」页面可以：
- 手动创建备份
- 下载备份文件到本地
- 从备份恢复（恢复前自动创建安全备份，恢复后面板自动重启）
- 删除备份

### 面板无法启动时的恢复步骤

如果面板恢复数据库后无法启动，或数据库损坏导致面板无法运行，请通过 SSH 手动恢复：

```bash
# 1. 查看可用备份
ls -lh /www/server/panel/backups/panel-db/

# 2. 停止面板
systemctl stop yub-wpanel

# 3. 备份当前损坏的数据库（以防万一）
cp /www/server/panel/panel.db /www/server/panel/panel.db.broken

# 4. 用备份替换当前数据库（替换为实际的备份文件名）
cp /www/server/panel/backups/panel-db/panel_20260107_023000.db /www/server/panel/panel.db

# 5. 启动面板
systemctl start yub-wpanel

# 6. 检查是否正常
systemctl status yub-wpanel
journalctl -u yub-wpanel -n 20
```

### 重装面板后导入备份

如果需要完全重装面板并恢复数据：

```bash
# 1. 先保存备份文件到安全位置
cp -r /www/server/panel/backups/panel-db/ /root/panel-db-backup/

# 2. 重装面板（选择"卸载后重新安装"，保留网站数据）

# 3. 安装完成后停止面板
systemctl stop yub-wpanel

# 4. 用备份替换新数据库
cp /root/panel-db-backup/panel_20260107_023000.db /www/server/panel/panel.db

# 5. 启动面板（自动执行数据库升级）
systemctl start yub-wpanel
```

> **注意**：这里只支持同一 YUB WPanel 分发链且仍在公开支持范围内的 SQLite 备份。其它发行身份的备份不得直接恢复；该数据库备份也不包含网站文件、MariaDB 数据或面板目录外配置。详见[升级兼容性说明](docs/upgrade-compatibility.md)。

## 项目结构

```
├── main.go               # 程序入口
├── config/               # 全局配置管理
├── database/             # SQLite 连接与迁移
├── models/               # 数据结构
├── router/               # 路由 + 页面分发
├── middleware/            # BasicAuth / Session / CSRF / 登录限流
├── handlers/             # HTTP 处理器
├── executor/             # 任务执行器
├── collector/            # 系统指标采集
├── templates/            # HTML 模板
├── static/               # 已生成并嵌入的 CSS / JS / Logo
├── assets/               # 品牌、社区图片与前端源文件
├── deploy/cloudflare/    # 短安装域名的可审计 Worker 配置
├── install.sh            # 一键安装脚本
├── install-cn.sh         # 国内入口及 bootstrap.sh 的共享验签源
├── tests/                # 安装器与跨包约束测试
├── security/             # 安全说明文档
└── yub-wpanel-optimizer/   # WordPress 配套插件
```

为什么部分 Go 测试仍留在根目录、哪些重复资产是有意保留的，见[仓库结构说明](docs/repository-layout.md)。

## YUB WPanel 开源许可

GNU GPL v3.0 only（SPDX：`GPL-3.0-only`）

YUB WPanel 依据 GNU GPL v3.0 only 发布，由 zangwp 维护。完整许可条款见
[`LICENSE`](LICENSE)，项目声明见 [`NOTICE.md`](NOTICE.md)，第三方组件许可见
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) 及每个 Release 附带的签名许可归档。
