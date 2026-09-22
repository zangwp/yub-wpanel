package tests

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/zangwp/yub-wpanel/config"
)

func TestInstallerReleasePublicKeyMatchesApplication(t *testing.T) {
	const prefix = "RELEASE_PUBLIC_KEY_PEM='"
	const pemEnd = "-----END PUBLIC KEY-----"
	for _, path := range []string{installScriptPath, installCNScriptPath} {
		script := readInstallScript(t, path)
		start := strings.Index(script, prefix)
		if start < 0 {
			t.Fatalf("%s is missing the embedded release public key PEM", path)
		}
		start += len(prefix)
		end := strings.Index(script[start:], pemEnd)
		if end < 0 {
			t.Fatalf("%s release public key PEM terminator was not found", path)
		}
		end += len(pemEnd)
		block, rest := pem.Decode([]byte(script[start : start+end]))
		if block == nil || len(rest) != 0 {
			t.Fatalf("%s release public key is not one valid PEM block", path)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			t.Fatalf("parse %s public key: %v", path, err)
		}
		publicKey, ok := parsed.(ed25519.PublicKey)
		if !ok {
			t.Fatalf("%s public key type = %T, want Ed25519", path, parsed)
		}
		if got := hex.EncodeToString(publicKey); got != config.ReleasePublicKeyHex {
			t.Fatalf("%s public key = %s, application public key = %s", path, got, config.ReleasePublicKeyHex)
		}
		if !strings.Contains(script, `RELEASE_PUBLIC_KEY_HEX="`+config.ReleasePublicKeyHex+`"`) {
			t.Fatalf("%s raw release public key does not match the application", path)
		}
	}
}

func TestInstallerPreflightConfigPassesCurrentValidation(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	const startMarker = `cat > "$preflight_config" << PREFLIGHTEOF` + "\n"
	const endMarker = "\nPREFLIGHTEOF"
	start := strings.Index(script, startMarker)
	if start < 0 {
		t.Fatal("install.sh preflight config heredoc was not found")
	}
	start += len(startMarker)
	end := strings.Index(script[start:], endMarker)
	if end < 0 {
		t.Fatal("install.sh preflight config heredoc terminator was not found")
	}
	data := strings.ReplaceAll(script[start:start+end], "$INSTALL_WORKDIR", "/tmp/yub-wpanel-preflight-test")
	var cfg config.Config
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("parse installer preflight config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("installer preflight config is incompatible with current config validation: %v", err)
	}
}

