# 验签安装 / Verified installation

YUB WPanel 的正式 Release 为面板二进制、`install.sh`、`install-cn.sh` 和第三方许可归档分别提供 SHA-256 清单及 Ed25519 签名。生产服务器不要把可变分支上的脚本直接通过管道交给 root shell；应先验证安装器，再执行它。安装器随后还会用同一把公钥验证同版本的面板二进制和许可归档，并把许可材料安装到 `/usr/share/doc/yub-wpanel`。

The official YUB WPanel release provides a SHA-256 manifest and Ed25519 signature for the panel binary, `install.sh`, `install-cn.sh`, and the third-party license archive. On a production server, do not pipe a mutable branch script directly into a root shell. Verify the installer first; the installer then verifies the same-version panel binary and license archive with the same public key and installs the license materials under `/usr/share/doc/yub-wpanel`.

当前发布公钥原始值：

```text
7351099720eeaf147f4894bc313a5456c01bbd29ad7d401ab6869bc7f7f92af5
```

在信任该公钥之前，最好通过另一个独立渠道核对它。签名证明下载内容与该密钥一致，但不证明软件没有漏洞，也不覆盖 GitHub 自动生成的源码压缩包。签名本身也不提供“这是最新版本”的在线证明；使用第三方反代时还应核对 Release 版本。正式发布的安装器会要求面板二进制版本与安装器固定版本完全一致，并额外拒绝低于最低安全版本的二进制；许可归档也必须包含唯一、单行且完全相同的根级 `RELEASE_VERSION`，从而拒绝较旧但签名有效的许可归档。

Cross-check this key through an independent channel before trusting it. A valid signature proves that the downloaded content matches this key; it does not prove that the software is vulnerability-free, and it does not cover GitHub-generated source archives. A signature alone is not an online freshness proof; when using a third-party proxy, also confirm the Release version. A released installer requires the panel binary version to exactly match its pinned version and additionally rejects binaries below the minimum security version. The license archive must also contain exactly one root-level, single-line `RELEASE_VERSION` equal to that pinned version, so an older but validly signed archive is rejected.

## 标准安装 / Standard installation

以 root 身份在全新的 Debian 13 amd64 服务器执行：

```bash
apt-get update
apt-get install -y wget ca-certificates openssl

(
  set -euo pipefail
  umask 077
  workdir="$(mktemp -d)"
  trap 'rm -rf -- "$workdir"' EXIT
  cd "$workdir"

  version='v2.0.1'
  base="https://github.com/zangwp/yub-wpanel/releases/download/$version"
  wget --no-config --https-only --no-hsts "$base/install.sh"
  wget --no-config --https-only --no-hsts "$base/install.sh.sha256"
  wget --no-config --https-only --no-hsts "$base/install.sh.sha256.sig"

  printf '%s\n' \
    '-----BEGIN PUBLIC KEY-----' \
    'MCowBQYDK2VwAyEAc1EJlyDurxR/SJS8MTpUVsAbvSmtfUAatoabx/f5KvU=' \
    '-----END PUBLIC KEY-----' > release-public-key.pem

  openssl pkeyutl -verify -rawin -pubin \
    -inkey release-public-key.pem \
    -in install.sh.sha256 \
    -sigfile install.sh.sha256.sig
  sha256sum --check --strict install.sh.sha256
  bash install.sh
)
```

任何验证命令失败都会终止子 shell，不会执行安装器。你也可以在最后一行之前先阅读 `install.sh`。

Any verification failure stops the subshell before the installer runs. You may also inspect `install.sh` before executing the final line.

## 国内入口 / China-friendly entry

国内入口会优先选择国内 Debian/PHP 软件源；它不会绕过签名验证。把上面命令中的三个 `install.sh` 文件名改为 `install-cn.sh`，最后执行：

```bash
YUB_WPANEL_GITHUB_PROXY='https://你信任的反代地址' bash install-cn.sh
```

如果 GitHub Release 无法直连，可让前三个下载 URL 也经过你选择的 HTTPS 反代。反代的 URL 拼接格式因服务而异。签名验证失败时不要继续安装。

