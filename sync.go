package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	garmin "github.com/se0wtf/go-garminconnect"
)

const (
	divePageSize    = 100
	maxChartSize    = 2000
	maxPolylineSize = 4000
)

var diveActivityTypes = map[string]struct{}{
	"apnea_diving":      {},
	"apnea_hunting":     {},
	"ccr_diving":        {},
	"gauge_diving":      {},
	"multi_gas_diving":  {},
	"resort_diving":     {},
	"single_gas_diving": {},
}

type diveClient interface {
	Activities(context.Context, garmin.ListOptions) ([]garmin.Activity, error)
	GetActivityDetails(context.Context, int64, garmin.DetailOptions) (json.RawMessage, error)
	GetDiveDetails(context.Context, int64) (json.RawMessage, error)
}

type activityDownloader interface {
	DownloadActivity(context.Context, int64, garmin.DownloadFormat) ([]byte, error)
}

type activityGetter interface {
	GetActivity(context.Context, int64) (json.RawMessage, error)
}

// syncDive refreshes one explicitly selected activity without listing history.
// Cached FIT data is retained; only an absent FIT download is backfilled.
func syncDive(ctx context.Context, client diveClient, archive *Archive, id int64, reportProgress func(int, int)) (int, error) {
	if id <= 0 {
		return 0, errors.New("activity ID must be positive")
	}
	var record DiveRecord
	found := false
	for _, stored := range archive.Dives {
		if stored.ID == id {
			record, found = stored, true
			break
		}
	}
	rawDive, err := client.GetDiveDetails(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("get Garmin Dive details for dive %d: %w", id, err)
	}
	var personal personalDiveDocument
	if err := json.Unmarshal(rawDive, &personal); err != nil {
		return 0, fmt.Errorf("decode Garmin Dive details for dive %d: %w", id, err)
	}
	// The Personal API's dive response is already the full activity document.
	// Reuse it instead of requesting the same endpoint a second time.
	rawActivity := rawDive
	if personal.ID == 0 {
		getter, ok := client.(activityGetter)
		if !ok {
			return 0, errors.New("Garmin client does not support refreshing a single activity")
		}
		rawActivity, err = getter.GetActivity(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("get activity %d: %w", id, err)
		}
	}
	var activity struct {
		ID           int64               `json:"activityId"`
		Type         garmin.ActivityType `json:"activityType"`
		PersonalType garmin.ActivityType `json:"activityTypeDTO"`
	}
	if err := json.Unmarshal(rawActivity, &activity); err != nil {
		return 0, fmt.Errorf("decode activity %d: %w", id, err)
	}
	if activity.ID != id {
		return 0, fmt.Errorf("Garmin returned a different activity for dive %d", id)
	}
	if !isDive(activity.Type) && !isDive(activity.PersonalType) {
		return 0, fmt.Errorf("activity %d is not a supported dive", id)
	}
	activityDocument, err := rawDocument(rawActivity)
	if err != nil {
		return 0, fmt.Errorf("decode activity %d: %w", id, err)
	}
	diveDocument, err := rawDocument(rawDive)
	if err != nil || diveDocument == nil {
		return 0, fmt.Errorf("invalid Garmin Dive document for dive %d", id)
	}
	if !found && personal.ID == 0 {
		rawDetails, err := client.GetActivityDetails(ctx, id, garmin.DetailOptions{
			MaxChartSize: maxChartSize, MaxPolylineSize: maxPolylineSize,
		})
		if err != nil {
			return 0, fmt.Errorf("get activity details for dive %d: %w", id, err)
		}
		record.ActivityDetails, err = rawDocument(rawDetails)
		if err != nil {
			return 0, fmt.Errorf("decode activity details for dive %d: %w", id, err)
		}
	}
	if record.FIT == nil && !record.fitComplete {
		if downloader, ok := client.(activityDownloader); ok {
			body, err := downloader.DownloadActivity(ctx, id, garmin.Original)
			if err != nil {
				return 0, fmt.Errorf("download original FIT for dive %d: %w", id, err)
			}
			record.FIT, err = decodeFITDownload(ctx, body)
			if err != nil {
				return 0, fmt.Errorf("decode original FIT for dive %d: %w", id, err)
			}
		}
	}
	record.ID, record.Activity, record.DiveDetails = id, activityDocument, diveDocument
	record.refreshMetadata = true
	// Mutate the archive only after all requests and decoding have succeeded.
	updated := make([]DiveRecord, 0, len(archive.Dives)+1)
	for _, stored := range archive.Dives {
		if stored.ID != id {
			updated = append(updated, stored)
		}
	}
	archive.Dives = append(updated, record)
	if reportProgress != nil {
		reportProgress(1, 1)
	}
	return 1, nil
}

