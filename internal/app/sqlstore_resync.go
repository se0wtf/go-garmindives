package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

var errResyncNotFound = errors.New("resync job not found")
var errResyncBusy = errors.New("another resync is active")

type resyncJob struct {
	ID                string  `json:"id"`
	Scope             string  `json:"scope"`
	ActivityID        int64   `json:"activity_id,omitempty"`
	Status            string  `json:"status"`
	Phase             string  `json:"phase"`
	CancelRequested   bool    `json:"cancel_requested"`
	CurrentActivityID int64   `json:"current_activity_id,omitempty"`
	Total             int     `json:"total"`
	Completed         int     `json:"completed"`
	Succeeded         int     `json:"succeeded"`
	Failed            int     `json:"failed"`
	Skipped           int     `json:"skipped"`
	Remaining         int     `json:"remaining"`
	ProgressPercent   float64 `json:"progress_percent"`
	ErrorCode         string  `json:"error_code,omitempty"`
	Error             string  `json:"error,omitempty"`
	CreatedAt         string  `json:"created_at"`
	StartedAt         string  `json:"started_at,omitempty"`
	CompletedAt       string  `json:"completed_at,omitempty"`
	URL               string  `json:"url"`
	ItemsURL          string  `json:"items_url"`
	createdMS         int64
}
type resyncItem struct {
	ActivityID  int64  `json:"activity_id"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	Error       string `json:"error,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

const resyncJobColumns = `j.id,j.scope,j.activity_id,j.status,j.phase,j.cancel_requested,j.current_activity_id,j.error_code,j.error_message,j.created_at_ms,j.started_at_ms,j.completed_at_ms,
	(SELECT COUNT(*) FROM resync_items i WHERE i.job_id=j.id),
	(SELECT COUNT(*) FROM resync_items i WHERE i.job_id=j.id AND i.status='succeeded'),
	(SELECT COUNT(*) FROM resync_items i WHERE i.job_id=j.id AND i.status='failed'),
	(SELECT COUNT(*) FROM resync_items i WHERE i.job_id=j.id AND i.status='skipped')`

func resyncTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}
func scanResyncJob(row rowScanner) (resyncJob, error) {
	var job resyncJob
	var started, completed int64
	err := row.Scan(&job.ID, &job.Scope, &job.ActivityID, &job.Status, &job.Phase, &job.CancelRequested, &job.CurrentActivityID, &job.ErrorCode, &job.Error, &job.createdMS, &started, &completed, &job.Total, &job.Succeeded, &job.Failed, &job.Skipped)
	if errors.Is(err, sql.ErrNoRows) {
		return job, errResyncNotFound
	}
	if err != nil {
		return job, err
	}
	job.Completed = job.Succeeded + job.Failed
	job.Remaining = max(0, job.Total-job.Completed-job.Skipped)
	if job.Total > 0 {
		job.ProgressPercent = 100 * float64(job.Completed) / float64(job.Total)
	} else if job.Status == "succeeded" {
		job.ProgressPercent = 100
	}
	job.CreatedAt, job.StartedAt, job.CompletedAt = resyncTime(job.createdMS), resyncTime(started), resyncTime(completed)
	job.URL = "/api/v1/admin/resync/jobs/" + job.ID
	job.ItemsURL = job.URL + "/items"
	return job, nil
}
func (s *sqlStore) resyncJob(ctx context.Context, id string) (resyncJob, error) {
	return scanResyncJob(s.db.QueryRowContext(ctx, s.bind(`SELECT `+resyncJobColumns+` FROM resync_jobs j WHERE j.id=?`), id))
}
func (s *sqlStore) activeResyncJob(ctx context.Context) (resyncJob, error) {
	return scanResyncJob(s.db.QueryRowContext(ctx, `SELECT `+resyncJobColumns+` FROM resync_jobs j WHERE j.active=1`))
}

