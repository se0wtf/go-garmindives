package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	garmin "github.com/se0wtf/go-garminconnect"
)

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

type rateTestRoundTrip func(*http.Request) (*http.Response, error)

func (f rateTestRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func rateTestClient(t *testing.T, base http.RoundTripper) (*http.Client, *rateTestClock) {
	t.Helper()
	client, err := newGarminHTTPClient(defaultRequestInterval, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	clock := &rateTestClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	transport := client.Transport.(*garminRateTransport)
	if base != nil {
		transport.base = base
	}
	transport.now, transport.wait = clock.Now, clock.Wait
	return client, clock
}

func rateTestResponse(status int, header string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("response"))}
	if header != "" {
		response.Header.Set("Retry-After", header)
	}
	return response
}

func TestGarminRateConfiguration(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"", 2 * time.Second}, {"  ", 2 * time.Second}, {"5s", 5 * time.Second}, {" 750ms ", 750 * time.Millisecond},
	} {
		t.Setenv("GARMIN_REQUEST_INTERVAL", test.value)
		client, err := newGarminHTTPClientFromEnvironment(testLogger(t))
		if err != nil {
			t.Fatal(err)
		}
		transport := client.Transport.(*garminRateTransport)
		if transport.interval != test.want || transport.timeout != 30*time.Second || client.Timeout != 0 || client.Jar == nil {
			t.Fatal("incorrect Garmin pacing, network timeout, or cookie configuration")
		}
		client.CloseIdleConnections()
	}
	for _, invalid := range []string{"0", "0s", "-2s", "invalid"} {
		t.Setenv("GARMIN_REQUEST_INTERVAL", invalid)
		if _, err := newGarminHTTPClientFromEnvironment(testLogger(t)); err == nil {
			t.Fatalf("accepted invalid request interval %q", invalid)
		}
		if err := run(context.Background(), []string{"sync"}, nil, io.Discard, testLogger(t)); err == nil || !strings.Contains(err.Error(), "GARMIN_REQUEST_INTERVAL") {
			t.Fatalf("sync did not validate the request interval before starting: %v", err)
		}
	}
	// Serving local data must not depend on Garmin's network configuration.
	if err := run(context.Background(), []string{"serve", "-help"}, nil, io.Discard, testLogger(t)); err != nil {
		t.Fatal(err)
	}
}

func TestGarminPacingHasNoConcurrentBurst(t *testing.T) {
	var starts []time.Time
	var clock *rateTestClock
	client, fakeClock := rateTestClient(t, rateTestRoundTrip(func(request *http.Request) (*http.Response, error) {
		// RoundTrip calls are serialized even when clients run concurrently.
		starts = append(starts, clock.Now())
		if deadline, ok := request.Context().Deadline(); !ok || time.Until(deadline) > garminRequestTimeout {
			t.Error("each network attempt must have its own timeout")
		}
		return rateTestResponse(http.StatusOK, ""), nil
	}))
	clock = fakeClock
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := client.Get("https://example.invalid/dive")
			if err != nil {
				t.Error(err)
				return
			}
			response.Body.Close()
		}()
	}
	group.Wait()
	if len(starts) != 12 || len(clock.waits) != 11 {
		t.Fatalf("requests=%d waits=%d; want 12/11", len(starts), len(clock.waits))
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < defaultRequestInterval {
			t.Fatalf("request burst: gap %v, want at least %v", gap, defaultRequestInterval)
		}
	}
}

type rateTestBody struct {
	io.Reader
	closed bool
}

func (b *rateTestBody) Close() error { b.closed = true; return nil }

