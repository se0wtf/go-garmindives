package app

import (
	"context"
	garminhttp "github.com/se0wtf/go-garmindives/internal/garminhttp"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSyncRateConfiguration(t *testing.T) {
	clearApplicationEnvironment(t)
	store, _ := openTestSQLStore(t)
	configuredArgs := configuredServeArgs(t, store, nil)
	for _, invalid := range []string{"0", "0s", "-2s", "invalid"} {
		t.Setenv("DIVELOG_GARMIN_REQUEST_INTERVAL", invalid)
		if err := Run(context.Background(), append([]string{"sync"}, configuredArgs...), nil, io.Discard, testLogger(t)); err == nil || !strings.Contains(err.Error(), "garmin.request_interval") {
			t.Fatalf("sync did not validate request interval before starting: %v", err)
		}
	}
	if err := Run(context.Background(), []string{"serve", "-garmin-request-interval=2s", "-help"}, nil, io.Discard, testLogger(t)); err != nil {
		t.Fatal(err)
	}
}

type rateTestClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *rateTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *rateTestClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, delay)
	c.now = c.now.Add(delay)
	return nil
}

func rateTestClient(t *testing.T, base http.RoundTripper) (*http.Client, *rateTestClock) {
	t.Helper()
	clock := &rateTestClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	client, err := garminhttp.NewClient(garminhttp.DefaultRequestInterval, testLogger(t), garminhttp.WithTransport(base), garminhttp.WithClock(clock.Now, clock.Wait))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, clock
}
