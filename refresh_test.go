package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	garmin "github.com/se0wtf/go-garminconnect"
)

type refreshDiveClient struct {
	*fitDiveClient
	activity      json.RawMessage
	activityErr   error
	activityCalls []int64
	listCalls     int
}

func (c *refreshDiveClient) Activities(context.Context, garmin.ListOptions) ([]garmin.Activity, error) {
	c.listCalls++
	return nil, errors.New("single-dive refresh must not list activity history")
}

func (c *refreshDiveClient) GetActivity(_ context.Context, id int64) (json.RawMessage, error) {
	c.activityCalls = append(c.activityCalls, id)
	return c.activity, c.activityErr
}

func newRefreshTestClient(t *testing.T, id int64, note string) *refreshDiveClient {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"activityId": id, "activityName": "Updated dive", "description": note,
		"activityTypeDTO": map[string]string{"typeKey": "single_gas_diving"},
		"summaryDTO":      map[string]string{"startTimeLocal": "2026-10-20T10:00:00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &refreshDiveClient{fitDiveClient: &fitDiveClient{fakeDiveClient: &fakeDiveClient{
		diveDetails: map[int64]json.RawMessage{id: raw},
	}, files: map[int64][]byte{id: testFIT(t)}}}
}

func TestSyncDiveRefreshesOnlySelectedActivity(t *testing.T) {
	const id int64 = 24424941853
	for _, personal := range []bool{true, false} {
		t.Run(map[bool]string{true: "personal", false: "legacy"}[personal], func(t *testing.T) {
			client := newRefreshTestClient(t, id, exampleDiveNote)
			if !personal {
				client.diveDetails[id] = json.RawMessage(`{"connectId":24424941853,"name":"Updated dive","notes":"N:4/5\nCommentaire: Beau récif\nHL: Raies aigles"}`)
				client.activity = json.RawMessage(`{"activityId":24424941853,"activityName":"Updated dive","activityTypeDTO":{"typeKey":"single_gas_diving"}}`)
			}
			archive := newArchive()
			other := DiveRecord{ID: 10, Activity: map[string]any{"description": "Untouched"}}
			old := DiveRecord{ID: id, fitComplete: true, ActivityDetails: map[string]any{"profile": "cached"}}
			archive.Dives = []DiveRecord{old, other}
			var progress [][2]int
			changed, err := syncDive(context.Background(), client, &archive, id, func(done, total int) {
				progress = append(progress, [2]int{done, total})
			})
			if err != nil || changed != 1 || len(archive.Dives) != 2 {
				t.Fatalf("refresh: changed=%d err=%v", changed, err)
			}
			updated := archive.Dives[1]
			if !reflect.DeepEqual(archive.Dives[0], other) || updated.ID != id || !updated.refreshMetadata || !updated.fitComplete || !reflect.DeepEqual(updated.ActivityDetails, old.ActivityDetails) {
				t.Fatalf("refresh changed unrelated/cached state: %+v", archive.Dives)
			}
			if client.listCalls != 0 || len(client.calls) != 0 || len(client.activityDetailCalls) != 0 || !reflect.DeepEqual(client.diveDetailCalls, []int64{id}) {
				t.Fatalf("unexpected requests: list=%d FIT=%v details=%v dive=%v", client.listCalls, client.calls, client.activityDetailCalls, client.diveDetailCalls)
			}
			if (personal && len(client.activityCalls) != 0) || (!personal && !reflect.DeepEqual(client.activityCalls, []int64{id})) {
				t.Fatalf("incorrect activity requests: %v", client.activityCalls)
			}
			if !reflect.DeepEqual(progress, [][2]int{{1, 1}}) {
				t.Fatalf("progress=%v", progress)
			}
		})
	}
}

func TestSyncDiveErrorsLeaveArchiveUnchanged(t *testing.T) {
	wantErr := errors.New("Garmin unavailable")
	for _, tc := range []struct {
		name string
		edit func(*refreshDiveClient)
	}{
		{"dive request", func(c *refreshDiveClient) { c.diveDetailsErr = map[int64]error{42: wantErr} }},
		{"malformed JSON", func(c *refreshDiveClient) { c.diveDetails[42] = json.RawMessage(`{`) }},
		{"missing document", func(c *refreshDiveClient) { c.diveDetails[42] = json.RawMessage(`null`) }},
		{"wrong ID", func(c *refreshDiveClient) { c.diveDetails[42] = json.RawMessage(`{"activityId":43}`) }},
		{"not a dive", func(c *refreshDiveClient) {
			c.diveDetails[42] = json.RawMessage(`{"activityId":42,"activityTypeDTO":{"typeKey":"running"}}`)
		}},
		{"activity request", func(c *refreshDiveClient) {
			c.diveDetails[42] = json.RawMessage(`{"connectId":42}`)
			c.activityErr = wantErr
		}},
		{"FIT download", func(c *refreshDiveClient) { c.err = wantErr }},
		{"invalid FIT", func(c *refreshDiveClient) { c.files[42] = []byte("invalid FIT") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newRefreshTestClient(t, 42, exampleDiveNote)
			tc.edit(client)
			archive := newArchive()
			archive.Dives = []DiveRecord{{ID: 42}, {ID: 43}}
			before := append([]DiveRecord{}, archive.Dives...)
			changed, err := syncDive(context.Background(), client, &archive, 42, nil)
			if err == nil || changed != 0 || !reflect.DeepEqual(before, archive.Dives) || client.listCalls != 0 {
				t.Fatalf("failed refresh changed state: changed=%d err=%v dives=%+v", changed, err, archive.Dives)
			}
		})
	}
}

