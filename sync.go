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

func syncDives(ctx context.Context, client diveClient, archive *Archive) (int, error) {
	activities, err := client.Activities(ctx, garmin.ListOptions{
		Limit: divePageSize,
	})
	if err != nil {
		return 0, fmt.Errorf("list dives: %w", err)
	}
	existing := make(map[int64]struct{}, len(archive.Dives))
	for _, dive := range archive.Dives {
		existing[dive.ID] = struct{}{}
	}
	pending := make([]DiveRecord, 0)
	for _, activity := range activities {
		if !isDive(activity.ActivityType) {
			continue
		}
		if _, found := existing[activity.ID]; found {
			continue
		}
		activityDetails, err := client.GetActivityDetails(ctx, activity.ID, garmin.DetailOptions{
			MaxChartSize:    maxChartSize,
			MaxPolylineSize: maxPolylineSize,
		})
		if err != nil {
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
		})
		existing[activity.ID] = struct{}{}
	}
	archive.Dives = append(archive.Dives, pending...)
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
