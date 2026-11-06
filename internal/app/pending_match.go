package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

const pendingMatchWindow = 10 * time.Minute
const pendingSettleTime = 2 * time.Second

type photoCapture struct {
	LocalTime       string `json:"local_time"`
	CapturedAt      string `json:"captured_at,omitempty"`
	Offset          string `json:"offset,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
	TimezoneAssumed bool   `json:"timezone_assumed"`
	Source          string `json:"source"`
}

func captureJSON(value any) string {
	if value == nil {
		return "{}"
	}
	data, _ := json.Marshal(value)
	return string(data)
}

type pendingCandidate struct {
	DiveID          int64  `json:"dive_id"`
	Name            string `json:"name"`
	StartTime       string `json:"start_time"`
	EndTime         string `json:"end_time"`
	Timezone        string `json:"timezone"`
	CapturedAt      string `json:"captured_at"`
	TimezoneAssumed bool   `json:"timezone_assumed"`
	AmbiguousTime   bool   `json:"ambiguous_time"`
}

type pendingDive struct {
	id         int64
	name       string
	start, end time.Time
	location   *time.Location
	timezone   string
}

func (s *sqlStore) pendingDives(ctx context.Context) ([]pendingDive, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,start_time,timezone,duration_seconds FROM dives ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dives := []pendingDive{}
	for rows.Next() {
		var id int64
		var name, raw, zone string
		var duration sql.NullFloat64
		if err := rows.Scan(&id, &name, &raw, &zone, &duration); err != nil {
			return nil, err
		}
		if !duration.Valid || duration.Float64 <= 0 || math.IsNaN(duration.Float64) || math.IsInf(duration.Float64, 0) || duration.Float64 > float64(math.MaxInt64/int64(time.Second)) {
			continue
		}
		start, location, timezone, ok := pendingDiveStart(raw, zone)
		if !ok {
			continue
		}
		dives = append(dives, pendingDive{id: id, name: name, start: start, end: start.Add(time.Duration(duration.Float64 * float64(time.Second))), location: location, timezone: timezone})
	}
	return dives, rows.Err()
}

func pendingDiveStart(raw, zone string) (time.Time, *time.Location, string, bool) {
	location, locErr := time.LoadLocation(zone)
	if zone == "" {
		locErr = fmt.Errorf("missing timezone")
	}
	if value, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		if locErr != nil {
			_, offset := value.Zone()
			zone = offsetString(offset)
			location = time.FixedZone(zone, offset)
		}
		return value.In(location), location, zone, true
	}
	if locErr != nil {
		return time.Time{}, nil, "", false
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
		wall, err := time.Parse(layout, raw)
		if err != nil {
			continue
		}
		values := localClockInstants(wall.Format("2006-01-02T15:04:05.999999999"), location)
		if len(values) == 1 {
			return values[0], location, zone, true
		}
	}
	return time.Time{}, nil, "", false
}

func offsetString(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	return fmt.Sprintf("%s%02d:%02d", sign, seconds/3600, seconds%3600/60)
}

// Enumerate both sides of nearby timezone transitions instead of allowing
// time.ParseInLocation to silently choose one side of a repeated clock hour.
func localClockInstants(wall string, location *time.Location) []time.Time {
	nominal, err := time.Parse("2006-01-02T15:04:05.999999999", wall)
	if err != nil {
		return nil
	}
	guess := time.Date(nominal.Year(), nominal.Month(), nominal.Day(), nominal.Hour(), nominal.Minute(), nominal.Second(), nominal.Nanosecond(), location)
	offsets := map[int]bool{}
	for _, delta := range []time.Duration{-48 * time.Hour, -24 * time.Hour, 0, 24 * time.Hour, 48 * time.Hour} {
		_, offset := guess.Add(delta).Zone()
		offsets[offset] = true
	}
	values := []time.Time{}
	for offset := range offsets {
		value := nominal.Add(-time.Duration(offset) * time.Second)
		if value.In(location).Format("2006-01-02T15:04:05.999999999") == wall {
			values = append(values, value.In(location))
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Before(values[j]) })
	return values
}

func captureForDive(capture *mediaproc.CaptureTime, dive pendingDive) (*photoCapture, []time.Time) {
	if capture == nil {
		return nil, nil
	}
	result := &photoCapture{LocalTime: capture.LocalTime, Offset: capture.Offset, Source: capture.Source}
	if capture.Offset != "" {
		value, err := time.Parse(time.RFC3339Nano, capture.LocalTime+capture.Offset)
		if err != nil {
			return result, nil
		}
		result.CapturedAt, result.Timezone = value.UTC().Format(time.RFC3339Nano), capture.Offset
		return result, []time.Time{value}
	}
	result.Timezone, result.TimezoneAssumed = dive.timezone, true
	if dive.location == nil {
		return result, nil
	}
	values := localClockInstants(capture.LocalTime, dive.location)
	if len(values) == 1 {
		result.CapturedAt = values[0].UTC().Format(time.RFC3339Nano)
	}
	return result, values
}

func matchPendingCapture(capture *mediaproc.CaptureTime, dives []pendingDive) ([]pendingCandidate, string) {
	candidates := []pendingCandidate{}
	if capture == nil {
		return candidates, "missing_capture_time"
	}
	for _, dive := range dives {
		resolved, values := captureForDive(capture, dive)
		for _, value := range values {
			if value.Before(dive.start.Add(-pendingMatchWindow)) || value.After(dive.end.Add(pendingMatchWindow)) {
				continue
			}
			candidates = append(candidates, pendingCandidate{DiveID: dive.id, Name: dive.name, StartTime: dive.start.Format(time.RFC3339Nano), EndTime: dive.end.Format(time.RFC3339Nano), Timezone: dive.timezone, CapturedAt: value.UTC().Format(time.RFC3339Nano), TimezoneAssumed: resolved.TimezoneAssumed, AmbiguousTime: len(values) > 1})
			break
		}
	}
	if len(candidates) == 0 {
		return candidates, "no_matching_dive"
	}
	if len(candidates) > 1 {
		return candidates, "multiple_matches"
	}
	if candidates[0].AmbiguousTime {
		return candidates, "ambiguous_time"
	}
	return candidates, "matched"
}
