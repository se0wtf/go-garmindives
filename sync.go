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