func TestCNBootstrapOnlyExecutesSignedReleaseInstaller(t *testing.T) {
	script := readInstallScript(t, installCNScriptPath)
	for _, required := range []string{
		`BOOTSTRAP_RELEASE_VERSION="__YUB_WPANEL_RELEASE_VERSION__"`,
		`https://github.com/zangwp/yub-wpanel/releases/download/${BOOTSTRAP_RELEASE_VERSION}/install.sh`,
		`download_install_script "${script_url}.sha256"`,
		`download_install_script "${script_url}.sha256.sig"`,
		`openssl pkeyutl -verify -pubin`,
		`install.sh|'*install.sh') ;;`,
		`actual_sha=$(sha256sum "$script_file"`,
		`copy_local_install_script_bundle "$SCRIPT_DIR"`,
		`verify_install_script_bundle "$INSTALL_SCRIPT" "$INSTALL_SHA256_FILE" "$INSTALL_SIGNATURE_FILE"`,
		`grep -qFx "INSTALLER_RELEASE_VERSION=\"${BOOTSTRAP_RELEASE_VERSION}\""`,
		`INSTALLER_ASSET_MAX_BYTES=$((4 * 1024 * 1024))`,
		`CHECKSUM_ASSET_MAX_BYTES=$((4 * 1024))`,
		`SIGNATURE_ASSET_MAX_BYTES=64`,
		`--max-filesize "$max_bytes"`,
		`head -c "$((max_bytes + 1))"`,
		`file_size_within_limit "$script_dir/install.sh" "$INSTALLER_ASSET_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/install.sh.sha256" "$CHECKSUM_ASSET_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/install.sh.sha256.sig" "$SIGNATURE_ASSET_MAX_BYTES"`,
		`download_install_script "$script_url" "$INSTALL_SCRIPT" "$INSTALLER_ASSET_MAX_BYTES"`,
		`download_install_script "${script_url}.sha256" "$INSTALL_SHA256_FILE" "$CHECKSUM_ASSET_MAX_BYTES"`,
		`download_install_script "${script_url}.sha256.sig" "$INSTALL_SIGNATURE_FILE" "$SIGNATURE_ASSET_MAX_BYTES"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("install-cn.sh missing signed bootstrap guard %q", required)
		}
	}
	for _, forbidden := range []string{
		"raw.githubusercontent.com/zangwp/yub-wpanel/main/install.sh",
		"cdn.jsdelivr.net/gh/zangwp/yub-wpanel@main/install.sh",
		`exec bash "$SCRIPT_DIR/install.sh"`,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("install-cn.sh still contains unsigned bootstrap path %q", forbidden)
		}
	}
	execute := requiredIndex(t, script, `bash "$INSTALL_SCRIPT" --prefer-cn "$@"`)
	lastVerify := strings.LastIndex(script[:execute], `verify_install_script_bundle "$INSTALL_SCRIPT"`)
	if lastVerify < 0 {
		t.Fatal("install-cn.sh executes install.sh without a preceding signature verification path")
	}
	localCopy := extractShellFunction(t, script, "copy_local_install_script_bundle", "download_install_script_bundle")
	lastLocalLimit := strings.LastIndex(localCopy, `file_size_within_limit `)
	firstLocalCopy := strings.Index(localCopy, `install -m 0600 `)
	if lastLocalLimit < 0 || firstLocalCopy < 0 || lastLocalLimit >= firstLocalCopy {
		t.Fatalf("install-cn local assets must all be size-checked before copying: last_limit=%d first_copy=%d", lastLocalLimit, firstLocalCopy)
	}
}

func TestInstallerVerifiesReleaseBeforeExecutionAndDeployment(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	for _, required := range []string{
		`openssl pkeyutl -verify -pubin`,
		`[[ "$(wc -c < "$sig_file" | tr -d '[:space:]')" == "64" ]]`,
		`[[ "$expected_sha" =~ ^[0-9a-fA-F]{64}$ ]]`,
		`actual_sha=$(sha256sum "$asset_file"`,
		`download_file "${binary_url}.sha256"`,
		`download_file "${binary_url}.sha256.sig"`,
		`download_file "${license_url}.sha256"`,
		`download_file "${license_url}.sha256.sig"`,
		`timeout 20s "$PANEL_CANDIDATE" --info --config "$preflight_config"`,
		`INSTALLER_RELEASE_VERSION="__YUB_WPANEL_RELEASE_VERSION__"`,
		`MIN_PANEL_VERSION="v2.0.1"`,
		`panel_version_at_least "$candidate_version" "$MIN_PANEL_VERSION"`,
		`[[ "$candidate_version" == "$INSTALLER_RELEASE_VERSION" ]]`,
		`verify_license_release_bundle`,
		`install_release_license_documentation`,
		`LICENSE_DOC_DIR="/usr/share/doc/yub-wpanel"`,
		`PANEL_ASSET_MAX_BYTES=$((256 * 1024 * 1024))`,
		`CHECKSUM_ASSET_MAX_BYTES=$((4 * 1024))`,
		`SIGNATURE_ASSET_MAX_BYTES=64`,
		`LICENSE_ARCHIVE_MAX_BYTES=$((64 * 1024 * 1024))`,
		`PHP_KEYRING_MAX_BYTES=$((1 * 1024 * 1024))`,
		`WORDPRESS_ZIP_MAX_BYTES=$((256 * 1024 * 1024))`,
		`PUBLIC_IP_MAX_BYTES=$((4 * 1024))`,
		`--max-filesize "$max_bytes"`,
		`head -c "$((max_bytes + 1))"`,
		`download_file "$binary_url" "$PANEL_CANDIDATE" 180 "$PANEL_ASSET_MAX_BYTES"`,
		`download_file "${binary_url}.sha256" "$PANEL_SHA256_FILE" 60 "$CHECKSUM_ASSET_MAX_BYTES"`,
		`download_file "${binary_url}.sha256.sig" "$PANEL_SIGNATURE_FILE" 60 "$SIGNATURE_ASSET_MAX_BYTES"`,
		`download_file "$license_url" "$LICENSE_ARCHIVE" 120 "$LICENSE_ARCHIVE_MAX_BYTES"`,
		`download_file "${license_url}.sha256" "$LICENSE_SHA256_FILE" 60 "$CHECKSUM_ASSET_MAX_BYTES"`,
		`download_file "${license_url}.sha256.sig" "$LICENSE_SIGNATURE_FILE" 60 "$SIGNATURE_ASSET_MAX_BYTES"`,
		`download_file "$PHP_KEY_URL" "$tmp_key" 20 "$PHP_KEYRING_MAX_BYTES"`,
		`download_file "https://wordpress.org/latest.zip" "$WP_ZIP_TMP" 60 "$WORDPRESS_ZIP_MAX_BYTES"`,
		`download_file "https://ip.sb" "$PUBLIC_IP_FILE" 15 "$PUBLIC_IP_MAX_BYTES"`,
		`download_file "https://ifconfig.me/ip" "$PUBLIC_IP_FILE" 15 "$PUBLIC_IP_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/yub-wpanel" "$PANEL_ASSET_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/yub-wpanel.sha256" "$CHECKSUM_ASSET_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/yub-wpanel.sha256.sig" "$SIGNATURE_ASSET_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/yub-wpanel-third-party-licenses.tar.gz" "$LICENSE_ARCHIVE_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/yub-wpanel-third-party-licenses.tar.gz.sha256" "$CHECKSUM_ASSET_MAX_BYTES"`,
		`file_size_within_limit "$script_dir/yub-wpanel-third-party-licenses.tar.gz.sha256.sig" "$SIGNATURE_ASSET_MAX_BYTES"`,
		`./RELEASE_VERSION`,
		`[[ "$archive_release_version" == "$INSTALLER_RELEASE_VERSION" ]]`,
		`"cron_file": "/etc/cron.d/yub_wpanel_cron"`,
		`"service_name": "yub-wpanel"`,
		`"service_path": "/etc/systemd/system/yub-wpanel.service"`,
		`"binary_path": "/usr/local/bin/yub-wpanel"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("install.sh missing release verification guard %q", required)
		}
	}

	prepare := extractShellFunction(t, script, "prepare_panel_candidate", "create_repair_backup")
	if verify := strings.Index(prepare, `verify_complete_release_bundle`); verify < 0 {
		t.Fatal("candidate preparation does not verify the release bundle")
	} else if execute := strings.LastIndex(prepare, "preflight_panel_candidate"); execute < verify {
		t.Fatalf("candidate --info preflight offset=%d must follow verification offset=%d", execute, verify)
	}

	deploy := script[requiredIndex(t, script, `log_info "部署面板二进制..."`):]
	verify := requiredIndex(t, deploy, `verify_complete_release_bundle`)
	atomicInstall := requiredIndex(t, deploy, `atomic_install_managed_file "$PANEL_CANDIDATE" "$BIN_PATH" 0755`)
	if verify >= atomicInstall {
		t.Fatalf("deployment verification order invalid: verify=%d atomic_install=%d", verify, atomicInstall)
	}
	atomicHelper := extractShellFunction(t, script, "atomic_install_managed_file", "repair_rollback")
	for _, required := range []string{
		`mktemp "${target_dir}/.${target_base}.yub-install.XXXXXXXX"`,
		`sync -f "$ATOMIC_STAGE_PATH"`,
		`mv -f -- "$ATOMIC_STAGE_PATH" "$target_path"`,
		`sync -f "$target_dir"`,
	} {
		requiredIndex(t, atomicHelper, required)
	}

	repair := script[requiredIndex(t, script, "if $REPAIR_MODE; then\n    prepare_panel_candidate"):]
	repairVerify := requiredIndex(t, repair, `verify_complete_release_bundle`)
	repairExecute := requiredIndex(t, repair, `repair_check=$($PANEL_CANDIDATE --repair-config-check`)
	if repairVerify >= repairExecute {
		t.Fatalf("repair execution offset=%d must follow verification offset=%d", repairExecute, repairVerify)
	}
	localCopy := extractShellFunction(t, script, "copy_local_release_bundle", "download_release_bundle")
	lastLocalLimit := strings.LastIndex(localCopy, `file_size_within_limit `)
	firstLocalCopy := strings.Index(localCopy, `install -m 0600 `)
	if lastLocalLimit < 0 || firstLocalCopy < 0 || lastLocalLimit >= firstLocalCopy {
		t.Fatalf("local release assets must all be size-checked before copying: last_limit=%d first_copy=%d", lastLocalLimit, firstLocalCopy)
	}
}

