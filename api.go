package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	charmLog "github.com/charmbracelet/log"
)

const (
	defaultDivePageSize   = 50
	maximumDivePageSize   = 100
	defaultSamplePageSize = 5000
	maximumSamplePageSize = 10000
)

type apiServer struct {
	store  *sqlStore
	logger *charmLog.Logger
}

type diveCursor struct {
	LocalDate string `json:"date"`
	StartTime string `json:"time"`
	ID        int64  `json:"id"`
}

type sampleCursor struct {
	Index int `json:"index"`
}

type diveListResponse struct {
	Items      []diveSummary `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type sampleListResponse struct {
	Items      []diveSampleAPIResponse `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newAPIHandler(store *sqlStore, logger *charmLog.Logger) http.Handler {
	server := &apiServer{store: store, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", server.health)
	mux.HandleFunc("GET /api/v1/dives", server.listDives)
	mux.HandleFunc("GET /api/v1/dives/{id}", server.getDive)
	mux.HandleFunc("GET /api/v1/dives/{id}/samples", server.listSamples)
	return apiMiddleware(mux)
}

func apiMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *apiServer) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.db.PingContext(r.Context()); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *apiServer) listDives(w http.ResponseWriter, r *http.Request) {
	limit, err := pageLimit(r, defaultDivePageSize, maximumDivePageSize)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	from, to := strings.TrimSpace(r.URL.Query().Get("from")), strings.TrimSpace(r.URL.Query().Get("to"))
	if err := validateDateRange(from, to); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	options := diveListOptions{Limit: limit, From: from, To: to, Location: strings.TrimSpace(r.URL.Query().Get("location"))}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		var decoded diveCursor
		if err := decodeCursor(cursor, &decoded); err != nil || decoded.ID <= 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		options.AfterDate, options.AfterTime, options.AfterID = decoded.LocalDate, decoded.StartTime, decoded.ID
	}
	items, hasMore, err := s.store.ListDives(r.Context(), options)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := diveListResponse{Items: items}
	if hasMore && len(items) != 0 {
		last := items[len(items)-1]
		response.NextCursor, err = encodeCursor(diveCursor{LocalDate: last.LocalDate, StartTime: last.StartTime, ID: last.ID})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *apiServer) getDive(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	dive, err := s.store.Dive(r.Context(), id)
	if errors.Is(err, errDiveNotFound) {
		writeAPIError(w, http.StatusNotFound, "not_found", "dive not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dive)
}

func (s *apiServer) listSamples(w http.ResponseWriter, r *http.Request) {
	id, ok := diveID(w, r)
	if !ok {
		return
	}
	limit, err := pageLimit(r, defaultSamplePageSize, maximumSamplePageSize)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	after := -1
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		var decoded sampleCursor
		if err := decodeCursor(cursor, &decoded); err != nil || decoded.Index < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		after = decoded.Index
	}
	items, hasMore, err := s.store.DiveSamples(r.Context(), id, after, limit)
	if errors.Is(err, errDiveNotFound) {
		writeAPIError(w, http.StatusNotFound, "not_found", "dive not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := sampleListResponse{Items: items}
	if hasMore && len(items) != 0 {
		response.NextCursor, err = encodeCursor(sampleCursor{Index: items[len(items)-1].Index})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func diveID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "dive ID must be a positive integer")
		return 0, false
	}
	return id, true
}

func pageLimit(r *http.Request, fallback, maximum int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > maximum {
		return 0, fmt.Errorf("limit must be between 1 and %d", maximum)
	}
	return limit, nil
}

func validateDateRange(from, to string) error {
	for name, value := range map[string]string{"from": from, "to": to} {
		if value == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, value); err != nil {
			return fmt.Errorf("%s must use YYYY-MM-DD", name)
		}
	}
	if from != "" && to != "" && from > to {
		return errors.New("from must not be after to")
	}
	return nil
}

func encodeCursor(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(value string, destination any) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("cursor contains trailing data")
		}
		return err
	}
	return nil
}

func (s *apiServer) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if s.logger != nil {
		s.logger.Error("API request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeAPIError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	response := errorResponse{}
	response.Error.Code = code
	response.Error.Message = message
	writeJSON(w, status, response)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func serveAPI(ctx context.Context, address string, store *sqlStore, logger *charmLog.Logger) error {
	if strings.TrimSpace(address) == "" {
		return errors.New("REST API listen address must not be empty")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for REST API: %w", err)
	}
	server := &http.Server{
		Handler:           newAPIHandler(store, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		result <- server.Serve(listener)
	}()
	if logger != nil {
		logger.Info("REST API listening", "address", listener.Addr().String())
		if !isLoopbackAddress(address) {
			logger.Warn("REST API has no authentication; only expose it to trusted networks", "address", address)
		}
	}
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve REST API: %w", err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down REST API: %w", err)
		}
		if err := <-result; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve REST API: %w", err)
		}
		return ctx.Err()
	}
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}
