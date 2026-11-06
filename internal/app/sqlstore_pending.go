package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

var errPendingNotFound = errors.New("pending media not found")
var errPendingChanged = errors.New("pending media changed; rescan before trying again")
var errPendingAssigned = errors.New("photo already assigned; use the dive media API to edit or delete it")

type pendingItem struct {
	ID              string                 `json:"id"`
	Filename        string                 `json:"filename"`
	Kind            string                 `json:"kind"`
	SizeBytes       int64                  `json:"size_bytes"`
	SHA256          string                 `json:"sha256"`
	Capture         *mediaproc.CaptureTime `json:"capture"`
	Reason          string                 `json:"reason"`
	Error           string                 `json:"error,omitempty"`
	Candidates      []pendingCandidate     `json:"candidates"`
	State           string                 `json:"state"`
	TargetDiveID    int64                  `json:"target_dive_id,omitempty"`
	MediaID         string                 `json:"media_id,omitempty"`
	CreatedAt       string                 `json:"created_at"`
	UpdatedAt       string                 `json:"updated_at"`
	URL             string                 `json:"url"`
	ThumbnailURL    string                 `json:"thumbnail_url"`
	ModifiedNS      int64                  `json:"-"`
	StableSince     int64                  `json:"-"`
	MetadataReady   bool                   `json:"-"`
	TransferSHA     string                 `json:"-"`
	TransferCapture *photoCapture          `json:"-"`
}

const pendingColumns = `id,filename,size_bytes,modified_ns,sha256,capture_json,metadata_ready,reason,last_error,candidates_json,stable_since,state,target_dive_id,media_id,transfer_sha256,transfer_capture_json,created_at,updated_at`

func scanPending(row rowScanner) (pendingItem, error) {
	var item pendingItem
	var capture, candidates, transfer string
	err := row.Scan(&item.ID, &item.Filename, &item.SizeBytes, &item.ModifiedNS, &item.SHA256, &capture, &item.MetadataReady, &item.Reason, &item.Error, &candidates, &item.StableSince, &item.State, &item.TargetDiveID, &item.MediaID, &item.TransferSHA, &transfer, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, errPendingNotFound
	}
	if err != nil {
		return item, err
	}
	item.Kind, _ = mediaType(item.Filename)
	if capture != "{}" && capture != "null" {
		if err := json.Unmarshal([]byte(capture), &item.Capture); err != nil {
			return item, err
		}
	}
	if err := json.Unmarshal([]byte(candidates), &item.Candidates); err != nil {
		return item, err
	}
	if item.Candidates == nil {
		item.Candidates = []pendingCandidate{}
	}
	if transfer != "{}" && transfer != "null" {
		if err := json.Unmarshal([]byte(transfer), &item.TransferCapture); err != nil {
			return item, err
		}
	}
	base := "/api/v1/admin/media/pending/" + url.PathEscape(item.ID)
	item.URL, item.ThumbnailURL = base+"/file", base+"/thumbnail"
	return item, nil
}

func (s *sqlStore) pendingByID(ctx context.Context, id string) (pendingItem, error) {
	return scanPending(s.db.QueryRowContext(ctx, s.bind(`SELECT `+pendingColumns+` FROM pending_media WHERE id=?`), id))
}
func (s *sqlStore) pendingByFilename(ctx context.Context, name string) (pendingItem, error) {
	return scanPending(s.db.QueryRowContext(ctx, s.bind(`SELECT `+pendingColumns+` FROM pending_media WHERE filename=?`), name))
}
func (s *sqlStore) pendingItems(ctx context.Context) ([]pendingItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pendingColumns+` FROM pending_media ORDER BY filename,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []pendingItem{}
	for rows.Next() {
		item, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
func (s *sqlStore) savePending(ctx context.Context, item pendingItem) error {
	_, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO pending_media (`+pendingColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET size_bytes=excluded.size_bytes,modified_ns=excluded.modified_ns,sha256=excluded.sha256,
		capture_json=excluded.capture_json,metadata_ready=excluded.metadata_ready,reason=excluded.reason,last_error=excluded.last_error,
		candidates_json=excluded.candidates_json,stable_since=excluded.stable_since,state=excluded.state,target_dive_id=excluded.target_dive_id,
		media_id=excluded.media_id,transfer_sha256=excluded.transfer_sha256,transfer_capture_json=excluded.transfer_capture_json,updated_at=excluded.updated_at`),
		item.ID, item.Filename, item.SizeBytes, item.ModifiedNS, item.SHA256, captureJSON(item.Capture), boolNumber(item.MetadataReady), item.Reason, item.Error, captureJSON(item.Candidates), item.StableSince, item.State, item.TargetDiveID, item.MediaID, item.TransferSHA, captureJSON(item.TransferCapture), item.CreatedAt, item.UpdatedAt)
	return err
}

type pendingImport struct {
	ID      string
	SHA     string
	Capture *photoCapture
}

func (s *sqlStore) pendingImportForFile(ctx context.Context, owner mediaOwner, name string) (*pendingImport, error) {
	if owner.TripID != "" {
		return nil, nil
	}
	var seed pendingImport
	var capture string
	err := s.db.QueryRowContext(ctx, s.bind(`SELECT media_id,transfer_sha256,transfer_capture_json FROM pending_media WHERE state='assigning' AND target_dive_id=? AND filename=?`), owner.DiveID, name).Scan(&seed.ID, &seed.SHA, &capture)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if capture != "{}" && capture != "null" {
		if err := json.Unmarshal([]byte(capture), &seed.Capture); err != nil {
			return nil, err
		}
	}
	return &seed, nil
}