func TestInstallerDownloadHelpersEnforceActualStreamLimitAndCleanPartialFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires bash, timeout, head, and GNU stat")
	}

	mainScript := readInstallScript(t, installScriptPath)
	cnScript := readInstallScript(t, installCNScriptPath)
	mainFunctions := extractShellFunction(t, mainScript, "file_size_within_limit", "download_file") + "\n" +
		extractShellFunction(t, mainScript, "download_file", "verify_signed_release_asset")
	cnFunctions := extractShellFunction(t, cnScript, "file_size_within_limit", "download_install_script") + "\n" +
		extractShellFunction(t, cnScript, "download_install_script", "verify_install_script_bundle")

	for _, test := range []struct {
		name      string
		functions string
		call      string
	}{
		{name: "main", functions: mainFunctions, call: `download_file "https://example.invalid/asset" "$DEST" 5 8`},
		{name: "cn", functions: cnFunctions, call: `download_install_script "https://example.invalid/install.sh" "$DEST" 8`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			fakeDownloader := "#!/bin/sh\n" +
				`dd if=/dev/zero bs=1 count="${FAKE_BYTES:?}" 2>/dev/null` + "\n"
			for _, name := range []string{"curl", "wget"} {
				path := filepath.Join(fakeBin, name)
				if err := os.WriteFile(path, []byte(fakeDownloader), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			dest := filepath.Join(t.TempDir(), "download")
			shell := `set -euo pipefail
` + test.functions + `
DEST=` + strconv.Quote(dest) + `
export FAKE_BYTES=9
if ` + test.call + `; then
    exit 91
fi
test ! -e "$DEST"
export FAKE_BYTES=8
` + test.call + `
test "$(stat -c '%s' -- "$DEST")" = 8
`
			cmd := exec.Command("bash", "-c", shell)
			cmd.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("download limit fixture failed: %v\n%s", err, output)
			}
		})
	}
}

