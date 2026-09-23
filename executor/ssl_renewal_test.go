package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/models"
)

func TestSSLAutoRenewalEligibility(t *testing.T) {
	for name, tc := range map[string]struct {
		site *models.Website
		want bool
	}{
		"automatic active certificate": {site: &models.Website{Status: models.StatusActive, SSLCertSource: "auto"}, want: true},
		"paused website":               {site: &models.Website{Status: models.StatusPaused, SSLCertSource: "auto"}, want: false},
		"manual certificate":           {site: &models.Website{Status: models.StatusActive, SSLCertSource: "manual"}, want: false},
		"unknown legacy source":        {site: &models.Website{Status: models.StatusActive}, want: false},
		"missing website":              {site: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sslAutoRenewalEligible(tc.site); got != tc.want {
				t.Fatalf("eligible=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnqueueSSLRenewalRetriesTransientQueuePressure(t *testing.T) {
	previousEnqueue := enqueueSSLRenewalTask
	previousDelay := sslRenewalEnqueueRetryDelay
	previousWait := waitBeforeSSLRenewalRetry
	t.Cleanup(func() {
		enqueueSSLRenewalTask = previousEnqueue
		sslRenewalEnqueueRetryDelay = previousDelay
		waitBeforeSSLRenewalRetry = previousWait
	})

	attempts := 0
	enqueueSSLRenewalTask = func(context.Context) error {
		attempts++
		if attempts < 3 {
			return ErrTaskQueueFull
		}
		return nil
	}
	sslRenewalEnqueueRetryDelay = time.Second
	var waits []time.Duration
	waitBeforeSSLRenewalRetry = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	if err := enqueueSSLRenewalWithRetry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d, want 3", attempts)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("retry waits=%v, want [1s 2s]", waits)
	}
}

func TestEnqueueSSLRenewalDoesNotRetryPermanentFailure(t *testing.T) {
	previousEnqueue := enqueueSSLRenewalTask
	previousWait := waitBeforeSSLRenewalRetry
	t.Cleanup(func() {
		enqueueSSLRenewalTask = previousEnqueue
		waitBeforeSSLRenewalRetry = previousWait
	})

	attempts := 0
	wantErr := errors.New("invalid task")
	enqueueSSLRenewalTask = func(context.Context) error {
		attempts++
		return wantErr
	}
	waitBeforeSSLRenewalRetry = func(context.Context, time.Duration) error {
		t.Fatal("permanent failure must not retry")
		return nil
	}
	if err := enqueueSSLRenewalWithRetry(context.Background()); !errors.Is(err, wantErr) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d, want permanent error after one attempt", err, attempts)
	}
}

func TestEnqueueSSLRenewalKeepsRetryingThroughLongQueuePressure(t *testing.T) {
	previousEnqueue := enqueueSSLRenewalTask
	previousDelay := sslRenewalEnqueueRetryDelay
	previousWait := waitBeforeSSLRenewalRetry
	t.Cleanup(func() {
		enqueueSSLRenewalTask = previousEnqueue
		sslRenewalEnqueueRetryDelay = previousDelay
		waitBeforeSSLRenewalRetry = previousWait
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	enqueueSSLRenewalTask = func(context.Context) error {
		attempts++
		if attempts == 10 {
			cancel()
		}
		return ErrTaskQueueUnavailable
	}
	sslRenewalEnqueueRetryDelay = time.Second
	var waits []time.Duration
	waitBeforeSSLRenewalRetry = func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return ctx.Err()
	}

	err := enqueueSSLRenewalWithRetry(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context cancellation after sustained pressure", err)
	}
	if attempts != 10 {
		t.Fatalf("attempts=%d, want retries beyond the old six-attempt limit", attempts)
	}
	if len(waits) != 10 {
		t.Fatalf("wait count=%d, want one bounded wait per rejected attempt", len(waits))
	}
	for _, delay := range waits {
		if delay > sslRenewalEnqueueMaxDelay {
			t.Fatalf("retry delay %v exceeded cap %v", delay, sslRenewalEnqueueMaxDelay)
		}
	}
	if waits[len(waits)-1] != sslRenewalEnqueueMaxDelay {
		t.Fatalf("last retry delay=%v, want capped %v", waits[len(waits)-1], sslRenewalEnqueueMaxDelay)
	}
}
