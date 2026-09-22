package executor

import (
	"context"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/database"
)

func TestTelemetryClientRejectsRedirects(t *testing.T) {
	client := newTelemetryHTTPClient()
	req, err := http.NewRequest(http.MethodGet, "https://example.com/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect error = %v, want http.ErrUseLastResponse", err)
	}
}

func TestTelemetrySettingsWakeupIsCoalesced(t *testing.T) {
	for len(telemetrySettingsChanged) > 0 {
		<-telemetrySettingsChanged
	}
	NotifyTelemetrySettingsChanged()
	NotifyTelemetrySettingsChanged()
	if got := len(telemetrySettingsChanged); got != 1 {
		t.Fatalf("queued telemetry wakeups = %d, want 1", got)
	}
	<-telemetrySettingsChanged
}

func TestPublicTelemetryIPPolicy(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{ip: "8.8.8.8", want: true},
		{ip: "2606:4700:4700::1111", want: true},
		{ip: "127.0.0.1", want: false},
		{ip: "10.0.0.1", want: false},
		{ip: "100.64.0.1", want: false},
		{ip: "169.254.169.254", want: false},
		{ip: "192.0.2.1", want: false},
		{ip: "192.88.99.1", want: false},
		{ip: "198.18.0.1", want: false},
		{ip: "203.0.113.1", want: false},
		{ip: "::1", want: false},
		{ip: "64:ff9b::a00:1", want: false},
		{ip: "100::1", want: false},
		{ip: "2001::1", want: false},
		{ip: "2001:db8::1", want: false},
		{ip: "2002:a00:1::1", want: false},
		{ip: "3fff::1", want: false},
		{ip: "fec0::1", want: false},
		{ip: "2606:4700:4700::1111%eth0", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.ip, func(t *testing.T) {
			if got := isPublicTelemetryIP(netip.MustParseAddr(tc.ip)); got != tc.want {
				t.Fatalf("isPublicTelemetryIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestTelemetryDialRejectsReservedAddress(t *testing.T) {
	for _, address := range []string{
		"192.0.2.1:443",
		"198.18.0.1:443",
		"[2001:db8::1]:443",
	} {
		if conn, err := dialPublicTelemetryAddress(context.Background(), "tcp", address); err == nil {
			conn.Close()
			t.Errorf("reserved dial address %q accepted", address)
		}
	}
}

func TestTelemetryHeartbeatURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://telemetry.example.com":       "https://telemetry.example.com/api/heartbeat",
		"https://telemetry.example.com/base/": "https://telemetry.example.com/base/api/heartbeat",
	} {
		got, err := telemetryHeartbeatURL(raw)
		if err != nil {
			t.Errorf("valid endpoint %q rejected: %v", raw, err)
		} else if got != want {
			t.Errorf("telemetryHeartbeatURL(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, invalid := range []string{
		"http://telemetry.example.com",
		"https://user:pass@telemetry.example.com",
		"https://telemetry.example.com/#fragment",
		"https://telemetry.example.com/?query=value",
		"https://telemetry.example.com?",
		"https://telemetry.example.com:70000",
		"/relative",
	} {
		if _, err := telemetryHeartbeatURL(invalid); err == nil {
			t.Errorf("invalid endpoint %q accepted", invalid)
		}
	}
}

func TestNormalizeTelemetryURLUsesDialAddressPolicy(t *testing.T) {
	got, err := NormalizeTelemetryURL(" HTTPS://8.8.8.8/telemetry/ ")
	if err != nil {
		t.Fatalf("public endpoint rejected: %v", err)
	}
	if got != "https://8.8.8.8/telemetry" {
		t.Fatalf("normalized endpoint = %q", got)
	}

	for _, endpoint := range []string{
		"http://8.8.8.8/telemetry",
		"https://127.0.0.1/telemetry",
		"https://10.0.0.1/telemetry",
		"https://100.64.0.1/telemetry",
		"https://169.254.169.254/telemetry",
		"https://192.0.2.1/telemetry",
		"https://198.18.0.1/telemetry",
		"https://198.51.100.1/telemetry",
		"https://203.0.113.1/telemetry",
		"https://[::1]/telemetry",
		"https://[2001:db8::1]/telemetry",
	} {
		if _, err := NormalizeTelemetryURL(endpoint); err == nil {
			t.Errorf("non-public endpoint %q accepted", endpoint)
		}
	}
}

func TestRecordFirstHeartbeatSentDoesNotOverwriteFirstSuccess(t *testing.T) {
	setupTelemetryTestDB(t)

	first := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := recordFirstHeartbeatSent(first); err != nil {
		t.Fatalf("record first heartbeat: %v", err)
	}
	const preservedUpdatedAt = "2000-01-01 00:00:00"
	if _, err := database.GetDB().Exec(`UPDATE security_settings SET updated_at=? WHERE skey='telemetry_first_sent'`, preservedUpdatedAt); err != nil {
		t.Fatalf("set update marker: %v", err)
	}

	second := first.Add(24 * time.Hour)
	if err := recordFirstHeartbeatSent(second); err != nil {
		t.Fatalf("record second heartbeat: %v", err)
	}

	var value, updatedAt string
	if err := database.GetDB().QueryRow(`SELECT svalue, updated_at FROM security_settings WHERE skey='telemetry_first_sent'`).Scan(&value, &updatedAt); err != nil {
		t.Fatalf("read first heartbeat marker: %v", err)
	}
	if want := first.Format(time.RFC3339); value != want {
		t.Fatalf("telemetry_first_sent = %q, want first success %q", value, want)
	}
	if updatedAt != preservedUpdatedAt {
		t.Fatalf("updated_at = %q, want preserved %q", updatedAt, preservedUpdatedAt)
	}
}

func setupTelemetryTestDB(t *testing.T) {
	t.Helper()
	oldDB := database.DB
	if err := database.Open(filepath.Join(t.TempDir(), "panel.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := database.GetDB().Exec(`CREATE TABLE security_settings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		skey TEXT NOT NULL UNIQUE,
		svalue TEXT NOT NULL DEFAULT '',
		description TEXT DEFAULT '',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create security settings: %v", err)
	}
	t.Cleanup(func() {
		database.Close()
		database.DB = oldDB
	})
}