func syncDives(ctx context.Context, client diveClient, archive *Archive, reportProgress func(int, int)) (int, error) {
	activities, err := client.Activities(ctx, garmin.ListOptions{
		Limit: divePageSize,
	})
	if err != nil {
		return 0, fmt.Errorf("list dives: %w", err)
	}
	downloader, canDownload := client.(activityDownloader)
	existing := make(map[int64]DiveRecord, len(archive.Dives))
	for _, dive := range archive.Dives {
		existing[dive.ID] = dive
	}
	missing := make([]garmin.Activity, 0)
	seen := make(map[int64]bool)
	for _, activity := range activities {
		if !isDive(activity.ActivityType) {
			continue
		}
		if seen[activity.ID] {
			continue
		}
		seen[activity.ID] = true
		if stored, found := existing[activity.ID]; found && (!canDownload || stored.FIT != nil || stored.fitComplete) {
			continue
		}
		missing = append(missing, activity)
	}
	pending := make([]DiveRecord, 0, len(missing))
	for _, activity := range missing {
		var fit *fitDocument
		if canDownload {
			body, err := downloader.DownloadActivity(ctx, activity.ID, garmin.Original)
			if err != nil {
				return 0, fmt.Errorf("download original FIT for dive %d: %w", activity.ID, err)
			}
			fit, err = decodeFITDownload(ctx, body)
			if err != nil {
				return 0, fmt.Errorf("decode original FIT for dive %d: %w", activity.ID, err)
			}
		}
		if stored, found := existing[activity.ID]; found {
			stored.FIT = fit
			pending = append(pending, stored)
			if reportProgress != nil {
				reportProgress(len(pending), len(missing))
			}
			continue
		}
		activityDetails, err := client.GetActivityDetails(ctx, activity.ID, garmin.DetailOptions{
			MaxChartSize:    maxChartSize,
			MaxPolylineSize: maxPolylineSize,
		})
		if errors.Is(err, garmin.ErrUnsupportedOperation) {
			// The Personal API supplies profile samples through the FIT file.
			if !canDownload {
				return 0, fmt.Errorf("get profile for dive %d: %w", activity.ID, err)
			}
			activityDetails = json.RawMessage("null")
		} else if err != nil {
			return 0, fmt.Errorf("get activity details for dive %d: %w", activity.ID, err)
		}
		diveDetails, err := client.GetDiveDetails(ctx, activity.ID)
		if err != nil {
			return 0, fmt.Errorf("get Garmin Dive details for dive %d: %w", activity.ID, err)
		}
		summaryDocument, err := document(activity)
		if err != nil {
			return 0, fmt.Errorf("prepare activity %d: %w", activity.ID, err)
		}
		activityDocument, err := rawDocument(activityDetails)
		if err != nil {
			return 0, fmt.Errorf("decode activity details for dive %d: %w", activity.ID, err)
		}
		diveDocument, err := rawDocument(diveDetails)
		if err != nil {
			return 0, fmt.Errorf("decode Garmin Dive details for dive %d: %w", activity.ID, err)
		}
		pending = append(pending, DiveRecord{
			ID:              activity.ID,
			Activity:        summaryDocument,
			ActivityDetails: activityDocument,
			DiveDetails:     diveDocument,
			FIT:             fit,
		})
		if reportProgress != nil {
			reportProgress(len(pending), len(missing))
		}
	}
	// Commit changes only after every download and decode succeeds. Updated
	// records occupy the tail, which SQL destinations save in one transaction.
	updated := make(map[int64]bool, len(pending))
	for _, record := range pending {
		updated[record.ID] = true
	}
	unchanged := make([]DiveRecord, 0, len(archive.Dives)+len(pending))
	for _, record := range archive.Dives {
		if !updated[record.ID] {
			unchanged = append(unchanged, record)
		}
	}
	archive.Dives = append(unchanged, pending...)
	return len(pending), nil
}

func isDive(activityType garmin.ActivityType) bool {
	_, found := diveActivityTypes[strings.ToLower(activityType.Key)]
	return found
}

func document(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return rawDocument(raw)
}

func rawDocument(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON documents")
		}
		return nil, err
	}
	return value, nil
}
