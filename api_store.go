package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var errDiveNotFound = errors.New("dive not found")

const diveSummaryColumns = `dives.id, dives.garmin_dive_id, dives.dive_number, dives.name, dives.activity_type, dives.dive_type,
	dives.start_time, dives.local_date, dives.timezone, dives.location, dives.entry_latitude, dives.entry_longitude,
	dives.exit_latitude, dives.exit_longitude, dives.notes, dives.description, dives.duration_seconds,
	dives.max_depth_meters, dives.average_depth_meters, dives.average_heart_rate, dives.max_heart_rate,
	dives.min_temperature_celsius, dives.average_temperature_celsius, dives.max_temperature_celsius,
	dives.cover_media_id, cover.id, cover.filename, cover.thumbnail_status, cover.thumbnail_key`

const diveSummaryFrom = `FROM dives LEFT JOIN dive_media AS cover
	ON cover.id = dives.cover_media_id AND cover.dive_id = dives.id AND cover.kind = 'photo'`

type diveCoverPhoto struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
}

type diveSummary struct {
	ID                        int64           `json:"id"`
	GarminDiveID              *int64          `json:"garmin_dive_id,omitempty"`
	DiveNumber                *int            `json:"dive_number,omitempty"`
	Name                      string          `json:"name"`
	ActivityType              string          `json:"activity_type"`
	DiveType                  string          `json:"dive_type"`
	StartTime                 string          `json:"start_time"`
	LocalDate                 string          `json:"local_date"`
	Timezone                  string          `json:"timezone"`
	Location                  string          `json:"location"`
	EntryLatitude             *float64        `json:"entry_latitude,omitempty"`
	EntryLongitude            *float64        `json:"entry_longitude,omitempty"`
	ExitLatitude              *float64        `json:"exit_latitude,omitempty"`
	ExitLongitude             *float64        `json:"exit_longitude,omitempty"`
	Notes                     string          `json:"notes"`
	Description               string          `json:"description"`
	DurationSeconds           *float64        `json:"duration_seconds,omitempty"`
	MaxDepthMeters            *float64        `json:"max_depth_meters,omitempty"`
	AverageDepthMeters        *float64        `json:"average_depth_meters,omitempty"`
	AverageHeartRate          *float64        `json:"average_heart_rate,omitempty"`
	MaxHeartRate              *float64        `json:"max_heart_rate,omitempty"`
	MinTemperatureCelsius     *float64        `json:"min_temperature_celsius,omitempty"`
	AverageTemperatureCelsius *float64        `json:"average_temperature_celsius,omitempty"`
	MaxTemperatureCelsius     *float64        `json:"max_temperature_celsius,omitempty"`
	CoverMediaID              *string         `json:"cover_media_id"`
	CoverPhoto                *diveCoverPhoto `json:"cover_photo"`
}

type diveListOptions struct {
	Limit     int
	From      string
	To        string
	Location  string
	AfterDate string
	AfterTime string
	AfterID   int64
}

