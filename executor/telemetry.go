package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zangwp/yub-wpanel/database"
)

const defaultTelemetryURL = ""

type heartbeatPayload struct {
	AnonymousID string `json:"anonymous_id"`
	Version     string `json:"version"`
}

var telemetryVersion string
var telemetrySettingsChanged = make(chan struct{}, 1)

// StartTelemetry 启动可选运行统计调度：首次明确启用后上报一次，此后每天 UTC 00:00 附近上报。
// 上报内容仅含稳定伪匿名 ID（machine-id 的 SHA256 前 16 字节）和面板版本号。
func StartTelemetry(version string) {
	telemetryVersion = version

	if !isTelemetryEnabled() || getTelemetryURL() == "" {
		log.Println("[遥测] 未启用自定义统计服务，后台调度保持待命")
	}

	go func() {
		// 已启用且从未成功上报时立即尝试，更新/重启则跳过。
		if isFirstHeartbeat() {
			sendHeartbeat()
		}

		// 计算距下一个 UTC 00:00 的间隔，加 ±5 分钟随机抖动
		now := time.Now().UTC()
		midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		jitter := time.Duration(rand.Intn(600)-300) * time.Second
		waitDur := midnight.Sub(now) + jitter
		if waitDur < 0 {
			waitDur = 0
		}

		timer := time.NewTimer(waitDur)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				sendHeartbeat()
				timer.Reset(24 * time.Hour)
			case <-telemetrySettingsChanged:
				// The first explicit enable should take effect without a restart.
				// Later settings saves do not create extra daily heartbeats.
				if isFirstHeartbeat() {
					sendHeartbeat()
				}
			}
		}
	}()
}

// NotifyTelemetrySettingsChanged wakes the telemetry scheduler after the
// administrator changes the opt-in or endpoint. The signal is coalesced.
func NotifyTelemetrySettingsChanged() {
	select {
	case telemetrySettingsChanged <- struct{}{}:
	default:
	}
}

// isFirstHeartbeat 检查是否从未成功上报过心跳（新装面板）。
func isFirstHeartbeat() bool {
	db := database.GetDB()
	if db == nil {
		return true
	}
	var val string
	err := db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'telemetry_first_sent'").Scan(&val)
	return err != nil || val == ""
}

func sendHeartbeat() {
	if !isTelemetryEnabled() {
		return
	}
	url := getTelemetryURL()
	if url == "" {
		return
	}
	heartbeatURL, err := telemetryHeartbeatURL(url)
	if err != nil {
		log.Printf("[遥测] 统计服务地址无效: %v", err)
		return
	}

	anonID := generateAnonymousID()
	if anonID == "" {
		return
	}

	payload := heartbeatPayload{AnonymousID: anonID, Version: telemetryVersion}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	client := newTelemetryHTTPClient()
	resp, err := client.Post(heartbeatURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[遥测] 上报失败: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[遥测] 上报返回非预期状态: %d", resp.StatusCode)
		return
	}

	// 标记首次心跳已发送，后续成功上报不得覆盖首次成功时间。
	if err := recordFirstHeartbeatSent(time.Now()); err != nil {
		log.Printf("[遥测] 已上报但无法记录首次成功时间: %v", err)
	}

	log.Println("[遥测] 伪匿名心跳上报成功")
}

func recordFirstHeartbeatSent(sentAt time.Time) error {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO security_settings (skey, svalue, description, updated_at)
		VALUES ('telemetry_first_sent', ?, '首次心跳上报时间', CURRENT_TIMESTAMP)
		ON CONFLICT(skey) DO UPDATE SET
			svalue=excluded.svalue,
			description=excluded.description,
			updated_at=CURRENT_TIMESTAMP
		WHERE security_settings.svalue = ''
	`, sentAt.UTC().Format(time.RFC3339))
	return err
}

func generateAnonymousID() string {
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		data, err = os.ReadFile("/var/lib/dbus/machine-id")
		if err != nil {
			return ""
		}
	}
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash[:16])
}

func getTelemetryURL() string {
	db := database.GetDB()
	if db == nil {
		return defaultTelemetryURL
	}
	var url string
	db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'telemetry_url'").Scan(&url)
	if url == "" {
		return defaultTelemetryURL
	}
	return url
}

func isTelemetryEnabled() bool {
	db := database.GetDB()
	if db == nil {
		return false
	}
	var val string
	db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'telemetry_enabled'").Scan(&val)
	return val == "true"
}

func newTelemetryHTTPClient() *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialPublicTelemetryAddress(ctx, network, address)
		},
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func dialPublicTelemetryAddress(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid telemetry address: %w", err)
	}
	addresses, err := resolvePublicTelemetryAddresses(ctx, host)
	if err != nil {
		return nil, err
	}

	var dialer net.Dialer
	var lastErr error
	for _, addr := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("connect to telemetry host: %w", lastErr)
}

func resolvePublicTelemetryAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return nil, fmt.Errorf("invalid telemetry host")
	}

	var addresses []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = append(addresses, literal.Unmap())
	} else {
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve telemetry host: %w", err)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("resolve telemetry host: no addresses")
		}
		for _, addr := range resolved {
			addresses = append(addresses, addr.Unmap())
		}
	}

	// Validate the complete DNS answer before attempting any connection. This
	// prevents a mixed public/private answer from being accepted based on order.
	for _, addr := range addresses {
		if !isPublicTelemetryIP(addr) {
			return nil, fmt.Errorf("telemetry host resolved to a non-public address")
		}
	}
	return addresses, nil
}

func isPublicTelemetryIP(addr netip.Addr) bool {
	if addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("3fff::/20"),
		netip.MustParsePrefix("5f00::/16"),
		netip.MustParsePrefix("fec0::/10"),
	} {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// NormalizeTelemetryURL validates and canonicalizes an administrator-provided
// telemetry endpoint using the same address policy enforced by the dialer.
func NormalizeTelemetryURL(raw string) (string, error) {
	u, err := parseTelemetryURL(raw)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := resolvePublicTelemetryAddresses(ctx, u.Hostname()); err != nil {
		return "", err
	}
	u.Scheme = "https"
	return strings.TrimRight(u.String(), "/"), nil
}

func parseTelemetryURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil ||
		u.Fragment != "" || u.RawFragment != "" || u.RawQuery != "" || u.ForceQuery {
		return nil, fmt.Errorf("invalid telemetry endpoint")
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return nil, fmt.Errorf("invalid telemetry endpoint")
	}
	if port := u.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("invalid telemetry endpoint port")
		}
	}
	return u, nil
}

func telemetryHeartbeatURL(raw string) (string, error) {
	u, err := parseTelemetryURL(raw)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/heartbeat"
	return u.String(), nil
}
