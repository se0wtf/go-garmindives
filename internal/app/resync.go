package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	charmLog "github.com/charmbracelet/log"
	garmin "github.com/se0wtf/go-garminconnect"
	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	garminhttp "github.com/se0wtf/go-garmindives/internal/garminhttp"
)

type resyncClientFactory func(context.Context) (resyncClient, error)
type resyncService struct {
	store             *sqlStore
	factory           resyncClientFactory
	logger            *charmLog.Logger
	unavailableCode   string
	httpClient        *http.Client
	wake, enqueueGate chan struct{}
	mu                sync.Mutex
	cancel            context.CancelFunc
	runCancel         context.CancelFunc
	runID             string
	done              chan struct{}
	closed            bool
}

func newResyncServiceWithFactory(store *sqlStore, factory resyncClientFactory, logger *charmLog.Logger) *resyncService {
	return &resyncService{store: store, factory: factory, logger: logger, wake: make(chan struct{}, 1), enqueueGate: make(chan struct{}, 1), done: make(chan struct{})}
}
func newResyncService(store *sqlStore, logger *charmLog.Logger) *resyncService {
	c, err := appconfig.Resolve("config.yaml", appconfig.Default())
	if err != nil {
		s := newResyncServiceWithFactory(store, nil, logger)
		s.unavailableCode = "invalid_garmin_configuration"
		return s
	}
	return newResyncServiceConfigured(store, logger, c)
}
func newResyncServiceConfigured(store *sqlStore, logger *charmLog.Logger, c appconfig.Loaded) *resyncService {
	s := newResyncServiceWithFactory(store, nil, logger)
	key := strings.TrimSpace(c.Effective.Garmin.PersonalAPIKey)
	if key == "" && c.Effective.Garmin.ResyncTokenFile == "" {
		s.unavailableCode = "garmin_not_configured"
		return s
	}
	interval, err := time.ParseDuration(c.Effective.Garmin.RequestInterval)
	if err != nil || interval <= 0 {
		s.unavailableCode = "invalid_garmin_configuration"
		return s
	}
	client, err := garminhttp.NewClient(interval, logger)
	if err != nil {
		s.unavailableCode = "invalid_garmin_configuration"
		return s
	}
	s.httpClient = client
	directory := ""
	if c.Effective.Garmin.ResyncTokenFile == "" {
		directory, err = os.UserConfigDir()
		if err != nil {
			s.unavailableCode = "invalid_garmin_configuration"
			return s
		}
	}
	tokenPath := c.TokenFile(true, directory)
	s.factory = func(ctx context.Context) (resyncClient, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if key != "" {
			return garmin.NewPersonalClient(key, garmin.WithTokenFile(tokenPath), garmin.WithHTTPClient(client))
		}
		// Explicit cached sessions are supported, but the serving process never
		// prompts for credentials/MFA or falls back to interactive login.
		return garmin.NewClientFromTokenFile(tokenPath, garmin.WithHTTPClient(client))
	}
	return s
}
func (s *resyncService) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// The deployment has one serve process. Interrupted jobs remain inspectable and
// never silently resume network work after a restart.
func (s *resyncService) Recover(ctx context.Context) error {
	job, err := s.store.activeResyncJob(ctx)
	if errors.Is(err, errResyncNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	status, code, message := "interrupted", "interrupted", "The serving process stopped before resync completed; completed activities were retained"
	if job.CancelRequested {
		status, code, message = "canceled", "canceled", "Resync was canceled; completed activities were retained"
	}
	return s.store.finishResync(ctx, job.ID, status, code, message)
}
func (s *resyncService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.cancel != nil || s.closed {
		s.mu.Unlock()
		return
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		for {
			if ctx.Err() != nil {
				return
			}
			if err := s.runNext(ctx); err != nil && ctx.Err() == nil && s.logger != nil {
				s.logger.Warn("activity resync worker could not update job state")
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.wake:
				timer.Stop()
			case <-timer.C:
			}
		}
	}()
}
func (s *resyncService) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-s.done
	}
	if s.httpClient != nil {
		s.httpClient.CloseIdleConnections()
	}
}
func (s *resyncService) Enqueue(ctx context.Context, id int64) (resyncJob, error) {
	select {
	case s.enqueueGate <- struct{}{}:
	case <-ctx.Done():
		return resyncJob{}, ctx.Err()
	}
	defer func() { <-s.enqueueGate }()
	job, err := s.store.enqueueResync(ctx, id)
	if err == nil {
		s.signal()
	}
	return job, err
}
func (s *resyncService) Cancel(ctx context.Context, id string) (resyncJob, error) {
	if _, err := s.store.resyncJob(ctx, id); err != nil {
		return resyncJob{}, err
	}
	if _, err := s.store.db.ExecContext(ctx, s.store.bind(`UPDATE resync_jobs SET cancel_requested=1 WHERE id=? AND active=1`), id); err != nil {
		return resyncJob{}, err
	}
	s.mu.Lock()
	if s.runID == id && s.runCancel != nil {
		s.runCancel()
	}
	s.mu.Unlock()
	s.signal()
	return s.store.resyncJob(ctx, id)
}

