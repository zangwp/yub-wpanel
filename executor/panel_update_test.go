package executor

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/config"
	"github.com/zangwp/yub-wpanel/database"
)

func TestHealthCheckVersionRequiresRunningTargetVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"v1.2.3"}`))
	}))
	defer server.Close()

	if err := healthCheckVersion(server.URL, "1.2.3"); err != nil {
		t.Fatalf("matching version rejected: %v", err)
	}
	if err := healthCheckVersion(server.URL, "v1.2.4"); err == nil {
		t.Fatal("old running version accepted as the update target")
	}
}

func TestHealthCheckVersionRejectsMissingVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	if err := healthCheckVersion(server.URL, "v1.2.3"); err == nil {
		t.Fatal("health response without a process version was accepted")
	}
}

func TestPanelDownloadRejectsKnownContentLengthOverLimitWithoutLeavingFile(t *testing.T) {
	payload := strings.Repeat("x", 33)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "oversized-known")
	err := downloadFileWithProgress(server.URL, dest, time.Second, 32, nil)
	if err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("known oversized response error = %v", err)
	}
	if _, statErr := os.Lstat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("known oversized response left destination behind: %v", statErr)
	}
}

func TestPanelDownloadRejectsChunkedStreamOverLimitAndCleansPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test response does not support flushing")
			return
		}
		_, _ = w.Write([]byte("12345678"))
		flusher.Flush()
		_, _ = w.Write([]byte("9"))
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "oversized-chunked")
	err := downloadFileWithProgress(server.URL, dest, time.Second, 8, nil)
	if err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("chunked oversized response error = %v", err)
	}
	if _, statErr := os.Lstat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("chunked oversized response left destination behind: %v", statErr)
	}
}

func TestPanelDownloadAcceptsStreamAtHardLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("12345678"))
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "at-limit")
	if err := downloadFileWithProgress(server.URL, dest, time.Second, 8, nil); err != nil {
		t.Fatalf("response at hard limit rejected: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "12345678" {
		t.Fatalf("downloaded data=%q err=%v", data, err)
	}
}

func TestLatestReleaseMetadataRejectsKnownAndChunkedOversize(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "chunked"}[chunked], func(t *testing.T) {
			payload := []byte(`{"tag_name":"v2.0.2"}` + strings.Repeat(" ", int(githubReleaseMetadataMaxBytes)))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if !chunked {
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
					_, _ = w.Write(payload)
					return
				}
				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Error("test response does not support flushing")
					return
				}
				_, _ = w.Write(payload[:1])
				flusher.Flush()
				_, _ = w.Write(payload[1:])
			}))
			defer server.Close()

			if _, err := FetchLatestPanelRelease(server.URL); err == nil || !strings.Contains(err.Error(), "字节上限") {
				t.Fatalf("oversized release metadata error = %v", err)
			}
		})
	}
}

func TestManagedHealthCheckRejectsSpoofedResponderWithoutServiceIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"v1.2.3"}`))
	}))
	defer server.Close()
	identityChecks := 0
	err := healthCheckManagedPanelVersion(server.URL, "v1.2.3", func(connection panelHealthConnection) error {
		identityChecks++
		if connection.ServerPort <= 0 || net.ParseIP(connection.ServerIP) == nil {
			t.Fatalf("health connection was not captured: %+v", connection)
		}
		return errors.New("health responder is not the systemd MainPID")
	})
	if err == nil || identityChecks != 1 {
		t.Fatalf("spoofed health result err=%v identity_checks=%d", err, identityChecks)
	}
	if err := healthCheckManagedPanelVersion(server.URL, "v1.2.3", func(connection panelHealthConnection) error {
		identityChecks++
		if !net.ParseIP(connection.ServerIP).IsLoopback() {
			return errors.New("health responder left loopback")
		}
		return nil
	}); err != nil {
		t.Fatalf("bound health check rejected: %v", err)
	}
}