func (s *sqlStore) ListDives(ctx context.Context, options diveListOptions) ([]diveSummary, bool, error) {
	conditions := []string{"1 = 1"}
	var arguments []any
	if options.From != "" {
		conditions = append(conditions, "dives.local_date >= ?")
		arguments = append(arguments, options.From)
	}
	if options.To != "" {
		conditions = append(conditions, "dives.local_date <= ?")
		arguments = append(arguments, options.To)
	}
	if options.Location != "" {
		conditions = append(conditions, `LOWER(dives.location) LIKE ? ESCAPE '\'`)
		arguments = append(arguments, "%"+escapeLikePattern(strings.ToLower(options.Location))+"%")
	}
	if options.AfterID != 0 {
		conditions = append(conditions, `(dives.local_date < ? OR
			(dives.local_date = ? AND (dives.start_time < ? OR (dives.start_time = ? AND dives.id < ?))))`)
		arguments = append(arguments, options.AfterDate, options.AfterDate, options.AfterTime, options.AfterTime, options.AfterID)
	}
	query := `SELECT ` + diveSummaryColumns + ` ` + diveSummaryFrom + ` WHERE ` + strings.Join(conditions, " AND ") +
		` ORDER BY dives.local_date DESC, dives.start_time DESC, dives.id DESC LIMIT ?`
	arguments = append(arguments, options.Limit+1)
	rows, err := s.db.QueryContext(ctx, s.bind(query), arguments...)
	if err != nil {
		return nil, false, fmt.Errorf("list dives for API: %w", err)
	}
	defer rows.Close()
	items := make([]diveSummary, 0, options.Limit)
	for rows.Next() {
		item, err := scanDiveSummary(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list dives for API: %w", err)
	}
	hasMore := len(items) > options.Limit
	if hasMore {
		items = items[:options.Limit]
	}
	return items, hasMore, nil
}

func escapeLikePattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

type rowScanner interface {
	Scan(...any) error
}

func scanDiveSummary(row rowScanner) (diveSummary, error) {
	values := newDiveSummaryScan()
	err := row.Scan(values.targets()...)
	if err != nil {
		return diveSummary{}, fmt.Errorf("read dive for API: %w", err)
	}
	return values.summary(), nil
}

type diveSummaryScan struct {
	item                                                        diveSummary
	garminDiveID, diveNumber                                    sql.NullInt64
	entryLatitude, entryLongitude, exitLatitude, exitLongitude  sql.NullFloat64
	duration, maxDepth, averageDepth, averageHR, maxHR          sql.NullFloat64
	minTemperature, averageTemperature, maxTemperature          sql.NullFloat64
	coverMediaID, coverID, coverFilename, coverStatus, coverKey sql.NullString
}

func newDiveSummaryScan() *diveSummaryScan { return &diveSummaryScan{} }

func (v *diveSummaryScan) targets(extra ...any) []any {
	targets := []any{
		&v.item.ID, &v.garminDiveID, &v.diveNumber, &v.item.Name, &v.item.ActivityType, &v.item.DiveType,
		&v.item.StartTime, &v.item.LocalDate, &v.item.Timezone, &v.item.Location,
		&v.entryLatitude, &v.entryLongitude, &v.exitLatitude, &v.exitLongitude,
		&v.item.Notes, &v.item.Description, &v.duration, &v.maxDepth, &v.averageDepth, &v.averageHR, &v.maxHR,
		&v.minTemperature, &v.averageTemperature, &v.maxTemperature,
		&v.coverMediaID, &v.coverID, &v.coverFilename, &v.coverStatus, &v.coverKey,
	}
	return append(targets, extra...)
}

func (v *diveSummaryScan) summary() diveSummary {
	item := v.item
	item.GarminDiveID = nullInt64Pointer(v.garminDiveID)
	if v.diveNumber.Valid {
		value := int(v.diveNumber.Int64)
		item.DiveNumber = &value
	}
	item.EntryLatitude = nullFloatPointer(v.entryLatitude)
	item.EntryLongitude = nullFloatPointer(v.entryLongitude)
	item.ExitLatitude = nullFloatPointer(v.exitLatitude)
	item.ExitLongitude = nullFloatPointer(v.exitLongitude)
	item.DurationSeconds = nullFloatPointer(v.duration)
	item.MaxDepthMeters = nullFloatPointer(v.maxDepth)
	item.AverageDepthMeters = nullFloatPointer(v.averageDepth)
	item.AverageHeartRate = nullFloatPointer(v.averageHR)
	item.MaxHeartRate = nullFloatPointer(v.maxHR)
	item.MinTemperatureCelsius = nullFloatPointer(v.minTemperature)
	item.AverageTemperatureCelsius = nullFloatPointer(v.averageTemperature)
	item.MaxTemperatureCelsius = nullFloatPointer(v.maxTemperature)
	if v.coverMediaID.Valid {
		item.CoverMediaID = &v.coverMediaID.String
	}
	if v.coverID.Valid {
		media := mediaItem{ID: v.coverID.String, DiveID: item.ID, Filename: v.coverFilename.String,
			ThumbnailStatus: v.coverStatus.String, ThumbnailKey: v.coverKey.String}
		media.setURLs()
		item.CoverPhoto = &diveCoverPhoto{ID: media.ID, Filename: media.Filename, URL: media.URL, ThumbnailURL: media.ThumbnailURL}
	}
	return item
}

type diveDetail struct {
	Dive            diveSummary            `json:"dive"`
	Activity        json.RawMessage        `json:"activity"`
	ActivityDetails json.RawMessage        `json:"activity_details"`
	DiveDetails     json.RawMessage        `json:"dive_details"`
	FIT             json.RawMessage        `json:"fit"`
	RecordMetadata  []recordMetadata       `json:"record_metadata"`
	Alerts          []diveAlertAPIResponse `json:"alerts"`
	Media           []mediaItem            `json:"media"`
}

type diveAlertAPIResponse struct {
	Index       int             `json:"index"`
	TimestampMS *int64          `json:"timestamp_ms,omitempty"`
	Type        string          `json:"type"`
	Data        json.RawMessage `json:"data"`
}

func (s *sqlStore) Dive(ctx context.Context, id int64) (diveDetail, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT `+diveSummaryColumns+`, dives.activity_json, dives.activity_details_json, dives.dive_details_json, dives.fit_json `+diveSummaryFrom+` WHERE dives.id = ?`), id)
	var result diveDetail
	var activity, activityDetails, diveDetails, fit string
	item, err := scanDiveSummaryWithDocuments(row, &activity, &activityDetails, &diveDetails, &fit)
	if errors.Is(err, sql.ErrNoRows) {
		return diveDetail{}, errDiveNotFound
	}
	if err != nil {
		return diveDetail{}, err
	}
	result.Dive = item
	result.Activity = json.RawMessage(activity)
	result.ActivityDetails = json.RawMessage(activityDetails)
	result.DiveDetails = json.RawMessage(diveDetails)
	result.FIT = json.RawMessage(fit)
	result.RecordMetadata = []recordMetadata{}
	metadataRows, err := s.db.QueryContext(ctx, s.bind(`SELECT position, key FROM dive_record_metadata WHERE dive_id = ? ORDER BY position`), id)
	if err != nil {
		return diveDetail{}, fmt.Errorf("list dive metadata for API: %w", err)
	}
	for metadataRows.Next() {
		var metadata recordMetadata
		if err := metadataRows.Scan(&metadata.Position, &metadata.Key); err != nil {
			metadataRows.Close()
			return diveDetail{}, fmt.Errorf("read dive metadata for API: %w", err)
		}
		result.RecordMetadata = append(result.RecordMetadata, metadata)
	}
	if err := metadataRows.Err(); err != nil {
		metadataRows.Close()
		return diveDetail{}, fmt.Errorf("list dive metadata for API: %w", err)
	}
	if err := metadataRows.Close(); err != nil {
		return diveDetail{}, fmt.Errorf("close dive metadata rows: %w", err)
	}
	result.Alerts = []diveAlertAPIResponse{}
	alertRows, err := s.db.QueryContext(ctx, s.bind(`SELECT alert_index, timestamp_ms, alert_type, data_json FROM dive_alerts WHERE dive_id = ? ORDER BY alert_index`), id)
	if err != nil {
		return diveDetail{}, fmt.Errorf("list dive alerts for API: %w", err)
	}
	defer alertRows.Close()
	for alertRows.Next() {
		var alert diveAlertAPIResponse
		var timestamp sql.NullInt64
		var data string
		if err := alertRows.Scan(&alert.Index, &timestamp, &alert.Type, &data); err != nil {
			return diveDetail{}, fmt.Errorf("read dive alert for API: %w", err)
		}
		alert.TimestampMS = nullInt64Pointer(timestamp)
		alert.Data = json.RawMessage(data)
		result.Alerts = append(result.Alerts, alert)
	}
	if err := alertRows.Err(); err != nil {
		return diveDetail{}, fmt.Errorf("list dive alerts for API: %w", err)
	}
	if err := alertRows.Close(); err != nil {
		return diveDetail{}, err
	}
	result.Media, err = s.mediaForDive(ctx, id)
	if err != nil {
		return diveDetail{}, fmt.Errorf("list dive media: %w", err)
	}
	return result, nil
}

func scanDiveSummaryWithDocuments(row rowScanner, activity, activityDetails, diveDetails, fit *string) (diveSummary, error) {
	values := newDiveSummaryScan()
	err := row.Scan(values.targets(activity, activityDetails, diveDetails, fit)...)
	if err != nil {
		return diveSummary{}, err
	}
	return values.summary(), nil
}

type diveSampleAPIResponse struct {
	Index                   int             `json:"index"`
	TimestampMS             int64           `json:"timestamp_ms"`
	ElapsedMS               int64           `json:"elapsed_ms"`
	HeartRate               *float64        `json:"heart_rate,omitempty"`
	TemperatureCelsius      *float64        `json:"temperature_celsius,omitempty"`
	DepthMeters             *float64        `json:"depth_meters,omitempty"`
	NextStopDepthMeters     *float64        `json:"next_stop_depth_meters,omitempty"`
	NextStopTimeSeconds     *float64        `json:"next_stop_time_seconds,omitempty"`
	TimeToSurfaceSeconds    *float64        `json:"time_to_surface_seconds,omitempty"`
	NDLTimeSeconds          *float64        `json:"ndl_time_seconds,omitempty"`
	PercentCNSLoad          *float64        `json:"percent_cns_load,omitempty"`
	PercentN2Load           *float64        `json:"percent_n2_load,omitempty"`
	PSAC                    *float64        `json:"psac,omitempty"`
	RMV                     *float64        `json:"rmv,omitempty"`
	VSAC                    *float64        `json:"vsac,omitempty"`
	AirTimeRemainingSeconds *float64        `json:"air_time_remaining_seconds,omitempty"`
	PO2                     *float64        `json:"po2,omitempty"`
	AscentRate              *float64        `json:"ascent_rate,omitempty"`
	Raw                     json.RawMessage `json:"raw"`
}

func (s *sqlStore) DiveSamples(ctx context.Context, diveID int64, after, limit int) ([]diveSampleAPIResponse, bool, error) {
	exists, err := s.diveExists(ctx, diveID)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, errDiveNotFound
	}
	query := s.bind(`SELECT sample_index, timestamp_ms, elapsed_ms, heart_rate, temperature_celsius,
		depth_meters, next_stop_depth_meters, next_stop_time_seconds, time_to_surface_seconds,
		ndl_time_seconds, percent_cns_load, percent_n2_load, psac, rmv, vsac,
		air_time_remaining_seconds, po2, ascent_rate, raw_json
		FROM dive_samples WHERE dive_id = ? AND sample_index > ? ORDER BY sample_index LIMIT ?`)
	rows, err := s.db.QueryContext(ctx, query, diveID, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list dive samples for API: %w", err)
	}
	defer rows.Close()
	items := make([]diveSampleAPIResponse, 0, limit)
	for rows.Next() {
		var item diveSampleAPIResponse
		var heartRate, temperature, depth, nextStopDepth, nextStopTime, timeToSurface sql.NullFloat64
		var ndlTime, percentCNS, percentN2, psac, rmv, vsac, airTime, po2, ascentRate sql.NullFloat64
		var raw string
		if err := rows.Scan(
			&item.Index, &item.TimestampMS, &item.ElapsedMS, &heartRate, &temperature,
			&depth, &nextStopDepth, &nextStopTime, &timeToSurface, &ndlTime,
			&percentCNS, &percentN2, &psac, &rmv, &vsac, &airTime, &po2, &ascentRate, &raw,
		); err != nil {
			return nil, false, fmt.Errorf("read dive sample for API: %w", err)
		}
		item.HeartRate = nullFloatPointer(heartRate)
		item.TemperatureCelsius = nullFloatPointer(temperature)
		item.DepthMeters = nullFloatPointer(depth)
		item.NextStopDepthMeters = nullFloatPointer(nextStopDepth)
		item.NextStopTimeSeconds = nullFloatPointer(nextStopTime)
		item.TimeToSurfaceSeconds = nullFloatPointer(timeToSurface)
		item.NDLTimeSeconds = nullFloatPointer(ndlTime)
		item.PercentCNSLoad = nullFloatPointer(percentCNS)
		item.PercentN2Load = nullFloatPointer(percentN2)
		item.PSAC = nullFloatPointer(psac)
		item.RMV = nullFloatPointer(rmv)
		item.VSAC = nullFloatPointer(vsac)
		item.AirTimeRemainingSeconds = nullFloatPointer(airTime)
		item.PO2 = nullFloatPointer(po2)
		item.AscentRate = nullFloatPointer(ascentRate)
		item.Raw = json.RawMessage(raw)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list dive samples for API: %w", err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
}

func (s *sqlStore) diveExists(ctx context.Context, id int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, s.bind(`SELECT 1 FROM dives WHERE id = ?`), id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check dive for API: %w", err)
	}
	return true, nil
}

func nullFloatPointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func nullInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}
