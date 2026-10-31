package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
)

var errMediaNotFound = errors.New("media not found")

type mediaItem struct {
	ID                   string          `json:"id"`
	DiveID               int64           `json:"dive_id"`
	Filename             string          `json:"filename"`
	Kind                 string          `json:"kind"`
	ContentType          string          `json:"content_type"`
	SizeBytes            int64           `json:"size_bytes"`
	ModifiedNS           int64           `json:"-"`
	SHA256               string          `json:"sha256"`
	Width                int             `json:"width"`
	Height               int             `json:"height"`
	DurationSeconds      float64         `json:"duration_seconds"`
	StereoLayout         string          `json:"stereo_layout"`
	DetectedStereoLayout string          `json:"detected_stereo_layout,omitempty"`
	Projection           string          `json:"projection"`
	EffectiveProjection  string          `json:"effective_projection"`
	ProjectionFormat     string          `json:"projection_format,omitempty"`
	Caption              string          `json:"caption"`
	ThumbnailStatus      string          `json:"thumbnail_status"`
	ThumbnailError       string          `json:"thumbnail_error,omitempty"`
	ThumbnailKey         string          `json:"-"`
	Probe                json.RawMessage `json:"probe"`
	CreatedAt            string          `json:"created_at"`
	UpdatedAt            string          `json:"updated_at"`
	URL                  string          `json:"url"`
	ThumbnailURL         string          `json:"thumbnail_url,omitempty"`
}

const mediaColumns = `id, dive_id, filename, kind, content_type, size_bytes, modified_ns,
	sha256, width, height, duration_seconds, stereo_layout, detected_stereo_layout,
	projection, caption, thumbnail_status, thumbnail_error, thumbnail_key, probe_json, created_at, updated_at`

func scanMedia(row rowScanner) (mediaItem, error) {
	var m mediaItem
	var probe string
	err := row.Scan(&m.ID, &m.DiveID, &m.Filename, &m.Kind, &m.ContentType, &m.SizeBytes,
		&m.ModifiedNS, &m.SHA256, &m.Width, &m.Height, &m.DurationSeconds, &m.StereoLayout,
		&m.DetectedStereoLayout, &m.Projection, &m.Caption, &m.ThumbnailStatus,
		&m.ThumbnailError, &m.ThumbnailKey, &probe, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return m, errMediaNotFound
	}
	if err != nil {
		return m, err
	}
	m.Probe = json.RawMessage(probe)
	m.setURLs()
	return m, nil
}

func (m *mediaItem) setURLs() {
	m.resolveProjection()
	id := strconv.FormatInt(m.DiveID, 10)
	m.URL = "/assets/" + id + "/" + url.PathEscape(m.Filename)
	m.ThumbnailURL = ""
	if m.ThumbnailStatus == "ready" {
		m.ThumbnailURL = "/api/v1/dives/" + id + "/media/" + m.ID + "/thumbnail?v=" + m.ThumbnailKey
	}
}

// Projection hints are derived from the retained probe JSON, so existing media
// can expose spherical metadata without another schema migration or download.
func (m *mediaItem) resolveProjection() {
	m.EffectiveProjection, m.ProjectionFormat = m.Projection, ""
	probe, err := decodeMediaProbe(m.Probe)
	if err == nil {
		m.ProjectionFormat = probe.ProjectionFormat
	}
	if m.Projection == "auto" || m.Projection == "" {
		m.EffectiveProjection = "flat"
		if err == nil && probe.Projection != "" {
			m.EffectiveProjection = probe.Projection
		}
	}
	if m.EffectiveProjection == "360" && m.ProjectionFormat == "" {
		// An explicit 360 override denotes an untagged stitched export.
		m.ProjectionFormat = "equirectangular"
	}
}

func (s *sqlStore) mediaForDive(ctx context.Context, diveID int64) ([]mediaItem, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT `+mediaColumns+` FROM dive_media WHERE dive_id = ? ORDER BY filename, id`), diveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []mediaItem{}
	for rows.Next() {
		item, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *sqlStore) mediaByID(ctx context.Context, diveID int64, id string) (mediaItem, error) {
	return scanMedia(s.db.QueryRowContext(ctx, s.bind(`SELECT `+mediaColumns+` FROM dive_media WHERE dive_id = ? AND id = ?`), diveID, id))
}

func (s *sqlStore) listMedia(ctx context.Context, diveID int64, limit int, afterName, afterID string) ([]mediaItem, bool, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT `+mediaColumns+` FROM dive_media
		WHERE dive_id = ? AND (filename > ? OR (filename = ? AND id > ?))
		ORDER BY filename, id LIMIT ?`), diveID, afterName, afterName, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []mediaItem{}
	for rows.Next() {
		item, err := scanMedia(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, rows.Err()
}

func (s *sqlStore) mediaByFilename(ctx context.Context, diveID int64, name string) (mediaItem, error) {
	return scanMedia(s.db.QueryRowContext(ctx, s.bind(`SELECT `+mediaColumns+` FROM dive_media WHERE dive_id = ? AND filename = ?`), diveID, name))
}

func (s *sqlStore) saveMedia(ctx context.Context, m mediaItem) error {
	_, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO dive_media (`+mediaColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dive_id, filename) DO UPDATE SET kind=excluded.kind,
		content_type=excluded.content_type, size_bytes=excluded.size_bytes, modified_ns=excluded.modified_ns,
		sha256=excluded.sha256, width=excluded.width, height=excluded.height,
		duration_seconds=excluded.duration_seconds, stereo_layout=excluded.stereo_layout,
		detected_stereo_layout=excluded.detected_stereo_layout, projection=excluded.projection,
		caption=excluded.caption, thumbnail_status=excluded.thumbnail_status,
		thumbnail_error=excluded.thumbnail_error, thumbnail_key=excluded.thumbnail_key,
		probe_json=excluded.probe_json, updated_at=excluded.updated_at`),
		m.ID, m.DiveID, m.Filename, m.Kind, m.ContentType, m.SizeBytes, m.ModifiedNS,
		m.SHA256, m.Width, m.Height, m.DurationSeconds, m.StereoLayout, m.DetectedStereoLayout,
		m.Projection, m.Caption, m.ThumbnailStatus, m.ThumbnailError, m.ThumbnailKey,
		string(m.Probe), m.CreatedAt, m.UpdatedAt)
	return err
}

func (s *sqlStore) removeMedia(ctx context.Context, m mediaItem) error {
	_, err := s.db.ExecContext(ctx, s.bind(`DELETE FROM dive_media WHERE dive_id = ? AND id = ?`), m.DiveID, m.ID)
	return err
}