func (s *resyncService) runNext(parent context.Context) error {
	job, err := s.store.activeResyncJob(parent)
	if errors.Is(err, errResyncNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if job.Status != "queued" {
		return s.store.finishResync(parent, job.ID, "interrupted", "interrupted", "The resync worker stopped before job completion; completed activities were retained")
	}
	result, err := s.store.db.ExecContext(parent, s.store.bind(`UPDATE resync_jobs SET status='running',phase='authenticating',started_at_ms=? WHERE id=? AND status='queued'`), time.Now().UnixMilli(), job.ID)
	if err != nil {
		return err
	}
	claimed, err := result.RowsAffected()
	if err != nil || claimed != 1 {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.runID, s.runCancel = job.ID, cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.runID, s.runCancel = "", nil; s.mu.Unlock() }()
	finish := func(status, code, message string) error {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		err := s.store.finishResync(cleanup, job.ID, status, code, message)
		if s.logger != nil {
			s.logger.Info("activity data resync finished", "job_id", job.ID, "status", status, "error_code", code)
		}
		return err
	}
	interrupted := func() error {
		if parent.Err() != nil {
			return finish("interrupted", "interrupted", "The serving process stopped before resync completed; completed activities were retained")
		}
		return finish("canceled", "canceled", "Resync was canceled; completed activities were retained")
	}
	latest, err := s.store.resyncJob(ctx, job.ID)
	if err != nil {
		if ctx.Err() != nil {
			return interrupted()
		}
		return finish("failed", "database_unavailable", "Resync stopped because job state could not be read")
	}
	if latest.CancelRequested {
		cancel()
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	if s.logger != nil {
		s.logger.Info("activity data resync started", "job_id", job.ID, "scope", job.Scope, "total", job.Total)
	}
	if job.Total == 0 {
		return finish("succeeded", "", "")
	}
	if s.factory == nil {
		return finish("failed", "garmin_not_configured", "Configure Garmin credentials on the serving process before starting resync")
	}
	client, err := s.factory(ctx)
	if ctx.Err() != nil {
		return interrupted()
	}
	if err != nil || client == nil {
		return finish("failed", "garmin_authentication_failed", "The server could not initialize its Garmin session; check its Personal API key or token cache")
	}
	for {
		if ctx.Err() != nil {
			return interrupted()
		}
		var id int64
		err := s.store.db.QueryRowContext(ctx, s.store.bind(`SELECT activity_id FROM resync_items WHERE job_id=? AND status='queued' ORDER BY activity_id LIMIT 1`), job.ID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return interrupted()
			}
			return finish("failed", "database_unavailable", "Resync stopped because job state could not be read")
		}
		if err := s.setCurrent(ctx, job.ID, id); err != nil {
			if ctx.Err() != nil {
				return interrupted()
			}
			return finish("failed", "database_unavailable", "Resync stopped because job state could not be saved")
		}
		record, fetchErr := fetchResyncMetadata(ctx, client, id)
		if ctx.Err() != nil {
			return interrupted()
		}
		var failure *resyncFailure
		if fetchErr != nil {
			failure = classifyResyncError(fetchErr)
		} else {
			if _, err := s.store.db.ExecContext(ctx, s.store.bind(`UPDATE resync_jobs SET phase='saving' WHERE id=? AND active=1`), job.ID); err != nil {
				if ctx.Err() != nil {
					return interrupted()
				}
				return finish("failed", "database_unavailable", "Resync stopped before activity data could be saved")
			}
			if err := s.store.saveResyncMetadata(ctx, job.ID, record); err != nil {
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return interrupted()
				}
				var typed *resyncFailure
				if errors.As(err, &typed) {
					failure = typed
				} else if errors.Is(err, errDiveNotFound) {
					failure = &resyncFailure{"stored_dive_missing", "The stored dive no longer exists", false}
				} else {
					failure = &resyncFailure{"save_failed", "The activity update could not be saved; the previous stored dive was retained", true}
				}
			}
		}
		if failure != nil {
			if _, err := s.store.db.ExecContext(ctx, s.store.bind(`UPDATE resync_items SET status='failed',error_code=?,error_message=?,completed_at_ms=? WHERE job_id=? AND activity_id=?`), failure.code, failure.message, time.Now().UnixMilli(), job.ID, id); err != nil {
				if ctx.Err() != nil {
					return interrupted()
				}
				return finish("failed", "database_unavailable", "Resync stopped because an activity result could not be saved")
			}
			if s.logger != nil {
				s.logger.Warn("activity data resync failed", "job_id", job.ID, "activity_id", id, "error_code", failure.code)
			}
			if failure.fatal {
				status := "failed"
				if current, err := s.store.resyncJob(ctx, job.ID); err == nil && current.Succeeded > 0 {
					status = "partial"
				}
				return finish(status, failure.code, failure.message)
			}
		} else if s.logger != nil {
			s.logger.Info("activity data resynced", "job_id", job.ID, "activity_id", id)
		}
	}
	latest, err = s.store.resyncJob(ctx, job.ID)
	if err != nil {
		if ctx.Err() != nil {
			return interrupted()
		}
		return finish("failed", "database_unavailable", "Resync results could not be read")
	}
	if latest.Failed == 0 {
		return finish("succeeded", "", "")
	}
	status := "partial"
	if latest.Succeeded == 0 {
		status = "failed"
	}
	return finish(status, "activity_errors", "Some activity updates failed; inspect the per-activity results")
}

func (s *resyncService) setCurrent(ctx context.Context, jobID string, id int64) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, s.store.bind(`UPDATE resync_jobs SET phase='fetching',current_activity_id=? WHERE id=? AND active=1`), id, jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.store.bind(`UPDATE resync_items SET status='running' WHERE job_id=? AND activity_id=? AND status='queued'`), jobID, id); err != nil {
		return err
	}
	return tx.Commit()
}