func TestGarmin429BackoffAndRetryLimit(t *testing.T) {
	var bodies []*rateTestBody
	calls := 0
	client, clock := rateTestClient(t, rateTestRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		response := rateTestResponse(http.StatusTooManyRequests, "")
		body := &rateTestBody{Reader: strings.NewReader("limited")}
		bodies = append(bodies, body)
		response.Body = body
		return response, nil
	}))
	response, err := client.Get("https://example.invalid/dive")
	if err != nil || response.StatusCode != http.StatusTooManyRequests || calls != 4 {
		t.Fatalf("expected a final 429 after three retries; calls=%d err=%v", calls, err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}) {
		t.Fatalf("backoff = %v", clock.waits)
	}
	for _, body := range bodies[:3] {
		if !body.closed {
			t.Error("retry leaked a response body")
		}
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "limited" {
		t.Fatal("final rate-limit response must remain available to the API client")
	}
	// The shared cooldown survives retry exhaustion and applies to the next call.
	client.Transport.(*garminRateTransport).base = rateTestRoundTrip(func(*http.Request) (*http.Response, error) {
		return rateTestResponse(http.StatusOK, ""), nil
	})
	response, err = client.Get("https://example.invalid/another-dive")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if clock.waits[len(clock.waits)-1] != 2*time.Minute {
		t.Fatal("429 cooldown was lost between requests")
	}
}

func TestGarminRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		header  string
		attempt int
		want    time.Duration
	}{
		{"120", 0, 2 * time.Minute}, {now.Add(90 * time.Second).Format(http.TimeFormat), 0, 90 * time.Second},
		{"0", 0, 2 * time.Second}, {now.Add(-time.Minute).Format(http.TimeFormat), 0, 2 * time.Second},
		{"", 0, 30 * time.Second}, {"invalid", 1, time.Minute}, {"-1", 2, 2 * time.Minute},
		{"18446744073709551615", 0, time.Duration(math.MaxInt64)},
	} {
		if got := garminRetryDelay(test.header, now, test.attempt, defaultRequestInterval); got != test.want {
			t.Errorf("Retry-After %q, attempt %d = %v; want %v", test.header, test.attempt, got, test.want)
		}
	}
	for _, header := range []string{"120", now.Add(90 * time.Second).Format(http.TimeFormat)} {
		calls := 0
		client, clock := rateTestClient(t, rateTestRoundTrip(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Context().Err() != nil {
				t.Error("rate-limit wait consumed the network timeout")
			}
			if calls == 1 {
				return rateTestResponse(http.StatusTooManyRequests, header), nil
			}
			return rateTestResponse(http.StatusOK, ""), nil
		}))
		response, err := client.Get("https://example.invalid/dive")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if len(clock.waits) != 1 || clock.waits[0] != garminRetryDelay(header, now, 0, defaultRequestInterval) {
			t.Fatal("HTTP transport did not honor Retry-After")
		}
	}
}

func TestGarminRetriesOnlySafeRequestsAnd429(t *testing.T) {
	for _, test := range []struct {
		method, path, body string
		status, calls      int
	}{
		{http.MethodGet, "/dive", "", 429, 2},
		{http.MethodHead, "/dive", "", 429, 2},
		{http.MethodPost, "/personalapitoken-service/personal-api/personal-api-key/token", "", 429, 2},
		{http.MethodPost, "/personalapitoken-service/personal-api/personal-api-key/token", "unexpected-body", 429, 1},
		{http.MethodPost, "/login", "credentials", 429, 1},
		{http.MethodPost, "/mfa", "one-time-code", 429, 1},
		{http.MethodGet, "/dive", "request-body", 429, 1},
		{http.MethodDelete, "/dive", "", 429, 1},
		{http.MethodGet, "/dive", "", 403, 1},
		{http.MethodGet, "/dive", "", 500, 1},
	} {
		calls := 0
		client, _ := rateTestClient(t, rateTestRoundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return rateTestResponse(test.status, "60"), nil
			}
			return rateTestResponse(http.StatusOK, ""), nil
		}))
		var body io.Reader
		if test.body != "" {
			body = strings.NewReader(test.body)
		}
		request, err := http.NewRequest(test.method, "https://example.invalid"+test.path, body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if calls != test.calls {
			t.Errorf("%s %s: calls=%d, want %d", test.method, test.path, calls, test.calls)
		}
	}
}

