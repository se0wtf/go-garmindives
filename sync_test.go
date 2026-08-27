package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	garmin "github.com/se0wtf/go-garminconnect"
)

type fakeDiveClient struct {
	activities          []garmin.Activity
	activitiesErr       error
	activityDetails     map[int64]json.RawMessage
	diveDetails         map[int64]json.RawMessage
	activityDetailsErr  map[int64]error
	diveDetailsErr      map[int64]error
	activityDetailCalls []int64
	diveDetailCalls     []int64
	listOptions         garmin.ListOptions
}

func (f *fakeDiveClient) Activities(_ context.Context, options garmin.ListOptions) ([]garmin.Activity, error) {
	f.listOptions = options
	return f.activities, f.activitiesErr
}

func (f *fakeDiveClient) GetActivityDetails(_ context.Context, id int64, options garmin.DetailOptions) (json.RawMessage, error) {
	if options.MaxChartSize != maxChartSize || options.MaxPolylineSize != maxPolylineSize {
		return nil, errors.New("incorrect detail options")
	}
	f.activityDetailCalls = append(f.activityDetailCalls, id)
	if err := f.activityDetailsErr[id]; err != nil {
		return nil, err
	}
	return f.activityDetails[id], nil
}

func (f *fakeDiveClient) GetDiveDetails(_ context.Context, id int64) (json.RawMessage, error) {
	f.diveDetailCalls = append(f.diveDetailCalls, id)
	if err := f.diveDetailsErr[id]; err != nil {
		return nil, err
	}
	return f.diveDetails[id], nil
}

func TestSyncDivesAddsOnlyMissingDives(t *testing.T) {
	client := &fakeDiveClient{
		activities: []garmin.Activity{
			{ID: 1, ActivityType: garmin.ActivityType{Key: "gauge_diving"}},
			{ID: 2, Name: "New dive", ActivityType: garmin.ActivityType{Key: "single_gas_diving"}},
			{ID: 3, ActivityType: garmin.ActivityType{Key: "running", ParentID: 1}},
		},
		activityDetails:    map[int64]json.RawMessage{2: json.RawMessage(`{"activityId":2}`)},
		diveDetails:        map[int64]json.RawMessage{2: json.RawMessage(`{"connectId":2,"maxDepth":18.2}`)},
		activityDetailsErr: map[int64]error{},
		diveDetailsErr:     map[int64]error{},
	}
	archive := newArchive()
	archive.Dives = append(archive.Dives, DiveRecord{ID: 1})
	added, err := syncDives(context.Background(), client, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || len(archive.Dives) != 2 || archive.Dives[1].ID != 2 {
		t.Fatalf("added=%d archive=%#v", added, archive)
	}
	if client.listOptions.ActivityType != "" || client.listOptions.Limit != divePageSize {
		t.Fatalf("list options = %#v", client.listOptions)
	}
	if !reflect.DeepEqual(client.activityDetailCalls, []int64{2}) || !reflect.DeepEqual(client.diveDetailCalls, []int64{2}) {
		t.Fatalf("detail calls = %v / %v", client.activityDetailCalls, client.diveDetailCalls)
	}
	added, err = syncDives(context.Background(), client, &archive)
	if err != nil || added != 0 || len(archive.Dives) != 2 {
		t.Fatalf("second sync: added=%d archive=%#v err=%v", added, archive, err)
	}
}

func TestSyncDivesIsTransactional(t *testing.T) {
	wantErr := errors.New("detail unavailable")
	client := &fakeDiveClient{
		activities: []garmin.Activity{
			{ID: 10, ActivityType: garmin.ActivityType{Key: "apnea_diving"}},
			{ID: 11, ActivityType: garmin.ActivityType{Key: "gauge_diving"}},
		},
		activityDetails: map[int64]json.RawMessage{
			10: json.RawMessage(`{"activityId":10}`),
			11: json.RawMessage(`{"activityId":11}`),
		},
		diveDetails:        map[int64]json.RawMessage{10: json.RawMessage(`{"connectId":10}`)},
		activityDetailsErr: map[int64]error{},
		diveDetailsErr:     map[int64]error{11: wantErr},
	}
	archive := newArchive()
	added, err := syncDives(context.Background(), client, &archive)
	if added != 0 || !errors.Is(err, wantErr) || len(archive.Dives) != 0 {
		t.Fatalf("added=%d archive=%#v err=%v", added, archive, err)
	}
}

func TestSyncDivesErrors(t *testing.T) {
	wantErr := errors.New("list failed")
	client := &fakeDiveClient{activitiesErr: wantErr}
	archive := newArchive()
	if _, err := syncDives(context.Background(), client, &archive); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}

	client = &fakeDiveClient{
		activities:         []garmin.Activity{{ID: 1, ActivityType: garmin.ActivityType{Key: "gauge_diving"}}},
		activityDetails:    map[int64]json.RawMessage{1: json.RawMessage(`not-json`)},
		diveDetails:        map[int64]json.RawMessage{1: json.RawMessage(`{}`)},
		activityDetailsErr: map[int64]error{},
		diveDetailsErr:     map[int64]error{},
	}
	if _, err := syncDives(context.Background(), client, &archive); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestIsDive(t *testing.T) {
	tests := []struct {
		activityType garmin.ActivityType
		want         bool
	}{
		{garmin.ActivityType{Key: "multi_gas_diving"}, true},
		{garmin.ActivityType{Key: "APNEA_HUNTING"}, true},
		{garmin.ActivityType{Key: "ccr_diving"}, true},
		{garmin.ActivityType{Key: "resort_diving"}, true},
		{garmin.ActivityType{Key: "generic_diving"}, false},
		{garmin.ActivityType{Key: "running"}, false},
	}
	for _, test := range tests {
		if got := isDive(test.activityType); got != test.want {
			t.Errorf("isDive(%#v) = %v, want %v", test.activityType, got, test.want)
		}
	}
}