func TestPanelServiceMainProcessIdentityBindsActivePIDPathAndInode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc executable identity")
	}
	pid := os.Getpid()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	clientConnection, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConnection.Close()
	var serverConnection net.Conn
	select {
	case serverConnection = <-accepted:
		defer serverConnection.Close()
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out accepting test health connection")
	}
	connection, err := panelHealthConnectionFromConn(clientConnection)
	if err != nil {
		t.Fatal(err)
	}
	expectedPath, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		t.Fatal(err)
	}
	property := func(name string) (string, error) {
		switch name {
		case "ActiveState":
			return "active", nil
		case "MainPID":
			return strconv.Itoa(pid), nil
		default:
			return "", errors.New("unexpected property")
		}
	}
	if err := verifyPanelServiceMainProcessAt(expectedPath, uint32(os.Geteuid()), connection, property); err != nil {
		t.Fatalf("actual active main process rejected: %v", err)
	}
	spoofedConnection := connection
	spoofedConnection.ServerPort++
	if err := verifyPanelServiceMainProcessAt(expectedPath, uint32(os.Geteuid()), spoofedConnection, property); err == nil {
		t.Fatal("health responder socket not owned by the MainPID was accepted")
	}
	spoofPath := filepath.Join(t.TempDir(), "yub-wpanel")
	if err := os.WriteFile(spoofPath, []byte("not-the-main-process"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyPanelServiceMainProcessAt(spoofPath, uint32(os.Geteuid()), connection, property); err == nil {
		t.Fatal("health responder not backed by the canonical MainPID executable was accepted")
	}
	inactiveProperty := func(name string) (string, error) {
		if name == "ActiveState" {
			return "inactive", nil
		}
		return strconv.Itoa(pid), nil
	}
	if err := verifyPanelServiceMainProcessAt(expectedPath, uint32(os.Geteuid()), connection, inactiveProperty); err == nil {
		t.Fatal("inactive systemd service was accepted")
	}
}

func TestParseProcNetTCPEndpointHandlesIPv4AndIPv6Loopback(t *testing.T) {
	for _, test := range []struct {
		raw      string
		wantIP   string
		wantPort int
	}{
		{raw: "0100007F:22B8", wantIP: "127.0.0.1", wantPort: 8888},
		{raw: "00000000000000000000000001000000:20FB", wantIP: "::1", wantPort: 8443},
		{raw: "0000000000000000FFFF00000100007F:22B8", wantIP: "127.0.0.1", wantPort: 8888},
	} {
		ip, port, err := parseProcNetTCPEndpoint(test.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", test.raw, err)
		}
		if !ip.Equal(net.ParseIP(test.wantIP)) || port != test.wantPort {
			t.Fatalf("parse %q = %s:%d, want %s:%d", test.raw, ip, port, test.wantIP, test.wantPort)
		}
	}
	for _, raw := range []string{"", "not-an-address", "0100007F:0000", "0100007F:10000"} {
		if _, _, err := parseProcNetTCPEndpoint(raw); err == nil {
			t.Fatalf("invalid proc endpoint %q accepted", raw)
		}
	}
}

func TestProcessSocketTableBindsServerSideConnectionDirection(t *testing.T) {
	tablePath := filepath.Join(t.TempDir(), "tcp")
	content := strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 0100007F:22B8 0100007F:C350 01 00000000:00000000 00:00000000 00000000 0 0 111 1",
		"   1: 0100007F:C350 0100007F:22B8 01 00000000:00000000 00:00000000 00000000 0 0 222 1",
	}, "\n") + "\n"
	if err := os.WriteFile(tablePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := panelHealthConnection{
		ClientIP:   "127.0.0.1",
		ClientPort: 50000,
		ServerIP:   "127.0.0.1",
		ServerPort: 8888,
	}
	ownedServerSocket := map[string]struct{}{"111": {}}
	matched, exists, err := processOwnsHealthConnectionInTable(tablePath, connection, ownedServerSocket)
	if err != nil || !exists || !matched {
		t.Fatalf("server-side connection rejected: matched=%v exists=%v err=%v", matched, exists, err)
	}
	reversed := panelHealthConnection{
		ClientIP:   connection.ServerIP,
		ClientPort: connection.ServerPort,
		ServerIP:   connection.ClientIP,
		ServerPort: connection.ClientPort,
	}
	matched, exists, err = processOwnsHealthConnectionInTable(tablePath, reversed, ownedServerSocket)
	if err != nil || !exists || matched {
		t.Fatalf("client-side/reversed connection accepted: matched=%v exists=%v err=%v", matched, exists, err)
	}
}

