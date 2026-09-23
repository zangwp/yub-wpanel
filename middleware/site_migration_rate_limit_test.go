package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSiteMigrationFailureLimitBlocksAfterTwentyUnauthorizedResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SiteMigrationFailureLimit())
	router.POST(siteMigrationAPIPath+"/machine", func(c *gin.Context) { c.Status(http.StatusUnauthorized) })

	for i := 0; i < siteMigrationFailureLimit; i++ {
		if status := performMigrationLimitRequest(router); status != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d, want %d", i+1, status, http.StatusUnauthorized)
		}
	}
	if status := performMigrationLimitRequest(router); status != http.StatusTooManyRequests {
		t.Fatalf("blocked status=%d, want %d", status, http.StatusTooManyRequests)
	}
}

func TestSiteMigrationFailureLimitSuccessClearsFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	succeed := false
	router := gin.New()
	router.Use(SiteMigrationFailureLimit())
	router.POST(siteMigrationAPIPath+"/machine", func(c *gin.Context) {
		if succeed {
			c.Status(http.StatusOK)
			return
		}
		c.Status(http.StatusUnauthorized)
	})

	for i := 0; i < siteMigrationFailureLimit-1; i++ {
		performMigrationLimitRequest(router)
	}
	succeed = true
	if status := performMigrationLimitRequest(router); status != http.StatusOK {
		t.Fatalf("success status=%d, want %d", status, http.StatusOK)
	}
	succeed = false
	if status := performMigrationLimitRequest(router); status != http.StatusUnauthorized {
		t.Fatalf("failure after success status=%d, want %d", status, http.StatusUnauthorized)
	}
}

func TestSiteMigrationFailureLimitClosesBlockedSlowBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SiteMigrationFailureLimit())
	router.POST(siteMigrationAPIPath+"/machine", func(c *gin.Context) { c.Status(http.StatusUnauthorized) })
	server := httptest.NewServer(router)
	defer server.Close()

	for i := 0; i < siteMigrationFailureLimit; i++ {
		request, err := http.NewRequest(http.MethodPost, server.URL+siteMigrationAPIPath+"/machine", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d", i+1, response.StatusCode)
		}
	}

	status, closed := performPartialChunkedMigrationRequest(t, server, siteMigrationAPIPath+"/machine")
	if status != http.StatusTooManyRequests || !closed {
		t.Fatalf("status=%d connection_closed=%t", status, closed)
	}
}

func performMigrationLimitRequest(handler http.Handler) int {
	req := httptest.NewRequest(http.MethodPost, siteMigrationAPIPath+"/machine", nil)
	req.RemoteAddr = "203.0.113.25:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}
