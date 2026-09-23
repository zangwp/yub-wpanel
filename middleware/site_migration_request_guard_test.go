package middleware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type migrationGuardAuthorizer struct {
	err          error
	calls        int
	bearer       string
	allowPending bool
}

func (a *migrationGuardAuthorizer) AuthorizeMachineBearer(_ context.Context, bearer string, allowPending bool) error {
	a.calls++
	a.bearer = bearer
	a.allowPending = allowPending
	return a.err
}

type migrationTrackingBody struct {
	reader *strings.Reader
	reads  int
}

func (b *migrationTrackingBody) Read(p []byte) (int, error) {
	b.reads++
	return b.reader.Read(p)
}

func (*migrationTrackingBody) Close() error { return nil }

func TestSiteMigrationRequestGuardRejectsAuthorizationBeforeReadingBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	validHeader := "Bearer " + migrationGuardToken(1)
	tests := []struct {
		name       string
		header     string
		authErr    error
		wantCalls  int
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic abc", wantStatus: http.StatusUnauthorized},
		{name: "empty bearer", header: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "wrong credential", header: validHeader, authErr: errors.New("rejected"), wantCalls: 1, wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authorizer := &migrationGuardAuthorizer{err: tt.authErr}
			body := &migrationTrackingBody{reader: strings.NewReader(`{"peer_id":"peer_00000000001"}`)}
			var deadlines []time.Time
			slots := make(chan struct{}, 1)
			router := gin.New()
			router.Use(newSiteMigrationRequestGuard(authorizer, func() time.Time { return now }, func(_ http.ResponseWriter, deadline time.Time) error {
				deadlines = append(deadlines, deadline)
				return nil
			}, slots, make(chan struct{}, 1)))
			reached := false
			router.POST(siteMigrationChallengePath, func(c *gin.Context) {
				reached = true
				_, _ = io.ReadAll(c.Request.Body)
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, siteMigrationChallengePath, nil)
			req.Body = body
			req.ContentLength = int64(body.reader.Len())
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus || reached || body.reads != 0 || authorizer.calls != tt.wantCalls {
				t.Fatalf("status=%d reached=%t reads=%d auth_calls=%d", recorder.Code, reached, body.reads, authorizer.calls)
			}
			if len(deadlines) != 1 || !deadlines[0].Equal(now) {
				t.Fatalf("deadlines=%v, want immediate read expiry", deadlines)
			}
			if recorder.Header().Get("Connection") != "close" || !req.Close || len(slots) != 0 {
				t.Fatalf("connection=%q request_close=%t slots=%d", recorder.Header().Get("Connection"), req.Close, len(slots))
			}
		})
	}
}

func TestSiteMigrationRequestGuardBoundsAndReleasesAuthenticatedBodyRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	authorizer := &migrationGuardAuthorizer{}
	body := &migrationTrackingBody{reader: strings.NewReader(`{"peer_id":"peer_00000000001"}`)}
	var deadlines []time.Time
	slots := make(chan struct{}, 1)
	router := gin.New()
	router.Use(newSiteMigrationRequestGuard(authorizer, func() time.Time { return now }, func(_ http.ResponseWriter, deadline time.Time) error {
		deadlines = append(deadlines, deadline)
		return nil
	}, slots, make(chan struct{}, 1)))
	router.POST(siteMigrationChallengePath, func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			t.Errorf("ReadAll(): %v", err)
		}
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, siteMigrationChallengePath, nil)
	req.Body = body
	req.ContentLength = int64(body.reader.Len())
	req.Header.Set("Authorization", "Bearer "+migrationGuardToken(2))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent || body.reads == 0 || authorizer.calls != 1 || !authorizer.allowPending {
		t.Fatalf("status=%d reads=%d auth=%+v", recorder.Code, body.reads, authorizer)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(now.Add(siteMigrationBodyReadTimeout)) || !deadlines[1].IsZero() {
		t.Fatalf("deadlines=%v", deadlines)
	}
	if len(slots) != 0 {
		t.Fatalf("reader slot leaked: %d", len(slots))
	}
}