The China-friendly entry prefers regional Debian/PHP mirrors and does not bypass signature verification. Replace the three `install.sh` filenames above with `install-cn.sh`. If direct GitHub Release access is unavailable, route those downloads through an HTTPS proxy you selected, then run `install-cn.sh` with the same proxy setting. Do not continue after a signature failure.

## 本地发布包 / Local release bundle

离线提供面板资产时，以下六个文件必须来自与 `install.sh` 相同的固定 Release，并放在 `install.sh` 同目录：

```text
yub-wpanel
yub-wpanel.sha256
yub-wpanel.sha256.sig
yub-wpanel-third-party-licenses.tar.gz
yub-wpanel-third-party-licenses.tar.gz.sha256
yub-wpanel-third-party-licenses.tar.gz.sha256.sig
```

安装器会在复制或计算哈希前对这六个本地候选执行硬字节上限，并要求许可归档的唯一 `RELEASE_VERSION` 与安装器固定版本完全一致。这只能省去面板和许可资产下载。APT 软件包、仓库密钥和 WordPress 包仍可能需要网络，因此当前不承诺完整离线安装。

All six files must come from the same fixed release as `install.sh`. Before copying or hashing them, the installer applies hard byte limits and requires exactly one license-archive `RELEASE_VERSION` equal to the installer's pinned version. This only avoids downloading the panel and license assets; APT packages, repository keys, and WordPress packages may still require network access, so a fully offline installation is not currently supported.

## v2.0.0 升级桥接 / v2.0.0 upgrade bridge

从 `v2.0.0` 升级到 `v2.0.1` 时，必须同时把固定 `v2.0.1` Release 的安装器、面板二进制和许可归档三组签名资产（共九个文件）放在同一私有目录。不要使用 `v2.0.0` 内置在线更新器，也不要把下面的固定版本 URL 改成 `latest`。以 root 执行以下命令，先验证安装器与面板；安装器会再次验证面板与许可归档，然后在菜单中选择“继续/修复安装”：

```bash
apt-get update
apt-get install -y wget ca-certificates openssl

(
  set -euo pipefail
  umask 077
  workdir="$(mktemp -d)"
  trap 'rm -rf -- "$workdir"' EXIT
  cd "$workdir"

  version='v2.0.1'
  base="https://github.com/zangwp/yub-wpanel/releases/download/$version"
  for asset in \
    install.sh install.sh.sha256 install.sh.sha256.sig \
    yub-wpanel yub-wpanel.sha256 yub-wpanel.sha256.sig \
    yub-wpanel-third-party-licenses.tar.gz \
    yub-wpanel-third-party-licenses.tar.gz.sha256 \
    yub-wpanel-third-party-licenses.tar.gz.sha256.sig; do
    wget --no-config --https-only --no-hsts -O "$asset" "$base/$asset"
  done

  printf '%s\n' \
    '-----BEGIN PUBLIC KEY-----' \
    'MCowBQYDK2VwAyEAc1EJlyDurxR/SJS8MTpUVsAbvSmtfUAatoabx/f5KvU=' \
    '-----END PUBLIC KEY-----' > release-public-key.pem

  for checksum in install.sh.sha256 yub-wpanel.sha256; do
    openssl pkeyutl -verify -rawin -pubin \
      -inkey release-public-key.pem \
      -in "$checksum" \
      -sigfile "$checksum.sig"
    sha256sum --check --strict "$checksum"
  done

  bash install.sh
)
```

详细原因和升级边界见[升级兼容性说明](upgrade-compatibility.md)。

For the `v2.0.0` to `v2.0.1` upgrade, use the fixed-version command above to place all nine installer, panel, and license-archive assets in one private directory, then select the repair option. The installer re-verifies both the panel and license archive before deployment. Do not use the updater built into `v2.0.0` or replace the fixed URL with `latest`. See the [upgrade compatibility note](upgrade-compatibility.md) for the rationale and boundaries.
