package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"

	charmLog "github.com/charmbracelet/log"
)

const (
	defaultRequestInterval = 2 * time.Second
	garminRequestTimeout   = 30 * time.Second
	garminRateLimitRetries = 3
	garminRateLimitBackoff = 30 * time.Second
)

func newGarminHTTPClientFromEnvironment(logger *charmLog.Logger) (*http.Client, error) {
	configured := envDefault("GARMIN_REQUEST_INTERVAL", defaultRequestInterval.String())
	interval, err := time.ParseDuration(configured)
	if err != nil || interval <= 0 {
		return nil, errors.New("GARMIN_REQUEST_INTERVAL must be a positive duration, for example 2s or 5s")
	}
	return newGarminHTTPClient(interval, logger)
}

func newGarminHTTPClient(interval time.Duration, logger *charmLog.Logger) (*http.Client, error) {
	if interval <= 0 {
		return nil, errors.New("Garmin request interval must be positive")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create Garmin cookie jar: %w", err)
	}
	base := http.DefaultTransport
	if transport, ok := base.(*http.Transport); ok {
		base = transport.Clone()
	}
	if logger != nil {
		logger.Debug("Garmin request pacing configured", "interval", interval)
	}
	return &http.Client{
		Jar: jar,
		// Timeouts apply to individual network attempts, after pacing/cooldown
		// waits. A legitimate Retry-After can be longer than 30 seconds.
		Transport: &garminRateTransport{
			base: base, interval: interval, gate: make(chan struct{}, 1),
			now: time.Now, wait: waitForGarmin, timeout: garminRequestTimeout, logger: logger,
		},
	}, nil
}

// A single transport is shared by all Garmin sessions during a sync. Holding
// gate through response headers prevents bursts even with concurrent callers.
type garminRateTransport struct {
	base     http.RoundTripper
	interval time.Duration
	gate     chan struct{}
	next     time.Time // Protected by gate.
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
	timeout  time.Duration
	logger   *charmLog.Logger
}

func (t *garminRateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	select {
	case t.gate <- struct{}{}:
		defer func() { <-t.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if delay := t.next.Sub(t.now()); delay > 0 {
			if err := t.wait(ctx, delay); err != nil {
				return nil, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := t.roundTripAttempt(request)
		t.next = t.now().Add(t.interval)
		if err != nil || response.StatusCode != http.StatusTooManyRequests {
			return response, err
		}
		delay := garminRetryDelay(response.Header.Get("Retry-After"), t.now(), attempt, t.interval)
		t.next = t.now().Add(delay)
		retry := retryableGarminRequest(request) && attempt < garminRateLimitRetries
		if t.logger != nil {
			t.logger.Warn("Garmin rate limit reached", "cooldown", delay, "retrying", retry)
		}
		if !retry {
			return response, nil
		}
		// Release the response and its attempt timeout before waiting. Request
		// headers and authentication response bodies never enter application logs.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}
}

func (t *garminRateTransport) roundTripAttempt(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(request.Context(), t.timeout)
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if response == nil {
		cancel()
		return nil, errors.New("Garmin transport returned no response")
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = &garminResponseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (t *garminRateTransport) CloseIdleConnections() {
	if transport, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

type garminResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *garminResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func retryableGarminRequest(request *http.Request) bool {
	if request.Body != nil && request.Body != http.NoBody {
		return false
	}
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return true
	}
	// The Personal API token exchange has an empty body. Login/MFA POSTs and
	// other operations are not automatically replayed.
	return request.Method == http.MethodPost &&
		strings.HasSuffix(request.URL.Path, "/personalapitoken-service/personal-api/personal-api-key/token")
}

func garminRetryDelay(header string, now time.Time, attempt int, interval time.Duration) time.Duration {
	delay := garminRateLimitBackoff << min(attempt, 2)
	header = strings.TrimSpace(header)
	if seconds, err := strconv.ParseUint(header, 10, 64); err == nil {
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			delay = time.Duration(math.MaxInt64)
		} else {
			delay = time.Duration(seconds) * time.Second
		}
	} else if date, err := http.ParseTime(header); err == nil {
		delay = date.Sub(now)
	}
	return max(delay, interval)
}

func waitForGarmin(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
