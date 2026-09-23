package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/config"
	"github.com/zangwp/yub-wpanel/database"
	"github.com/zangwp/yub-wpanel/router"
)

type fakeCoreUpdateWorkerLifecycle struct {
	startErr error
	stop     func(context.Context) error
}

func TestSiteMigrationMachineRoutePassesRealScanDefenseStack(t *testing.T) {
	if err := database.Open(filepath.Join(t.TempDir(), "panel.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.RunMigrations(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Panel.RandomSuffix = "test-panel-prefix"
	cfg.Panel.TLSCertPath = filepath.Join(t.TempDir(), "unused.crt")
	engine := router.SetupRouter(cfg, TemplatesFS, StaticFS, "test-version", "")

	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/site-migration/v1/pair/redeem", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Go-http-client/1.1")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want downstream pairing rejection %d", response.StatusCode, http.StatusUnauthorized)
	}
	var bans int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM firewall_bans WHERE source_jail='panel_scan'`).Scan(&bans); err != nil {
		t.Fatal(err)
	}
	if bans != 0 {
		t.Fatalf("panel scan bans=%d, want 0", bans)
	}
}

func (f *fakeCoreUpdateWorkerLifecycle) Start() error { return f.startErr }
func (f *fakeCoreUpdateWorkerLifecycle) Stop(ctx context.Context) error {
	if f.stop != nil {
		return f.stop(ctx)
	}
	return nil
}

func TestStartWPCoreUpdateWorkerFailsClosedWithoutHalfStartedWorker(t *testing.T) {
	cfg := &config.Config{}
	wantErr := errors.New("injected")
	if worker, err := startWPCoreUpdateWorker(cfg, func(*config.Config) (wpCoreUpdateWorkerLifecycle, error) {
		return nil, wantErr
	}); !errors.Is(err, wantErr) || worker != nil {
		t.Fatalf("constructor failure worker=%v err=%v", worker, err)
	}
	if worker, err := startWPCoreUpdateWorker(cfg, func(*config.Config) (wpCoreUpdateWorkerLifecycle, error) {
		return &fakeCoreUpdateWorkerLifecycle{startErr: wantErr}, nil
	}); !errors.Is(err, wantErr) || worker != nil {
		t.Fatalf("start failure worker=%v err=%v", worker, err)
	}
	want := &fakeCoreUpdateWorkerLifecycle{}
	worker, err := startWPCoreUpdateWorker(cfg, func(*config.Config) (wpCoreUpdateWorkerLifecycle, error) { return want, nil })
	if err != nil || worker != want {
		t.Fatalf("worker=%v err=%v", worker, err)
	}
}

func TestStopWPCoreUpdateWorkerUsesBoundedContext(t *testing.T) {
	timeout := 20 * time.Millisecond
	worker := &fakeCoreUpdateWorkerLifecycle{stop: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	started := time.Now()
	err := stopWPCoreUpdateWorker(worker, timeout)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if elapsed < timeout || elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown elapsed=%s", elapsed)
	}
}

func TestPanelHTTPServerHasBoundedHeadersAndIdleConnections(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := newPanelHTTPServer(":9443", handler)
	if server.Addr != ":9443" || server.Handler == nil {
		t.Fatalf("server address/handler not configured: %#v", server)
	}
	if server.ReadHeaderTimeout != panelReadHeaderTimeout || server.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout=%s", server.ReadHeaderTimeout)
	}
	if server.IdleTimeout != panelIdleTimeout || server.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout=%s", server.IdleTimeout)
	}
	if server.MaxHeaderBytes != panelMaxHeaderBytes || server.MaxHeaderBytes <= 0 {
		t.Fatalf("MaxHeaderBytes=%d", server.MaxHeaderBytes)
	}
}

func TestPrivilegedCLIModesPassRuntimeIdentityGate(t *testing.T) {
	sourceBytes, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	repair := strings.Index(source, "if *repairConfigCheck {")
	info := strings.Index(source, "if *showInfo {")
	watchdogMode := strings.Index(source, `if *updateWatchdog != "" {`)
	watchdogGate := strings.Index(source, "executor.ValidateUpdateWatchdogDistributionIdentity")
	gate := strings.Index(source, "executor.ValidateRuntimeDistributionIdentity")
	systemUpdate := strings.Index(source, `if *systemPackageUpdatePlan != "" {`)
	databaseRestore := strings.Index(source, `if *panelDBRestorePlan != "" {`)
	fail2ban := strings.Index(source, `if *banIPNginx != "" || *unbanIPNginx != ""`)
	watchdogDatabaseOpen := strings.Index(source[watchdogGate:], "database.Open(cfg.SQLite.Path)")
	if watchdogDatabaseOpen >= 0 {
		watchdogDatabaseOpen += watchdogGate
	}
	watchdogReady := strings.Index(source[watchdogDatabaseOpen:], "executor.SignalUpdateWatchdogReady")
	if watchdogReady >= 0 {
		watchdogReady += watchdogDatabaseOpen
	}
	watchdogRun := strings.Index(source[watchdogReady:], "executor.RunUpdateWatchdog")
	if watchdogRun >= 0 {
		watchdogRun += watchdogReady
	}
	databaseOpen := strings.Index(source[gate:], "database.Open(cfg.SQLite.Path)")
	if databaseOpen >= 0 {
		databaseOpen += gate
	}
	for name, offset := range map[string]int{
		"repair": repair, "info": info, "watchdog mode": watchdogMode, "watchdog gate": watchdogGate,
		"watchdog database open": watchdogDatabaseOpen, "watchdog ready": watchdogReady, "watchdog run": watchdogRun,
		"gate": gate, "system update": systemUpdate,
		"database restore": databaseRestore, "fail2ban": fail2ban, "database open": databaseOpen,
	} {
		if offset < 0 {
			t.Fatalf("main.go is missing %s path", name)
		}
	}
	if !(repair < info && info < watchdogMode && watchdogMode <= watchdogGate && watchdogGate < watchdogDatabaseOpen && watchdogDatabaseOpen < watchdogReady && watchdogReady < watchdogRun && watchdogRun < gate) {
		t.Fatalf("read-only/watchdog gate order invalid: repair=%d info=%d watchdog_mode=%d watchdog_gate=%d watchdog_db=%d watchdog_ready=%d watchdog_run=%d gate=%d", repair, info, watchdogMode, watchdogGate, watchdogDatabaseOpen, watchdogReady, watchdogRun, gate)
	}
	if !strings.Contains(source[watchdogMode:watchdogGate], "flag.NFlag() != 2") {
		t.Fatal("update watchdog mode does not reject mixed CLI actions")
	}
	for name, offset := range map[string]int{
		"system update":    systemUpdate,
		"database restore": databaseRestore,
		"fail2ban":         fail2ban,
		"database open":    databaseOpen,
	} {
		if gate >= offset {
			t.Fatalf("runtime gate offset=%d must precede %s offset=%d", gate, name, offset)
		}
	}
}

func TestPasswordResetDoesNotAcceptPlaintextInProcessArguments(t *testing.T) {
	sourceBytes, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, forbidden := range []string{`flag.String("passwd"`, `--passwd`, `resetAdminPassword`} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("main.go still exposes plaintext password CLI path %q", forbidden)
		}
	}
	if !strings.Contains(source, `flag.Bool("reset-admin"`) {
		t.Fatal("safe random administrator reset mode is missing")
	}
}