func TestSiteMigrationRequestGuardWorksThroughGinResponseWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authorizer := &migrationGuardAuthorizer{}
	router := gin.New()
	router.Use(SiteMigrationRequestGuard(authorizer))
	router.POST(siteMigrationChallengePath, func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusNoContent)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+siteMigrationChallengePath, strings.NewReader(`{"peer_id":"peer_00000000001"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+migrationGuardToken(5))
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || authorizer.calls != 1 {
		t.Fatalf("status=%d auth_calls=%d", response.StatusCode, authorizer.calls)
	}
}

func TestSiteMigrationRequestGuardClosesMalformedChunkedBodyWithoutDraining(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SiteMigrationRequestGuard(nil))
	router.POST(siteMigrationRedeemPath, func(c *gin.Context) {
		var payload map[string]any
		if err := json.NewDecoder(c.Request.Body).Decode(&payload); err != nil {
			AbortSiteMigrationRequestBody(c)
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusNoContent)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	status, closed := performPartialChunkedMigrationRequest(t, server, siteMigrationRedeemPath)
	if status != http.StatusBadRequest || !closed {
		t.Fatalf("status=%d connection_closed=%t", status, closed)
	}
}

func TestSiteMigrationRequestGuardProtectsUnknownNamespaceRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SiteMigrationFailureLimit())
	router.Use(SiteMigrationRequestGuard(nil))
	server := httptest.NewServer(router)
	defer server.Close()

	status, closed := performPartialChunkedMigrationRequestMethod(t, server, http.MethodPost, siteMigrationAPIPath+"/unknown")
	if status != http.StatusUnauthorized || !closed {
		t.Fatalf("status=%d connection_closed=%t", status, closed)
	}
}

func TestSiteMigrationRequestGuardProtectsWrongMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SiteMigrationFailureLimit())
	router.Use(SiteMigrationRequestGuard(nil))
	router.POST(siteMigrationRedeemPath, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	server := httptest.NewServer(router)
	defer server.Close()

	status, closed := performPartialChunkedMigrationRequestMethod(t, server, http.MethodPut, siteMigrationRedeemPath)
	if status != http.StatusNotFound || !closed {
		t.Fatalf("status=%d connection_closed=%t", status, closed)
	}
}

func TestSiteMigrationRequestGuardIsNoOpOutsideMachineNamespace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authorizer := &migrationGuardAuthorizer{err: errors.New("must not be called")}
	router := gin.New()
	router.Use(SiteMigrationRequestGuard(authorizer))
	router.POST("/outside", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/outside", strings.NewReader(`{"ok":true}`))
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || authorizer.calls != 0 {
		t.Fatalf("status=%d auth_calls=%d", recorder.Code, authorizer.calls)
	}
}

func TestSiteMigrationRequestGuardAllowsRedeemWithoutBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	authorizer := &migrationGuardAuthorizer{err: errors.New("must not be called")}
	slots := make(chan struct{}, 1)
	pairingSlots := make(chan struct{}, 1)
	router := gin.New()
	router.Use(newSiteMigrationRequestGuard(authorizer, func() time.Time { return now }, func(http.ResponseWriter, time.Time) error { return nil }, slots, pairingSlots))
	router.POST(siteMigrationRedeemPath, func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, siteMigrationRedeemPath, strings.NewReader(`{"token":"pairing-token"}`))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent || authorizer.calls != 0 || len(pairingSlots) != 0 {
		t.Fatalf("status=%d auth_calls=%d pairing_slots=%d", recorder.Code, authorizer.calls, len(pairingSlots))
	}
}

func TestSiteMigrationRequestGuardFailsClosedWhenDeadlineCannotBeSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	body := &migrationTrackingBody{reader: strings.NewReader(`{"peer_id":"peer_00000000001"}`)}
	slots := make(chan struct{}, 1)
	router := gin.New()
	router.Use(newSiteMigrationRequestGuard(&migrationGuardAuthorizer{}, func() time.Time { return now }, func(_ http.ResponseWriter, deadline time.Time) error {
		if deadline.After(now) {
			return errors.New("read deadlines unavailable")
		}
		return nil
	}, slots, make(chan struct{}, 1)))
	reached := false
	router.POST(siteMigrationChallengePath, func(c *gin.Context) { reached = true })
	req := httptest.NewRequest(http.MethodPost, siteMigrationChallengePath, nil)
	req.Body = body
	req.ContentLength = int64(body.reader.Len())
	req.Header.Set("Authorization", "Bearer "+migrationGuardToken(3))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable || reached || body.reads != 0 || len(slots) != 0 {
		t.Fatalf("status=%d reached=%t reads=%d slots=%d", recorder.Code, reached, body.reads, len(slots))
	}
}

func TestSiteMigrationRequestGuardRejectsWhenReaderLimitIsFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	authorizer := &migrationGuardAuthorizer{}
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	router := gin.New()
	router.Use(newSiteMigrationRequestGuard(authorizer, func() time.Time { return now }, func(http.ResponseWriter, time.Time) error { return nil }, slots, make(chan struct{}, 1)))
	router.POST(siteMigrationChallengePath, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, siteMigrationChallengePath, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+migrationGuardToken(4))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusTooManyRequests || authorizer.calls != 0 || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d auth_calls=%d retry_after=%q", recorder.Code, authorizer.calls, recorder.Header().Get("Retry-After"))
	}
}

func TestSiteMigrationPairingReaderLimitCannotStarveAuthenticatedRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	authorizer := &migrationGuardAuthorizer{}
	readerSlots := make(chan struct{}, 1)
	pairingSlots := make(chan struct{}, 1)
	pairingSlots <- struct{}{}
	router := gin.New()
	router.Use(newSiteMigrationRequestGuard(authorizer, func() time.Time { return now }, func(http.ResponseWriter, time.Time) error { return nil }, readerSlots, pairingSlots))
	router.POST(siteMigrationChallengePath, func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, siteMigrationChallengePath, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+migrationGuardToken(6))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent || authorizer.calls != 1 || len(readerSlots) != 0 {
		t.Fatalf("status=%d auth_calls=%d reader_slots=%d", recorder.Code, authorizer.calls, len(readerSlots))
	}
}

func TestParseSiteMigrationBearerRequiresCanonicalCredential(t *testing.T) {
	token := migrationGuardToken(9)
	if parsed, ok := parseSiteMigrationBearer("Bearer " + token); !ok || parsed != token {
		t.Fatalf("valid token rejected: parsed=%q ok=%t", parsed, ok)
	}
	for _, header := range []string{"bearer " + token, "Bearer " + token + "=", "Bearer " + strings.Repeat("a", 42), "Bearer " + strings.Repeat("!", 43)} {
		if _, ok := parseSiteMigrationBearer(header); ok {
			t.Fatalf("invalid header accepted: %q", header)
		}
	}
}

func migrationGuardToken(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func performPartialChunkedMigrationRequest(t *testing.T, server *httptest.Server, requestPath string) (int, bool) {
	return performPartialChunkedMigrationRequestMethod(t, server, http.MethodPost, requestPath)
}

func performPartialChunkedMigrationRequestMethod(t *testing.T, server *httptest.Server, method, requestPath string) (int, bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	request := method + " " + requestPath + " HTTP/1.1\r\n" +
		"Host: " + server.Listener.Addr().String() + "\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: keep-alive\r\n\r\n" +
		"1\r\n!\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response before slow-body deadline: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return response.StatusCode, response.Close || response.Header.Get("Connection") == "close"
}
