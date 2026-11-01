package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"

	charmLog "github.com/charmbracelet/log"

	geolookup "github.com/se0wtf/go-garmindives/internal/geocoding"
)

type countryService struct {
	store   *sqlStore
	client  geolookup.Lookup
	config  geolookup.Config
	owner   string
	logger  *charmLog.Logger
	cancel  context.CancelFunc
	done    chan struct{}
	now     func() time.Time
	blocked atomic.Pointer[string]
}

func newCountryService(s *sqlStore, c geolookup.Config, client geolookup.Lookup, logger *charmLog.Logger) *countryService {
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
		s.logger.Info("country lookup worker configured", "enabled", s.client != nil, "request_interval", s.config.Interval, "attribution", geolookup.Attribution)
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
				delay = max(s.config.Interval, cooldown)
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
	r, err := tx.ExecContext(ctx, s.store.bind(`UPDATE country_lookup_pacing SET next_request_at=? WHERE id=1 AND next_request_at<=?`), now.Add(s.config.Interval).UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, 0, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return false, s.config.Interval, nil
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
		failure := &geolookup.Failure{Code: "geocoding_network_error", Retryable: true}
		var known *geolookup.Failure
		if errors.As(lookupErr, &known) {
			failure = known
		}
		attempts++
		lastError = failure.Code
		if failure.Code == "geocoding_http_400" || failure.Code == "geocoding_http_401" || failure.Code == "geocoding_http_403" {
			code := failure.Code
			s.blocked.Store(&code)
		}
		status = "error"
		delay = max(30*time.Second*time.Duration(1<<min(attempts-1, 5)), failure.Delay)
		if failure.Retryable && attempts < 5 {
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
		until := s.now().Add(max(delay, s.config.Interval)).UnixMilli()
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