func TestProcessOwnsHealthConnectionSupportsIPv6Loopback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc TCP socket tables")
	}
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	client, err := net.Dial("tcp6", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var server net.Conn
	select {
	case server = <-accepted:
		defer server.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out accepting IPv6 health connection")
	}
	connection, err := panelHealthConnectionFromConn(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyProcessOwnsHealthConnection("/proc", os.Getpid(), connection); err != nil {
		t.Fatalf("IPv6 health connection rejected: %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a    string
		b    string
		want int
	}{
		{a: "v1.2.0", b: "v1.1.9", want: 1},
		{a: "v1.2.0", b: "v1.2.0", want: 0},
		{a: "v1.2.0-rc5", b: "v1.2.0-rc4", want: 1},
		{a: "v1.2.0", b: "v1.2.0-rc5", want: 1},
		{a: "v1.2.0-rc4", b: "v1.2.0", want: -1},
	}

	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		if (got > 0 && tt.want <= 0) || (got == 0 && tt.want != 0) || (got < 0 && tt.want >= 0) {
			t.Fatalf("CompareVersions(%q, %q) = %d, want sign %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestIsPatchBump(t *testing.T) {
	tests := []struct {
		current string
		target  string
		want    bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"1.2.3", "1.2.5", true},
		{"v1.2.3", "v1.3.0", false},
		{"v1.2.3", "v2.0.0", false},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.4-rc1", false},
	}
	for _, tt := range tests {
		if got := IsPatchBump(tt.current, tt.target); got != tt.want {
			t.Fatalf("IsPatchBump(%q, %q) = %v, want %v", tt.current, tt.target, got, tt.want)
		}
	}
}

func TestIsStableVersion(t *testing.T) {
	if !IsStableVersion("v1.2.3") {
		t.Fatal("stable version rejected")
	}
	if IsStableVersion("v1.2.3-rc1") {
		t.Fatal("prerelease accepted as stable")
	}
}

func TestCanonicalStableTag(t *testing.T) {
	for _, version := range []string{"1.2.3", "v01.2.3", "v1.2.3-rc1", "v1.2"} {
		if isCanonicalStableTag(version) {
			t.Fatalf("non-canonical version %q accepted", version)
		}
	}
	if !isCanonicalStableTag("v1.2.3") {
		t.Fatal("canonical stable tag rejected")
	}
}

func TestParsePanelInfoVersion(t *testing.T) {
	got, err := parsePanelInfoVersion([]byte("YUB WPanel 面板信息\n版本: v2.0.1 (构建: test)\n"))
	if err != nil {
		t.Fatalf("parse valid info: %v", err)
	}
	if got != "v2.0.1" {
		t.Fatalf("version = %q, want v2.0.1", got)
	}
	for _, output := range []string{
		"YUB WPanel 面板信息\n",
		"版本: 2.0.1\n",
		"版本: v2.0.1-rc1\n",
		"版本: v2.0.1\n版本: v2.0.1\n",
	} {
		if _, err := parsePanelInfoVersion([]byte(output)); err == nil {
			t.Fatalf("invalid info output %q accepted", output)
		}
	}
}

func TestVerifySHA256RequiresSingleMatchingFilename(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, panelBinaryName)
	checksum := filepath.Join(dir, panelBinaryName+".sha256")
	if err := os.WriteFile(binary, []byte("hello"), 0600); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	const digest = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if err := os.WriteFile(checksum, []byte(digest+"  "+panelBinaryName+"\n"), 0600); err != nil {
		t.Fatalf("write checksum: %v", err)
	}
	if err := verifySHA256(binary, checksum); err != nil {
		t.Fatalf("valid checksum rejected: %v", err)
	}
	for _, content := range []string{
		digest + "  different-name\n",
		digest + "\n",
		digest + "  " + panelBinaryName + "\n" + digest + "  " + panelBinaryName + "\n",
	} {
		if err := os.WriteFile(checksum, []byte(content), 0600); err != nil {
			t.Fatalf("rewrite checksum: %v", err)
		}
		if err := verifySHA256(binary, checksum); err == nil {
			t.Fatalf("invalid checksum manifest %q accepted", content)
		}
	}
}

func TestPreflightBinaryBindsConfigAndReleaseVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable script required")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("YUB_WPANEL_TEST_CONFIG", configPath)
	script := filepath.Join(dir, "candidate")
	content := "#!/bin/sh\n" +
		"[ \"$1\" = \"--info\" ] || exit 31\n" +
		"[ \"$2\" = \"--config\" ] || exit 32\n" +
		"[ \"$3\" = \"$YUB_WPANEL_TEST_CONFIG\" ] || exit 33\n" +
		"printf '版本: v2.0.1 (构建: test)\\n'\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if err := preflightBinaryWithTimeout(script, configPath, "v2.0.1", time.Second); err != nil {
		t.Fatalf("valid candidate rejected: %v", err)
	}
	if err := preflightBinaryWithTimeout(script, configPath, "v2.0.2", time.Second); err == nil {
		t.Fatal("replayed signed candidate with a mismatched version was accepted")
	}
}

func TestPreflightBinaryTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable script required")
	}
	script := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 2\n"), 0700); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if err := preflightBinaryWithTimeout(script, "/tmp/config.json", "v2.0.1", 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("timeout result = %v", err)
	}
}

func TestWithinAutoUpdateWindow(t *testing.T) {
	base := time.Date(2026, 6, 19, 3, 30, 0, 0, time.Local)
	if !withinAutoUpdateWindow("03:00-05:00", base) {
		t.Fatal("time inside same-day window was rejected")
	}
	if withinAutoUpdateWindow("04:00-05:00", base) {
		t.Fatal("time before same-day window was accepted")
	}
	late := time.Date(2026, 6, 19, 23, 30, 0, 0, time.Local)
	if !withinAutoUpdateWindow("23:00-02:00", late) {
		t.Fatal("time inside cross-day late window was rejected")
	}
	early := time.Date(2026, 6, 19, 1, 30, 0, 0, time.Local)
	if !withinAutoUpdateWindow("23:00-02:00", early) {
		t.Fatal("time inside cross-day early window was rejected")
	}
	for _, invalid := range []string{"", "25:00-26:00", "not-a-window"} {
		if withinAutoUpdateWindow(invalid, base) {
			t.Fatalf("invalid window %q was accepted", invalid)
		}
	}
}

func TestShouldFetchForAutoUpdate(t *testing.T) {
	now := time.Date(2026, 6, 19, 3, 30, 0, 0, time.UTC)
	base := autoUpdateSettings{
		LastCheckAt:      now.Add(-time.Hour),
		SignatureTimeout: 120 * time.Minute,
		ReleaseDelay:     15 * time.Minute,
	}
	if shouldFetchForAutoUpdate(base, now) {
		t.Fatal("normal check should respect 24 hour fetch interval")
	}
	base.LastCheckAt = now.Add(-25 * time.Hour)
	if !shouldFetchForAutoUpdate(base, now) {
		t.Fatal("normal check should run after 24 hour fetch interval")
	}
	waitingSig := autoUpdateSettings{
		LastCheckAt:              now.Add(-time.Hour),
		LastStatus:               "waiting",
		LastSignatureWaitVersion: "v1.2.4",
		LastSignatureWaitAt:      now.Add(-30 * time.Minute),
		SignatureTimeout:         120 * time.Minute,
	}
	if !shouldFetchForAutoUpdate(waitingSig, now) {
		t.Fatal("signature waiting should bypass 24 hour fetch interval")
	}
	releaseReady := autoUpdateSettings{
		LastCheckAt:       now.Add(-time.Hour),
		LastStatus:        "waiting",
		LastStage:         "waiting_release_delay",
		LastTargetVersion: "v1.2.4",
		LastAttemptAt:     now.Add(-20 * time.Minute),
		ReleaseDelay:      15 * time.Minute,
	}
	if !shouldFetchForAutoUpdate(releaseReady, now) {
		t.Fatal("release delay completion should bypass 24 hour fetch interval")
	}
}