func TestGarminRateWaitCancellation(t *testing.T) {
	for _, queued := range []bool{false, true} {
		calls := 0
		client, _ := rateTestClient(t, rateTestRoundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return rateTestResponse(http.StatusOK, ""), nil
		}))
		transport := client.Transport.(*garminRateTransport)
		transport.next = transport.now().Add(time.Hour)
		started := make(chan struct{})
		transport.wait = func(ctx context.Context, delay time.Duration) error {
			close(started)
			return waitForGarmin(ctx, delay)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if queued {
			transport.gate <- struct{}{}
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.invalid/dive", nil)
		completed := make(chan error, 1)
		go func() {
			_, err := client.Do(request)
			completed <- err
		}()
		if !queued {
			<-started
		}
		cancel()
		select {
		case err := <-completed:
			if !errors.Is(err, context.Canceled) || calls != 0 {
				t.Fatalf("canceled wait sent a request: calls=%d, err=%v", calls, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancellation did not interrupt Garmin pacing")
		}
		if queued {
			<-transport.gate
		}
	}
}

func TestGarmin429CancellationClosesResponse(t *testing.T) {
	body := &rateTestBody{Reader: strings.NewReader("limited")}
	calls := 0
	client, _ := rateTestClient(t, rateTestRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		response := rateTestResponse(http.StatusTooManyRequests, "3600")
		response.Body = body
		return response, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	client.Transport.(*garminRateTransport).wait = func(ctx context.Context, delay time.Duration) error {
		if delay != time.Hour || !body.closed {
			t.Error("retry did not close its response before waiting")
		}
		cancel()
		return waitForGarmin(ctx, delay)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.invalid/dive", nil)
	_, err := client.Do(request)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("429 cancellation: calls=%d, err=%v", calls, err)
	}
}

func TestGarminTokenRenewalSharesRequestLimiter(t *testing.T) {
	tokens, reads := 0, 0
	httpClient, clock := rateTestClient(t, rateTestRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			tokens++
			payload := fmt.Sprintf(`{"exp":%d,"jti":%d}`, time.Now().Add(time.Hour).Unix(), tokens)
			token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
			response := rateTestResponse(http.StatusOK, "")
			response.Body = io.NopCloser(strings.NewReader(token))
			return response, nil
		}
		reads++
		if reads == 1 {
			return rateTestResponse(http.StatusUnauthorized, ""), nil
		}
		response := rateTestResponse(http.StatusOK, "")
		response.Body = io.NopCloser(strings.NewReader("[]"))
		return response, nil
	}))
	client, err := garmin.NewPersonalClient("synthetic-key", garmin.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListActivities(context.Background(), garmin.ListOptions{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if tokens != 2 || reads != 2 || !reflect.DeepEqual(clock.waits, []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second}) {
		t.Fatalf("token renewal bypassed rate limiting: tokens=%d reads=%d waits=%v", tokens, reads, clock.waits)
	}
}

func TestGarminNetworkTimeoutExcludesRateWait(t *testing.T) {
	calls := 0
	var attemptContext context.Context
	client, _ := rateTestClient(t, rateTestRoundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		attemptContext = request.Context()
		if err := attemptContext.Err(); err != nil {
			t.Errorf("network timeout expired during pacing: %v", err)
		}
		return rateTestResponse(http.StatusOK, ""), nil
	}))
	transport := client.Transport.(*garminRateTransport)
	transport.now, transport.wait = time.Now, waitForGarmin
	transport.timeout = 50 * time.Millisecond
	transport.next = time.Now().Add(100 * time.Millisecond)
	response, err := client.Get("https://example.invalid/dive")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 || !errors.Is(attemptContext.Err(), context.Canceled) {
		t.Fatal("closing the response must release the network timeout context")
	}

	transport.next = time.Time{}
	transport.base = rateTestRoundTrip(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	if _, err := client.Get("https://example.invalid/stalled"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled network attempt did not time out: %v", err)
	}
}
