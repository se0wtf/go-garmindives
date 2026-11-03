package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	charmLog "github.com/charmbracelet/log"
)

func TestCanceledDiveAndTripRequestsDoNotLogServerFailures(t *testing.T) {
	s, _ := openTestSQLStore(t)
	for _, path := range []string{"/api/v1/dives", "/api/v1/trips"} {
		t.Run(path, func(t *testing.T) {
			var logs bytes.Buffer
			logger := charmLog.NewWithOptions(&logs, charmLog.Options{Level: charmLog.DebugLevel, Formatter: charmLog.JSONFormatter})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			request := httptest.NewRequest("GET", path, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			newAPIHandler(s, logger).ServeHTTP(response, request)
			if response.Body.Len() != 0 || response.Header().Get("Content-Type") != "" {
				t.Fatalf("wrote a response for an abandoned request: %s", response.Body)
			}
			var entry struct {
				Level   string `json:"level"`
				Message string `json:"msg"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if entry.Level != "debug" || entry.Message != "API request canceled" || entry.Path != path {
				t.Fatalf("unexpected cancellation log: %s", &logs)
			}
		})
	}
}

func TestInternalErrorDoesNotHideLiveRequestCancellationsOrTimeouts(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, cancelTimeout := context.WithTimeout(context.Background(), -time.Second)
	defer cancelTimeout()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"internal cancellation with live caller", context.Background(), context.Canceled},
		{"request deadline", timedOut, context.DeadlineExceeded},
		{"independent database deadline", context.Background(), fmt.Errorf("query: %w", context.DeadlineExceeded)},
		{"other failure with canceled caller", canceled, errors.New("database unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := charmLog.NewWithOptions(&logs, charmLog.Options{Level: charmLog.DebugLevel, Formatter: charmLog.JSONFormatter})
			server := &apiServer{logger: logger}
			response := httptest.NewRecorder()
			request := httptest.NewRequest("GET", "/api/v1/dives", nil).WithContext(tc.ctx)
			server.internalError(response, request, tc.err)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("expected server error, got %d", response.Code)
			}
			var entry struct {
				Level   string `json:"level"`
				Message string `json:"msg"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if entry.Level != "error" || entry.Message != "API request failed" {
				t.Fatalf("real failure was hidden: %s", &logs)
			}
		})
	}
}