func TestSanitizeBackupPart(t *testing.T) {
	got := sanitizeBackupPart("v1.2.3; rm -rf /_ok")
	want := "v1.2.3rm-rf_ok"
	if got != want {
		t.Fatalf("sanitizeBackupPart() = %q, want %q", got, want)
	}
}

func TestVersionedBackupPath(t *testing.T) {
	got := versionedBackupPath("v1.2.3; bad")
	if !strings.HasPrefix(got, panelInstallPath+".bak.v1.2.3bad.") {
		t.Fatalf("versionedBackupPath() = %q", got)
	}
	if strings.ContainsAny(strings.TrimPrefix(got, panelInstallPath+".bak."), " ;/\\") {
		t.Fatalf("versionedBackupPath contains unsafe characters: %q", got)
	}
}

func TestCopyFileCopiesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := copyPanelFile(src, dst, 0750); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("dst content = %q, want hello", string(data))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0750 {
		t.Fatalf("dst mode = %o, want 0750", info.Mode().Perm())
	}
}

func TestReplacePanelFileAtomicallyUsesSameDirectoryRandomTemporary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "candidate")
	dst := filepath.Join(dir, "yub-wpanel")
	if err := os.WriteFile(src, []byte("new-version"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replacePanelFileAtomically(src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(dst)
	if err != nil || string(content) != "new-version" {
		t.Fatalf("replaced panel content=%q err=%v", content, err)
	}
	info, err := os.Lstat(dst)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("replaced panel info=%v err=%v", info, err)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".yub-wpanel.replace-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("panel replacement temporary files=%v err=%v", temps, err)
	}
}

func TestReplacePanelFileAtomicallyReportsCommittedRenameWhenDirectorySyncFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "candidate")
	dst := filepath.Join(dir, "yub-wpanel")
	if err := os.WriteFile(src, []byte("new-version"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected directory fsync failure")
	err := replacePanelFileAtomicallyWithSync(src, dst, 0o755, func(path string) error {
		if path != dir {
			t.Fatalf("sync path=%q want=%q", path, dir)
		}
		return injected
	})
	if err == nil || !errors.Is(err, injected) || !panelFileReplacementCommitted(err) {
		t.Fatalf("replace error=%v committed=%v", err, panelFileReplacementCommitted(err))
	}
	content, readErr := os.ReadFile(dst)
	if readErr != nil || string(content) != "new-version" {
		t.Fatalf("rename was not committed before injected fsync failure: content=%q err=%v", content, readErr)
	}
}

func TestWriteRollbackPlanFileAtomicallyReplacesProtectedTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update_rollback.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := rollbackPlan{CurrentVersion: "v1.2.3", TargetVersion: "v1.2.4"}
	if err := writeRollbackPlanFile(path, plan); err != nil {
		t.Fatal(err)
	}
	written, err := readSecureRollbackPlanFile(path, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if written.PlanPath != path || written.CurrentVersion != plan.CurrentVersion || written.TargetVersion != plan.TargetVersion {
		t.Fatalf("rollback plan=%+v", written)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("rollback plan info=%v err=%v", info, err)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".update-rollback.tmp-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("rollback plan temporary files=%v err=%v", temps, err)
	}
}

func TestWriteRollbackPlanFileRejectsSymlinkWithoutTouchingVictim(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	path := filepath.Join(dir, "update_rollback.json")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeRollbackPlanFile(path, rollbackPlan{}); err == nil {
		t.Fatal("rollback plan symlink was accepted")
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "do-not-touch" {
		t.Fatalf("rollback plan symlink victim changed: content=%q err=%v", content, err)
	}
}

func TestWatchdogReadyFileIsAtomicRootOnlyAndNonceBound(t *testing.T) {
	dir := t.TempDir()
	plan := rollbackPlan{
		PlanPath:   filepath.Join(dir, "update_rollback.json"),
		ReadyPath:  filepath.Join(dir, "update_rollback.json.ready"),
		ReadyNonce: strings.Repeat("a", 64),
	}
	if err := writeWatchdogReadyFile(plan, uint32(os.Geteuid())); err != nil {
		t.Fatalf("write ready file: %v", err)
	}
	if err := readWatchdogReadyFile(plan, uint32(os.Geteuid())); err != nil {
		t.Fatalf("read ready file: %v", err)
	}
	info, err := os.Lstat(plan.ReadyPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("ready file info=%v err=%v", info, err)
	}
	wrongNonce := plan
	wrongNonce.ReadyNonce = strings.Repeat("b", 64)
	if err := readWatchdogReadyFile(wrongNonce, uint32(os.Geteuid())); err == nil {
		t.Fatal("ready file with a nonce from another plan was accepted")
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".update-watchdog-ready.tmp-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("ready temporary files=%v err=%v", temps, err)
	}
}

func TestWatchdogReadyFileRejectsSymlinkWithoutTouchingVictim(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	plan := rollbackPlan{
		PlanPath:   filepath.Join(dir, "update_rollback.json"),
		ReadyPath:  filepath.Join(dir, "update_rollback.json.ready"),
		ReadyNonce: strings.Repeat("a", 64),
	}
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, plan.ReadyPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeWatchdogReadyFile(plan, uint32(os.Geteuid())); err == nil {
		t.Fatal("symlink watchdog ready target was accepted")
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "do-not-touch" {
		t.Fatalf("ready symlink victim changed: content=%q err=%v", content, err)
	}
}

func TestExecutePanelRollbackStopsBeforeDatabaseAndBinaryMutation(t *testing.T) {
	var calls []string
	ops := panelRollbackOps{
		stopAndWait: func(time.Duration) error {
			calls = append(calls, "stop")
			return nil
		},
		prepare: func() (rollbackPlan, bool, error) {
			calls = append(calls, "prepare")
			return rollbackPlan{
				BackupDB: "backup.db", BackupBinary: "backup-bin", HealthURL: "health", CurrentVersion: "v1.2.3",
			}, true, nil
		},
		restoreDB: func(*config.Config, string) error {
			calls = append(calls, "database")
			return nil
		},
		restoreBinary: func(string) error {
			calls = append(calls, "binary")
			return nil
		},
		start: func() error {
			calls = append(calls, "start")
			return nil
		},
		waitVersion: func(string, string, time.Duration) error {
			calls = append(calls, "health")
			return nil
		},
	}
	stage, err := executePanelRollback(&config.Config{}, ops)
	if err != nil || stage != "rollback_health" {
		t.Fatalf("rollback stage=%q err=%v", stage, err)
	}
	if got, want := strings.Join(calls, ","), "stop,prepare,database,binary,start,health"; got != want {
		t.Fatalf("rollback calls=%q want=%q", got, want)
	}
}

func TestExecutePanelRollbackStopFailureDoesNotMutateAndRetainsPlan(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "update_rollback.json")
	if err := os.WriteFile(planPath, []byte("retain"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutated := false
	ops := panelRollbackOps{
		stopAndWait: func(time.Duration) error { return errors.New("cannot stop") },
		prepare: func() (rollbackPlan, bool, error) {
			mutated = true
			return rollbackPlan{}, true, nil
		},
		restoreDB: func(*config.Config, string) error {
			mutated = true
			return nil
		},
		restoreBinary: func(string) error {
			mutated = true
			return nil
		},
		start:       func() error { mutated = true; return nil },
		waitVersion: func(string, string, time.Duration) error { mutated = true; return nil },
	}
	stage, err := executePanelRollback(&config.Config{}, ops)
	if err == nil || stage != "rollback_stop" {
		t.Fatalf("rollback stage=%q err=%v", stage, err)
	}
	if mutated {
		t.Fatal("database, binary, or service start was touched after stop failure")
	}
	if content, readErr := os.ReadFile(planPath); readErr != nil || string(content) != "retain" {
		t.Fatalf("rollback plan not retained: content=%q err=%v", content, readErr)
	}
}

func TestExecutePanelRollbackFailureRestopsAndDoesNotContinue(t *testing.T) {
	for _, test := range []struct {
		name      string
		failCall  string
		wantStage string
		wantCalls string
	}{
		{name: "prepare", failCall: "prepare", wantStage: "rollback_validate", wantCalls: "stop,prepare,stop"},
		{name: "database", failCall: "database", wantStage: "rollback_database", wantCalls: "stop,prepare,database,stop"},
		{name: "binary", failCall: "binary", wantStage: "rollback_binary", wantCalls: "stop,prepare,database,binary,stop"},
		{name: "start", failCall: "start", wantStage: "rollback_start", wantCalls: "stop,prepare,database,binary,start,stop"},
		{name: "health", failCall: "health", wantStage: "rollback_health", wantCalls: "stop,prepare,database,binary,start,health,stop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			call := func(name string) error {
				calls = append(calls, name)
				if name == test.failCall {
					return errors.New("injected failure")
				}
				return nil
			}
			ops := panelRollbackOps{
				stopAndWait: func(time.Duration) error { return call("stop") },
				prepare: func() (rollbackPlan, bool, error) {
					if err := call("prepare"); err != nil {
						return rollbackPlan{}, false, err
					}
					return rollbackPlan{}, true, nil
				},
				restoreDB:     func(*config.Config, string) error { return call("database") },
				restoreBinary: func(string) error { return call("binary") },
				start:         func() error { return call("start") },
				waitVersion:   func(string, string, time.Duration) error { return call("health") },
			}
			stage, err := executePanelRollback(&config.Config{}, ops)
			if err == nil || stage != test.wantStage {
				t.Fatalf("rollback stage=%q err=%v", stage, err)
			}
			if got := strings.Join(calls, ","); got != test.wantCalls {
				t.Fatalf("rollback calls=%q want=%q", got, test.wantCalls)
			}
		})
	}
}

func TestWatchdogStartFailurePreservesPlanWhenBinaryRecoveryFails(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "update_rollback.json")
	if err := os.WriteFile(planPath, []byte("retain-for-operator"), 0o600); err != nil {
		t.Fatal(err)
	}
	preserve, err := recoverBinaryAfterWatchdogStartFailure("backup-bin", errors.New("handshake failed"), func(string) error {
		return errors.New("binary restore failed")
	})
	if err == nil || !preserve {
		t.Fatalf("recovery preserve=%v err=%v", preserve, err)
	}
	cleanupUntransferredRollbackPlan(planPath, false, preserve)
	if content, readErr := os.ReadFile(planPath); readErr != nil || string(content) != "retain-for-operator" {
		t.Fatalf("rollback plan was not retained: content=%q err=%v", content, readErr)
	}
}

func TestWatchdogStartFailureRemovesPlanAfterSuccessfulBinaryRecovery(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "update_rollback.json")
	if err := os.WriteFile(planPath, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	preserve, err := recoverBinaryAfterWatchdogStartFailure("backup-bin", errors.New("handshake failed"), func(path string) error {
		if path != "backup-bin" {
			t.Fatalf("recovery path=%q", path)
		}
		return nil
	})
	if err == nil || preserve {
		t.Fatalf("recovery preserve=%v err=%v", preserve, err)
	}
	cleanupUntransferredRollbackPlan(planPath, false, preserve)
	if _, statErr := os.Lstat(planPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("completed failed-start plan was not removed: %v", statErr)
	}
}

func TestCommittedReplaceFailurePreservesPlanWhenOldBinaryRecoveryFails(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "update_rollback.json")
	if err := os.WriteFile(planPath, []byte("retain-for-operator"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaceErr := &panelFileReplaceError{err: errors.New("directory fsync failed"), committed: true}
	preserve, err := recoverBinaryAfterCommittedReplaceFailure("backup-bin", replaceErr, func(path string) error {
		if path != "backup-bin" {
			t.Fatalf("recovery path=%q", path)
		}
		return errors.New("old binary recovery failed")
	})
	if err == nil || !preserve {
		t.Fatalf("recovery preserve=%v err=%v", preserve, err)
	}
	cleanupUntransferredRollbackPlan(planPath, false, preserve)
	if content, readErr := os.ReadFile(planPath); readErr != nil || string(content) != "retain-for-operator" {
		t.Fatalf("rollback plan was not retained: content=%q err=%v", content, readErr)
	}
}

func TestCommittedReplaceFailureRemovesPlanAfterOldBinaryRecoverySucceeds(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "update_rollback.json")
	if err := os.WriteFile(planPath, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaceErr := &panelFileReplaceError{err: errors.New("directory fsync failed"), committed: true}
	preserve, err := recoverBinaryAfterCommittedReplaceFailure("backup-bin", replaceErr, func(string) error {
		return nil
	})
	if err == nil || preserve {
		t.Fatalf("recovery preserve=%v err=%v", preserve, err)
	}
	cleanupUntransferredRollbackPlan(planPath, false, preserve)
	if _, statErr := os.Lstat(planPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("recovered replacement plan was not removed: %v", statErr)
	}
}

func TestRestoreDBFileAtomicallyReplacesDatabaseAndRemovesSidecars(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "backup.db")
	targetPath := filepath.Join(dir, "panel.db")
	createRollbackTestDatabase(t, backupPath, "backup-value")
	createRollbackTestDatabase(t, targetPath, "target-value")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.WriteFile(targetPath+suffix, []byte("stale-sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{SQLite: config.SQLiteConfig{Path: targetPath}}
	if err := restoreDBFile(cfg, backupPath); err != nil {
		t.Fatalf("restore database: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(targetPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale sidecar %s remains: %v", suffix, err)
		}
	}
	info, err := os.Lstat(targetPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("restored database info=%v err=%v", info, err)
	}
	if err := database.Open(targetPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var marker string
	if err := database.GetDB().QueryRow("SELECT marker FROM rollback_test LIMIT 1").Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "backup-value" {
		t.Fatalf("restored marker=%q", marker)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{".panel.db.rollback-*", ".panel.db-wal.quarantine-*", ".panel.db-shm.quarantine-*", ".panel.db-journal.quarantine-*"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil || len(matches) != 0 {
			t.Fatalf("rollback artifacts for %s=%v err=%v", pattern, matches, err)
		}
	}
}

func TestRestoreDBFileRejectsSymlinkTargetWithoutTouchingVictim(t *testing.T) {
	dir := t.TempDir()
	backupPath := filepath.Join(dir, "backup.db")
	createRollbackTestDatabase(t, backupPath, "backup-value")
	victim := filepath.Join(dir, "victim.db")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(dir, "panel.db")
	if err := os.Symlink(victim, targetPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := restoreDBFile(&config.Config{SQLite: config.SQLiteConfig{Path: targetPath}}, backupPath); err == nil {
		t.Fatal("symlink database target was accepted")
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "do-not-touch" {
		t.Fatalf("database symlink victim changed: content=%q err=%v", content, err)
	}
}

func createRollbackTestDatabase(t *testing.T, path, marker string) {
	t.Helper()
	if err := database.Open(path); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetDB().Exec("CREATE TABLE rollback_test (marker TEXT NOT NULL)"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.GetDB().Exec("INSERT INTO rollback_test(marker) VALUES (?)", marker); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotPanelUpdateStatusExpiresTerminalState(t *testing.T) {
	restore := preservePanelUpdateStatus(t)
	panelUpdateStatusMu.Lock()
	currentPanelUpdateStatus = PanelUpdateStatus{
		Completed: true,
		Stage:     "completed",
		Message:   "更新完成",
		Percent:   100,
		UpdatedAt: time.Now().Add(-updateTerminalStatusTTL - time.Second),
	}
	panelUpdateStatusMu.Unlock()
	got := SnapshotPanelUpdateStatus()
	if got.Stage != "idle" || got.Completed || got.Running || got.Percent != 0 {
		t.Fatalf("expired status = %+v, want idle", got)
	}
	restore()
}

func preservePanelUpdateStatus(t *testing.T) func() {
	t.Helper()
	prev := SnapshotPanelUpdateStatus()
	restored := false
	restore := func() {
		if restored {
			return
		}
		panelUpdateStatusMu.Lock()
		currentPanelUpdateStatus = prev
		panelUpdateStatusMu.Unlock()
		restored = true
	}
	t.Cleanup(restore)
	return restore
}