func TestLicenseArchiveReleaseVersionMustBeUniqueAndExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires bash and tar")
	}

	script := readInstallScript(t, installScriptPath)
	verifyFunction := extractShellFunction(t, script, "verify_license_release_bundle", "verify_complete_release_bundle")
	for _, test := range []struct {
		name     string
		versions []string
		wantOK   bool
	}{
		{name: "matching", versions: []string{"v2.0.1"}, wantOK: true},
		{name: "stale-signed-archive", versions: []string{"v2.0.0"}, wantOK: false},
		{name: "duplicate-marker", versions: []string{"v2.0.1", "v2.0.1"}, wantOK: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "licenses.tar.gz")
			writeLicenseArchiveFixture(t, archive, test.versions)
			shell := `set -euo pipefail
INSTALL_WORKDIR=` + strconv.Quote(dir) + `
INSTALLER_RELEASE_VERSION=v2.0.1
LICENSE_ARCHIVE_MAX_BYTES=67108864
LICENSE_RELEASE_VERSION_FILE="$INSTALL_WORKDIR/RELEASE_VERSION"
PROJECT_LICENSE_FILE="$INSTALL_WORKDIR/LICENSE.out"
PROJECT_NOTICE_FILE="$INSTALL_WORKDIR/NOTICE.out"
THIRD_PARTY_NOTICE_FILE="$INSTALL_WORKDIR/THIRD_PARTY.out"
verify_signed_release_asset() { return 0; }
` + verifyFunction + "\n"
			if test.wantOK {
				shell += `verify_license_release_bundle ` + strconv.Quote(archive) + ` ignored ignored
test "$(cat "$LICENSE_RELEASE_VERSION_FILE")" = v2.0.1
`
			} else {
				shell += `if verify_license_release_bundle ` + strconv.Quote(archive) + ` ignored ignored; then exit 92; fi
`
			}
			cmd := exec.Command("bash", "-c", shell)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("license archive fixture failed: %v\n%s", err, output)
			}
		})
	}
}

