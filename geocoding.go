package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	charmLog "github.com/charmbracelet/log"
)

const geocodingAttribution = "© OpenStreetMap contributors, ODbL 1.0 — https://www.openstreetmap.org/copyright"

type geocodingConfig struct {
	enabled             bool
	endpoint, userAgent string
	interval            time.Duration
}

func geocodingConfigFromEnvironment() (geocodingConfig, error) {
	c := geocodingConfig{endpoint: envDefault("GARMIN_GEOCODING_URL", "https://nominatim.openstreetmap.org"), userAgent: envDefault("GARMIN_GEOCODING_USER_AGENT", "go-garmindives/1.0 (https://github.com/se0wtf/go-garmindives)")}
	var err error
	c.enabled, err = strconv.ParseBool(envDefault("GARMIN_GEOCODING_ENABLED", "true"))
	if err != nil {
		return c, errors.New("GARMIN_GEOCODING_ENABLED must be true or false")
	}
	c.interval, err = time.ParseDuration(envDefault("GARMIN_GEOCODING_INTERVAL", "15s"))
	if err != nil || c.interval <= 0 {
		return c, errors.New("GARMIN_GEOCODING_INTERVAL must be positive")
	}
	u, err := url.Parse(c.endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("GARMIN_GEOCODING_URL must be an origin without credentials, path, or query")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return c, errors.New("geocoding requires HTTPS except for loopback test/proxy servers")
	}
	if strings.EqualFold(u.Hostname(), "nominatim.openstreetmap.org") && c.interval < 15*time.Second {
		return c, errors.New("public Nominatim background requests require GARMIN_GEOCODING_INTERVAL of at least 15s")
	}
	if len(c.userAgent) < 8 || strings.ContainsAny(c.userAgent, "\r\n") {
		return c, errors.New("GARMIN_GEOCODING_USER_AGENT must identify your application")
	}
	c.endpoint = strings.TrimRight(c.endpoint, "/") + "/reverse"
	return c, nil
}

type countryLookup interface {
	Lookup(context.Context, float64, float64) (string, error)
}
type geocodingFailure struct {
	code      string
	retryable bool
	delay     time.Duration
}

func (e *geocodingFailure) Error() string { return e.code }

type nominatimClient struct {
	config geocodingConfig
	http   *http.Client
}

func newNominatimClient(config geocodingConfig) *nominatimClient {
	return &nominatimClient{config: config, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *nominatimClient) Lookup(ctx context.Context, lat, lon float64) (string, error) {
	u, err := url.Parse(c.config.endpoint)
	if err != nil {
		return "", &geocodingFailure{code: "geocoding_configuration_error"}
	}
	q := u.Query()
	q.Set("format", "jsonv2")
	// Regional boundaries retain overseas territory identifiers (a country-only
	// response labels French Polynesia and New Caledonia as France).
	q.Set("zoom", "5")
	q.Set("addressdetails", "1")
	q.Set("accept-language", "en")
	q.Set("lat", strconv.FormatFloat(lat, 'f', 6, 64))
	q.Set("lon", strconv.FormatFloat(lon, 'f', 6, 64))
	u.RawQuery = q.Encode()
	r, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", &geocodingFailure{code: "geocoding_configuration_error"}
	}
	r.Header.Set("User-Agent", c.config.userAgent)
	r.Header.Set("Accept", "application/json")
	response, err := c.http.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &geocodingFailure{code: "geocoding_network_error", retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return "", nil
	}
	if response.StatusCode != 200 {
		return "", &geocodingFailure{code: fmt.Sprintf("geocoding_http_%d", response.StatusCode), retryable: response.StatusCode == 429 || response.StatusCode >= 500, delay: translationRetryAfter(response.Header.Get("Retry-After"), time.Now())}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return "", &geocodingFailure{code: "invalid_geocoding_response", retryable: true}
	}
	var result struct {
		Address map[string]string `json:"address"`
		Error   string            `json:"error"`
	}
	if json.Unmarshal(body, &result) != nil {
		return "", &geocodingFailure{code: "invalid_geocoding_response", retryable: true}
	}
	if result.Address["country_code"] == "" {
		if result.Error != "" && result.Error != "Unable to geocode" {
			return "", &geocodingFailure{code: "invalid_geocoding_response", retryable: true}
		}
		return "", nil
	}
	code := strings.ToUpper(result.Address["country_code"])
	if code == "FR" {
		territories := map[string]string{"FR-PF": "PF", "FR-NC": "NC", "FR-WF": "WF", "FR-PM": "PM", "FR-TF": "TF", "FR-GP": "GP", "FR-MQ": "MQ", "FR-GF": "GF", "FR-RE": "RE", "FR-YT": "YT", "FR-BL": "BL", "FR-MF": "MF"}
		for key, value := range result.Address {
			if strings.HasPrefix(key, "ISO3166-2-") {
				if territory, ok := territories[value]; ok {
					code = territory
					break
				}
			}
		}
	}
	if countryByCode(code) == nil {
		return "", &geocodingFailure{code: "unknown_geocoding_country"}
	}
	return code, nil
}

type countryService struct {
	store   *sqlStore
	client  countryLookup
	config  geocodingConfig
	owner   string
	logger  *charmLog.Logger
	cancel  context.CancelFunc
	done    chan struct{}
	now     func() time.Time
	blocked atomic.Pointer[string]
}

