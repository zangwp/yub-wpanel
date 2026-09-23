package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	siteMigrationBodyReadTimeout             = 30 * time.Second
	siteMigrationMaxConcurrentReaders        = 64
	siteMigrationMaxConcurrentPairingReaders = 8
	siteMigrationAPIPath                     = "/api/site-migration/v1"
	siteMigrationRedeemPath                  = "/api/site-migration/v1/pair/redeem"
	siteMigrationChallengePath               = "/api/site-migration/v1/pair/challenge"
	siteMigrationAbortBodyKey                = "yub-wpanel.site-migration.abort-body"
)

type siteMigrationBearerAuthorizer interface {
	AuthorizeMachineBearer(context.Context, string, bool) error
}

type siteMigrationReadDeadlineSetter func(http.ResponseWriter, time.Time) error

func setSiteMigrationReadDeadline(writer http.ResponseWriter, deadline time.Time) error {
	return http.NewResponseController(writer).SetReadDeadline(deadline)
}

// SiteMigrationRequestGuard authenticates machine credentials before reading
// JSON and bounds both the time and concurrency spent receiving request bodies.
// The one-time pairing redemption endpoint has no bearer yet, so it receives
// the same body protections without the credential check.
func SiteMigrationRequestGuard(authorizer siteMigrationBearerAuthorizer) gin.HandlerFunc {
	return newSiteMigrationRequestGuard(
		authorizer,
		time.Now,
		setSiteMigrationReadDeadline,
		make(chan struct{}, siteMigrationMaxConcurrentReaders),
		make(chan struct{}, siteMigrationMaxConcurrentPairingReaders),
	)
}

func newSiteMigrationRequestGuard(
	authorizer siteMigrationBearerAuthorizer,
	now func() time.Time,
	setReadDeadline siteMigrationReadDeadlineSetter,
	readerSlots chan struct{},
	pairingReaderSlots chan struct{},
) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		// This middleware is installed globally so it also protects unknown
		// routes and wrong-method requests in the public machine namespace.
		// Requests outside that namespace must pass through unchanged.
		if !isSiteMigrationAPIPath(path) {
			c.Next()
			return
		}
		activeSlots := readerSlots
		if path == siteMigrationRedeemPath {
			activeSlots = pairingReaderSlots
		}
		if path != siteMigrationRedeemPath {
			bearer, ok := parseSiteMigrationBearer(c.GetHeader("Authorization"))
			if !ok {
				abortSiteMigrationWithoutReading(c, http.StatusUnauthorized, now, setReadDeadline)
				return
			}
			if !acquireSiteMigrationReader(activeSlots) {
				c.Header("Retry-After", "1")
				abortSiteMigrationWithoutReading(c, http.StatusTooManyRequests, now, setReadDeadline)
				return
			}
			if authorizer == nil || authorizer.AuthorizeMachineBearer(c.Request.Context(), bearer, path == siteMigrationChallengePath) != nil {
				releaseSiteMigrationReader(activeSlots)
				abortSiteMigrationWithoutReading(c, http.StatusUnauthorized, now, setReadDeadline)
				return
			}
		} else if !acquireSiteMigrationReader(activeSlots) {
			c.Header("Retry-After", "1")
			abortSiteMigrationWithoutReading(c, http.StatusTooManyRequests, now, setReadDeadline)
			return
		}

		released := false
		finish := func() {
			if released {
				return
			}
			released = true
			releaseSiteMigrationReader(activeSlots)
		}

		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			defer finish()
			c.Next()
			return
		}

		if err := setReadDeadline(c.Writer, now().Add(siteMigrationBodyReadTimeout)); err != nil {
			finish()
			abortSiteMigrationWithoutReading(c, http.StatusServiceUnavailable, now, setReadDeadline)
			return
		}

		guardedBody := &siteMigrationDeadlineBody{
			ReadCloser: c.Request.Body,
			onEOF: func() {
				_ = setReadDeadline(c.Writer, time.Time{})
				finish()
			},
			onAbort: func() {
				abortSiteMigrationBody(c, now, setReadDeadline)
				finish()
			},
		}
		c.Request.Body = guardedBody
		c.Set(siteMigrationAbortBodyKey, func() { guardedBody.abort() })
		defer guardedBody.abort()
		c.Next()
	}
}

func isSiteMigrationAPIPath(path string) bool {
	return path == siteMigrationAPIPath || strings.HasPrefix(path, siteMigrationAPIPath+"/")
}

// AbortSiteMigrationRequestBody marks a partially consumed machine request as
// non-reusable before a handler writes its error response. This prevents
// net/http from draining an attacker-controlled slow body after JSON decoding
// or size validation fails.
func AbortSiteMigrationRequestBody(c *gin.Context) {
	if c == nil {
		return
	}
	value, ok := c.Get(siteMigrationAbortBodyKey)
	if !ok {
		return
	}
	if abort, ok := value.(func()); ok {
		abort()
	}
}

func parseSiteMigrationBearer(value string) (string, bool) {
	const prefix = "Bearer "
	if len(value) != len(prefix)+base64.RawURLEncoding.EncodedLen(sha256.Size) || value[:len(prefix)] != prefix {
		return "", false
	}
	token := value[len(prefix):]
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return "", false
	}
	return token, true
}

func acquireSiteMigrationReader(slots chan struct{}) bool {
	if slots == nil {
		return false
	}
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseSiteMigrationReader(slots chan struct{}) {
	if slots != nil {
		<-slots
	}
}

func abortSiteMigrationWithoutReading(c *gin.Context, status int, now func() time.Time, setReadDeadline siteMigrationReadDeadlineSetter) {
	abortSiteMigrationBody(c, now, setReadDeadline)
	c.AbortWithStatus(status)
}

func abortSiteMigrationBody(c *gin.Context, now func() time.Time, setReadDeadline siteMigrationReadDeadlineSetter) {
	c.Request.Close = true
	c.Header("Connection", "close")
	c.Header("Cache-Control", "no-store")
	// net/http may otherwise drain an unread request body after the handler
	// returns. Keep the read side expired so cleanup cannot become a second
	// slow-body path.
	_ = setReadDeadline(c.Writer, now())
}

type siteMigrationDeadlineBody struct {
	io.ReadCloser
	onEOF   func()
	onAbort func()
	once    sync.Once
}

func (b *siteMigrationDeadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.once.Do(b.onEOF)
	} else if err != nil {
		b.abort()
	}
	return n, err
}

func (b *siteMigrationDeadlineBody) Close() error {
	// Expire the read side before the server request body's Close method gets a
	// chance to drain unread bytes.
	b.abort()
	return b.ReadCloser.Close()
}

func (b *siteMigrationDeadlineBody) abort() {
	b.once.Do(b.onAbort)
}