func (s *sqlStore) enqueueResync(ctx context.Context, id int64) (resyncJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return resyncJob{}, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM resync_jobs WHERE active=1`).Scan(&exists); err != nil {
		return resyncJob{}, err
	}
	if exists != 0 {
		return resyncJob{}, errResyncBusy
	}
	if id > 0 {
		if err := tx.QueryRowContext(ctx, s.bind(`SELECT id FROM dives WHERE id=?`), id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return resyncJob{}, errDiveNotFound
		} else if err != nil {
			return resyncJob{}, err
		}
	}
	jobID, scope, now := identity.NewID(), "all", time.Now().UnixMilli()
	if id > 0 {
		scope = "activity"
	}
	_, err = tx.ExecContext(ctx, s.bind(`INSERT INTO resync_jobs(id,scope,activity_id,status,phase,active,created_at_ms) VALUES(?,?,?,'queued','queued',1,?)`), jobID, scope, id, now)
	if err != nil {
		tx.Rollback()
		if _, activeErr := s.activeResyncJob(ctx); activeErr == nil {
			return resyncJob{}, errResyncBusy
		}
		return resyncJob{}, err
	}
	if id > 0 {
		_, err = tx.ExecContext(ctx, s.bind(`INSERT INTO resync_items(job_id,activity_id) VALUES(?,?)`), jobID, id)
	} else {
		_, err = tx.ExecContext(ctx, s.bind(`INSERT INTO resync_items(job_id,activity_id) SELECT ?,id FROM dives`), jobID)
	}
	if err != nil {
		return resyncJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return resyncJob{}, err
	}
	return s.resyncJob(ctx, jobID)
}

// Apply only Garmin metadata and this job item's success in one short transaction.
// No FIT document/blob/profile read, insert, update or delete is possible here.
func (s *sqlStore) saveResyncMetadata(ctx context.Context, jobID string, record DiveRecord) error {
	if record.FIT != nil {
		return errors.New("resync cannot save FIT data")
	}
	dive, err := prepareStoredDive(record)
	if err != nil {
		return &resyncFailure{"invalid_garmin_data", "Garmin returned invalid activity data; the stored dive was retained", false}
	}
	tx, err := s.beginTravelTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, s.bind(`SELECT id FROM dives WHERE id=?`), record.ID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return errDiveNotFound
	} else if err != nil {
		return err
	}
	var active, cancel int
	var itemStatus string
	if err := tx.QueryRowContext(ctx, s.bind(`SELECT j.active,j.cancel_requested,i.status FROM resync_jobs j JOIN resync_items i ON i.job_id=j.id WHERE j.id=? AND i.activity_id=?`), jobID, record.ID).Scan(&active, &cancel, &itemStatus); err != nil {
		return err
	}
	if active != 1 || cancel != 0 {
		return context.Canceled
	}
	if itemStatus != "running" {
		return errors.New("resync target is not running")
	}
	if err := s.refreshDiveMetadata(ctx, tx, dive); err != nil {
		return err
	}
	if err := reconcileCountriesTx(ctx, s, tx); err != nil {
		return err
	}
	if err := reconcileTripsTx(ctx, s, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.bind(`UPDATE resync_items SET status='succeeded',completed_at_ms=? WHERE job_id=? AND activity_id=?`), time.Now().UnixMilli(), jobID, record.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.translationsChanged()
	return nil
}

func (s *sqlStore) finishResync(ctx context.Context, id, status, code, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, s.bind(`UPDATE resync_items SET status='skipped',error_code=?,error_message=?,completed_at_ms=? WHERE job_id=? AND status IN ('queued','running')`), code, message, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.bind(`UPDATE resync_jobs SET status=?,phase='idle',active=0,current_activity_id=0,error_code=?,error_message=?,completed_at_ms=? WHERE id=? AND active=1`), status, code, message, now, id); err != nil {
		return err
	}
	return tx.Commit()
}