func writeLicenseArchiveFixture(t *testing.T, path string, versions []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	members := []struct {
		name string
		body string
	}{
		{name: "./LICENSE", body: "project license\n"},
		{name: "./NOTICE.md", body: "project notice\n"},
		{name: "./THIRD_PARTY_NOTICES.md", body: "third party notice\n"},
		{name: "./go-toolchain/LICENSE", body: "go license\n"},
		{name: "./go-toolchain/VERSION", body: "go1.26.8\n"},
		{name: "./adminer-6.0.1/LICENSE-APACHE-2.0.txt", body: "apache license\n"},
		{name: "./adminer-6.0.1/NOTICE.txt", body: "adminer notice\n"},
	}
	for _, version := range versions {
		members = append(members, struct {
			name string
			body string
		}{name: "./RELEASE_VERSION", body: version + "\n"})
	}
	for _, member := range members {
		header := &tar.Header{Name: member.name, Mode: 0o644, Size: int64(len(member.body))}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(member.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerRejectsOldOrMalformedSignedPanelVersions(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	versionCheck := extractShellFunction(t, script, "panel_version_at_least", "write_panel_service_unit")
	for _, required := range []string{
		`^([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})$`,
		`10#$actual_major`,
		`10#$actual_minor`,
		`10#$actual_patch`,
	} {
		if !strings.Contains(versionCheck, required) {
			t.Errorf("panel version guard is missing %q", required)
		}
	}
}

func TestInstallerPlatformAndArtifactPreflightPrecedeSystemWrites(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	cnScript := readInstallScript(t, installCNScriptPath)
	mainStart := requiredIndex(t, script, "if [[ $EUID -ne 0 ]]")
	main := script[mainStart:]
	platform := requiredIndex(t, main, "assert_supported_platform\n")
	workdir := requiredIndex(t, main, "init_install_workdir\n")
	artifact := requiredIndex(t, main, "prepare_panel_candidate\n")
	lock := requiredIndex(t, main, "exec 9>/run/lock/yub-wpanel-install.lock")
	if !(platform < workdir && workdir < artifact && artifact < lock) {
		t.Fatalf("early safety order invalid: platform=%d workdir=%d artifact=%d first_system_write=%d", platform, workdir, artifact, lock)
	}
	for _, required := range []string{
		`[[ "$os_id" == "debian" ]]`,
		`[[ "$version_id" == "13" ]]`,
		`x86_64|amd64) ;;`,
		`[[ "$dpkg_arch" == "amd64" ]]`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("install.sh missing platform restriction %q", required)
		}
	}
	cnPlatform := requiredIndex(t, cnScript, "assert_bootstrap_platform\n")
	cnWorkdir := requiredIndex(t, cnScript, "CN_WORKDIR=$(mktemp -d")
	if cnPlatform >= cnWorkdir {
		t.Fatalf("install-cn platform check offset=%d must precede temporary write offset=%d", cnPlatform, cnWorkdir)
	}
}

func TestInstallerUsesPrivateWorkdirsAndBoundedDownloads(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	cnScript := readInstallScript(t, installCNScriptPath)
	for name, content := range map[string]string{"install.sh": script, "install-cn.sh": cnScript} {
		for _, required := range []string{
			"mktemp -d /tmp/yub-wpanel-",
			"chmod 0700",
			"--connect-timeout",
			"--no-hsts",
			"--read-timeout=30",
			"--retry",
			"--speed-limit 1024",
		} {
			if !strings.Contains(content, required) {
				t.Errorf("%s missing temporary/download hardening %q", name, required)
			}
		}
	}
	for _, forbidden := range []string{
		`/tmp/yub-wpanel.$$.candidate`,
		`/tmp/yub-wpanel-debian-apt-update.log`,
		`/tmp/debsuryorg-archive-keyring.deb`,
		`/tmp/yub-wpanel-apt-update.log`,
		`"${WP_ZIP}.download"`,
		`"${BIN_PATH}.repair.$$"`,
		`"${DB_PATH}.repair-rollback.$$"`,
		`$INSTALL_WORKDIR/yub-wpanel.deploy`,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("install.sh still uses predictable temporary path %q", forbidden)
		}
	}
}

func TestPHPKeyringPackageIsPinnedBeforeExecution(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	start := requiredIndex(t, script, "configure_php_source() {")
	endRelative := requiredIndex(t, script[start:], "select_php_source() {")
	configure := script[start : start+endRelative]

	for _, required := range []string{
		`DEBSURY_KEYRING_PACKAGE="debsuryorg-archive-keyring"`,
		`DEBSURY_KEYRING_VERSION="2025.11.18"`,
		`DEBSURY_KEYRING_SHA256="7511384559c9ddf1d5ce5f60be429ae9d4e7d01d9480d6f1b7a30c0810cf8b60"`,
		`actual_sha=$(sha256sum "$tmp_key"`,
		`[[ "$actual_sha" != "$DEBSURY_KEYRING_SHA256" ]]`,
		`package_name=$(dpkg-deb -f "$tmp_key" Package`,
		`package_version=$(dpkg-deb -f "$tmp_key" Version`,
		`package_arch=$(dpkg-deb -f "$tmp_key" Architecture`,
		`[[ "$package_name" != "$DEBSURY_KEYRING_PACKAGE" ]]`,
		`[[ "$package_version" != "$DEBSURY_KEYRING_VERSION" ]]`,
		`[[ "$package_arch" != "all" ]]`,
		`rm -f "$tmp_key"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("install.sh missing PHP keyring guard %q", required)
		}
	}

	hashCheck := requiredIndex(t, configure, `[[ "$actual_sha" != "$DEBSURY_KEYRING_SHA256" ]]`)
	metadataCheck := requiredIndex(t, configure, `[[ "$package_name" != "$DEBSURY_KEYRING_PACKAGE" ]]`)
	packageInstall := requiredIndex(t, configure, `dpkg -i "$tmp_key"`)
	if !(hashCheck < metadataCheck && metadataCheck < packageInstall) {
		t.Fatalf("PHP keyring validation must precede package execution: hash=%d metadata=%d install=%d", hashCheck, metadataCheck, packageInstall)
	}
	if strings.Contains(configure, "复用本机已有 keyring") {
		t.Fatal("PHP source bootstrap must not reuse an unverified pre-existing keyring")
	}
}