func newCountryService(s *sqlStore, c geocodingConfig, client countryLookup, logger *charmLog.Logger) *countryService {
	return &countryService{store: s, config: c, client: client, owner: rand.Text(), logger: logger, done: make(chan struct{}), now: time.Now}
}
func (s *countryService) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	go s.run(ctx)
}
func (s *countryService) Close() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
}
func (s *countryService) run(ctx context.Context) {
	defer close(s.done)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = s.store.db.ExecContext(cleanup, s.store.bind(`UPDATE country_lookup_cache SET lease_owner='',lease_until=0 WHERE lease_owner=?`), s.owner)
	}()
	if s.logger != nil {
		s.logger.Info("country lookup worker configured", "enabled", s.client != nil, "request_interval", s.config.interval, "attribution", geocodingAttribution)
	}
	var nextScan time.Time
	for ctx.Err() == nil {
		if !s.now().Before(nextScan) {
			if err := s.store.reconcileTravel(ctx); err != nil && ctx.Err() == nil && s.logger != nil {
				s.logger.Warn("country/trip reconciliation deferred", "err", err)
			}
			nextScan = s.now().Add(30 * time.Second)
		}
		delay := time.Second
		if s.client != nil {
			worked, cooldown, err := s.workOnce(ctx)
			if err != nil && ctx.Err() == nil && s.logger != nil {
				s.logger.Warn("country lookup deferred", "err", err)
			}
			if worked {
				delay = max(s.config.interval, cooldown)
			} else if cooldown > 0 {
				delay = min(cooldown, 30*time.Second)
			} else {
				delay = 30 * time.Second
			}
		} else {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *countryService) workOnce(ctx context.Context) (bool, time.Duration, error) {
	if s.client == nil || s.blocked.Load() != nil {
		return false, 0, nil
	}
	tx, err := s.store.beginTravelTx(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	now := s.now()
	// A database-wide pacing gate also covers concurrent server processes.
	var next int64
	err = tx.QueryRowContext(ctx, `SELECT next_request_at FROM country_lookup_pacing WHERE id=1`).Scan(&next)
	if err != nil {
		return false, 0, err
	}
	if next > now.UnixMilli() {
		return false, time.Duration(next-now.UnixMilli()) * time.Millisecond, nil
	}
	var inflight int
	if err := tx.QueryRowContext(ctx, s.store.bind(`SELECT COUNT(*) FROM country_lookup_cache WHERE lease_until>? AND lease_owner<>''`), now.UnixMilli()).Scan(&inflight); err != nil {
		return false, 0, err
	}
	if inflight > 0 {
		return false, 30 * time.Second, nil
	}
	var key string
	var lat, lon float64
	var attempts int
	err = tx.QueryRowContext(ctx, s.store.bind(`UPDATE country_lookup_cache SET lease_owner=?,lease_until=? WHERE lookup_key=(
	 SELECT c.lookup_key FROM country_lookup_cache c WHERE c.status='pending' AND c.retry_at<=? AND c.lease_until<=?
	 AND EXISTS(SELECT 1 FROM dives d WHERE d.country_lookup_key=c.lookup_key AND d.country_override IS NULL)
	 ORDER BY c.retry_at,c.lookup_key LIMIT 1) AND lease_until<=? RETURNING lookup_key,latitude,longitude,attempts`),
		s.owner, now.Add(2*time.Minute).UnixMilli(), now.UnixMilli(), now.UnixMilli(), now.UnixMilli()).Scan(&key, &lat, &lon, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	r, err := tx.ExecContext(ctx, s.store.bind(`UPDATE country_lookup_pacing SET next_request_at=? WHERE id=1 AND next_request_at<=?`), now.Add(s.config.interval).UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, 0, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return false, s.config.interval, nil
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	code, lookupErr := s.client.Lookup(ctx, lat, lon)
	if ctx.Err() != nil {
		return true, 0, ctx.Err()
	}
	status, lastError, delay := "ready", "", time.Duration(0)
	if lookupErr != nil {
		failure := &geocodingFailure{code: "geocoding_network_error", retryable: true}
		var known *geocodingFailure
		if errors.As(lookupErr, &known) {
			failure = known
		}
		attempts++
		lastError = failure.code
		if failure.code == "geocoding_http_400" || failure.code == "geocoding_http_401" || failure.code == "geocoding_http_403" {
			code := failure.code
			s.blocked.Store(&code)
		}
		status = "error"
		delay = max(30*time.Second*time.Duration(1<<min(attempts-1, 5)), failure.delay)
		if failure.retryable && attempts < 5 {
			status = "pending"
		}
	} else {
		attempts = 0
		if code == "" {
			status = "not_found"
		}
	}
	tx, err = s.store.beginTravelTx(ctx)
	if err != nil {
		return true, delay, err
	}
	defer tx.Rollback()
	var value any
	if code != "" {
		value = code
	}
	_, err = tx.ExecContext(ctx, s.store.bind(`UPDATE country_lookup_cache SET country_code=?,status=?,attempts=?,retry_at=?,last_error=?,lease_owner='',lease_until=0,updated_at=? WHERE lookup_key=? AND lease_owner=?`), value, status, attempts, s.now().Add(delay).UnixMilli(), lastError, s.now().UnixMilli(), key, s.owner)
	if err != nil {
		return true, delay, err
	}
	if delay > 0 {
		until := s.now().Add(max(delay, s.config.interval)).UnixMilli()
		if _, err := tx.ExecContext(ctx, s.store.bind(`UPDATE country_lookup_pacing SET next_request_at=CASE WHEN next_request_at<? THEN ? ELSE next_request_at END WHERE id=1`), until, until); err != nil {
			return true, delay, err
		}
	}
	if err := reconcileCountriesTx(ctx, s.store, tx); err != nil {
		return true, delay, err
	}
	if err := reconcileTripsTx(ctx, s.store, tx); err != nil {
		return true, delay, err
	}
	if err := tx.Commit(); err != nil {
		return true, delay, err
	}
	if s.logger != nil {
		s.logger.Info("country lookup completed", "country", code, "status", status, "error_code", lastError)
	}
	return true, delay, nil
}