func TestSyncDiveImportsMissingActivityAndBackfillsFIT(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "new dive", true: "missing FIT"}[exists], func(t *testing.T) {
			client := newRefreshTestClient(t, 42, exampleDiveNote)
			archive := newArchive()
			if exists {
				archive.Dives = []DiveRecord{{ID: 42}}
			}
			changed, err := syncDive(context.Background(), client, &archive, 42, nil)
			if err != nil || changed != 1 || len(archive.Dives) != 1 || archive.Dives[0].FIT == nil || !reflect.DeepEqual(client.calls, []int64{42}) || client.listCalls != 0 {
				t.Fatalf("targeted import: changed=%d err=%v FIT requests=%v", changed, err, client.calls)
			}
		})
	}
}

func TestRunRefreshKeepsFITMediaAndOtherDives(t *testing.T) {
	const id int64 = 24424941853
	library, _ := mediaTestLibrary(t)
	store := library.store
	record := testSQLDive(t, id)
	fit, err := decodeFITDownload(context.Background(), testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	record.FIT = fit
	if err := store.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
	photo, err := library.Upload(context.Background(), id, "Requin.jpg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	description := "Un requin près du récif"
	if _, err := library.Patch(context.Background(), id, photo.ID, mediaPatch{Description: &description}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.setDiveCover(context.Background(), id, &photo.ID); err != nil {
		t.Fatal(err)
	}
	before, err := store.Dive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Dive(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	samples, _, err := store.DiveSamples(context.Background(), id, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	files, err := store.FITFiles(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	client := newRefreshTestClient(t, id, exampleDiveNote)
	path := store.path
	err = runWithDependencies(context.Background(), []string{"-output-type", "sqlite", "-activity-id", "24424941853"}, io.Discard, testLogger(t), appDependencies{
		configDirectory: func() (string, error) { return t.TempDir(), nil },
		openDestination: func(context.Context, outputConfig) (diveDestination, error) {
			return &sqlDestination{store: store}, nil
		},
		loadCached: func(string) (diveClient, error) { return client, nil },
		loginFresh: func(context.Context, string) (diveClient, error) { return nil, errors.New("unexpected login") },
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := openSQLStore(context.Background(), outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.Dive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Dive.Rating == nil || *after.Dive.Rating != 4 || after.Dive.RatingMax == nil || *after.Dive.RatingMax != 5 || after.Dive.Comment != exampleDiveComment || len(after.Dive.Highlights) != 3 || after.Dive.Description != exampleDiveNote || after.Dive.Name != "Updated dive" {
		t.Fatalf("fresh metadata was not parsed: %+v", after.Dive)
	}
	if !reflect.DeepEqual(before.FIT, after.FIT) || !reflect.DeepEqual(before.ActivityDetails, after.ActivityDetails) || !reflect.DeepEqual(before.Media, after.Media) || !reflect.DeepEqual(before.Dive.CoverPhoto, after.Dive.CoverPhoto) || !reflect.DeepEqual(before.Dive.MaxDepthMeters, after.Dive.MaxDepthMeters) {
		t.Fatal("refresh replaced cached FIT, profile documents, measurements, media, or cover")
	}
	newSamples, _, err := reopened.DiveSamples(context.Background(), id, -1, 100)
	if err != nil || !reflect.DeepEqual(samples, newSamples) {
		t.Fatalf("refresh changed profile samples: %v", err)
	}
	newFiles, err := reopened.FITFiles(context.Background(), id)
	if err != nil || !reflect.DeepEqual(files, newFiles) {
		t.Fatalf("refresh changed original FIT bytes: %v", err)
	}
	newOther, err := reopened.Dive(context.Background(), 42)
	if err != nil || !reflect.DeepEqual(other, newOther) || client.listCalls != 0 || len(client.calls) != 0 {
		t.Fatalf("refresh changed another dive or redownloaded FIT: %v", err)
	}
}

func TestRunRefreshRejectsInvalidSelectionBeforeOpeningOutput(t *testing.T) {
	for _, args := range [][]string{{"-activity-id", "0"}, {"-activity-id", "-1"}, {"-activity-id", "oops"}, {"24424941853"}} {
		err := runWithDependencies(context.Background(), args, io.Discard, testLogger(t), appDependencies{
			configDirectory: func() (string, error) { return "/config", nil },
			openDestination: func(context.Context, outputConfig) (diveDestination, error) {
				t.Fatal("invalid selection opened the output")
				return nil, nil
			},
		})
		if err == nil {
			t.Fatalf("invalid selection accepted: %v", args)
		}
	}
}
