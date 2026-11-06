package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	garmin "github.com/se0wtf/go-garminconnect"
	fitdata "github.com/se0wtf/go-garmindives/internal/fit"
)

type fakeResyncClient struct {
	get                        func(context.Context, int64) (json.RawMessage, error)
	activity                   func(context.Context, int64) (json.RawMessage, error)
	mu                         sync.Mutex
	ids                        []int64
	downloads, lists, profiles atomic.Int32
}

func (c *fakeResyncClient) GetDiveDetails(ctx context.Context, id int64) (json.RawMessage, error) {
	c.mu.Lock()
	c.ids = append(c.ids, id)
	c.mu.Unlock()
	return c.get(ctx, id)
}
func (c *fakeResyncClient) GetActivity(ctx context.Context, id int64) (json.RawMessage, error) {
	if c.activity != nil {
		return c.activity(ctx, id)
	}
	return nil, errors.New("unexpected activity GET")
}
func (c *fakeResyncClient) DownloadActivity(context.Context, int64, garmin.DownloadFormat) ([]byte, error) {
	c.downloads.Add(1)
	return nil, errors.New("FIT download forbidden")
}
func (c *fakeResyncClient) Activities(context.Context, garmin.ListOptions) ([]garmin.Activity, error) {
	c.lists.Add(1)
	return nil, errors.New("history listing forbidden")
}
func (c *fakeResyncClient) GetActivityDetails(context.Context, int64, garmin.DetailOptions) (json.RawMessage, error) {
	c.profiles.Add(1)
	return nil, errors.New("profile download forbidden")
}
func (c *fakeResyncClient) requested() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64{}, c.ids...)
}
func resyncDocument(id int64, note string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"activityId": id, "activityName": "Updated dive", "description": note, "activityTypeDTO": map[string]string{"typeKey": "single_gas_diving"}, "timeZoneUnitDTO": map[string]string{"unitKey": "Pacific/Noumea"}, "summaryDTO": map[string]string{"startTimeLocal": "2026-08-15T10:00:00"}})
	return data
}
func resyncTestService(t *testing.T, store *sqlStore, client resyncClient) *resyncService {
	t.Helper()
	s := newResyncServiceWithFactory(store, func(context.Context) (resyncClient, error) { return client, nil }, testLogger(t))
	store.resync = s
	t.Cleanup(s.Close)
	return s
}
func waitResync(t *testing.T, s *resyncService, id, status string) resyncJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := s.store.resyncJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == status {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := s.store.resyncJob(context.Background(), id)
	t.Fatalf("job never reached %s: %+v", status, job)
	return job
}

func TestResyncMetadataPreservesFITProfileAndLocalMedia(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	store := l.store
	ctx := context.Background()
	record := testSQLDive(t, 42)
	fit, err := fitdata.DecodeDownload(ctx, testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	record.FIT = fit
	if err := store.SaveDives(ctx, []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
	photo, err := l.Upload(ctx, 42, "Reef.jpg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.setDiveCover(ctx, 42, &photo.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{42, 99} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			before, err := store.Dive(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			fitBefore := storedFITJSON(t, store, id)
			filesBefore, _ := store.FITFiles(ctx, id)
			samplesBefore, _, _ := store.DiveSamples(ctx, id, -1, 100)
			client := &fakeResyncClient{get: func(_ context.Context, id int64) (json.RawMessage, error) {
				return resyncDocument(id, exampleDiveNote), nil
			}}
			s := resyncTestService(t, store, client)
			job, err := s.Enqueue(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.runNext(ctx); err != nil {
				t.Fatal(err)
			}
			job, _ = store.resyncJob(ctx, job.ID)
			if job.Status != "succeeded" || job.Total != 1 || job.Succeeded != 1 || job.Completed != 1 || job.ProgressPercent != 100 || job.Remaining != 0 {
				t.Fatalf("resync result: %+v", job)
			}
			after, err := store.Dive(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			filesAfter, _ := store.FITFiles(ctx, id)
			samplesAfter, _, _ := store.DiveSamples(ctx, id, -1, 100)
			if after.Dive.Name != "Updated dive" || after.Dive.Rating == nil || *after.Dive.Rating != 4 || after.Dive.Comment != exampleDiveComment {
				t.Fatalf("metadata not refreshed: %+v", after.Dive)
			}
			if !reflect.DeepEqual(fitBefore, storedFITJSON(t, store, id)) || !reflect.DeepEqual(filesBefore, filesAfter) || !reflect.DeepEqual(samplesBefore, samplesAfter) || !reflect.DeepEqual(before.ActivityDetails, after.ActivityDetails) || !reflect.DeepEqual(before.RecordMetadata, after.RecordMetadata) || !reflect.DeepEqual(before.Alerts, after.Alerts) || !reflect.DeepEqual(before.Media, after.Media) || !reflect.DeepEqual(before.Dive.CoverPhoto, after.Dive.CoverPhoto) {
				t.Fatal("resync changed cached FIT/profile or local media")
			}
			if client.downloads.Load() != 0 || client.lists.Load() != 0 || client.profiles.Load() != 0 {
				t.Fatal("data-only resync requested FIT, history or profile data")
			}
			// A second refresh can clear the parsed rating/comment fields as well.
			client.get = func(_ context.Context, id int64) (json.RawMessage, error) {
				return resyncDocument(id, "Simple note"), nil
			}
			if _, err := s.Enqueue(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err := s.runNext(ctx); err != nil {
				t.Fatal(err)
			}
			cleared, _ := store.Dive(ctx, id)
			if cleared.Dive.Rating != nil || cleared.Dive.Comment != "" || len(cleared.Dive.Highlights) != 0 {
				t.Fatal("cleared Garmin note fields were kept")
			}
		})
	}
}

func TestResyncBulkSnapshotPartialFailureAndErrors(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprint(fatal), func(t *testing.T) {
			l, _ := mediaTestLibrary(t)
			ctx := context.Background()
			client := &fakeResyncClient{get: func(_ context.Context, id int64) (json.RawMessage, error) {
				if fatal {
					return nil, &garmin.HTTPError{StatusCode: 429, Body: "secret response must not be exposed"}
				}
				if id == 99 {
					return nil, &garmin.HTTPError{StatusCode: 404, Body: "secret response must not be exposed"}
				}
				return resyncDocument(id, exampleDiveNote), nil
			}}
			s := resyncTestService(t, l.store, client)
			job, err := s.Enqueue(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.store.SaveDives(ctx, []DiveRecord{testSQLDive(t, 100)}); err != nil {
				t.Fatal(err)
			}
			if err := s.runNext(ctx); err != nil {
				t.Fatal(err)
			}
			job, _ = l.store.resyncJob(ctx, job.ID)
			if job.Total != 2 || job.Failed != 1 || job.Remaining != 0 {
				t.Fatalf("bulk counters: %+v", job)
			}
			if fatal {
				if job.Status != "failed" || job.Skipped != 1 || job.ErrorCode != "garmin_rate_limited" || !reflect.DeepEqual(client.requested(), []int64{42}) {
					t.Fatalf("rate limit must stop bulk: %+v", job)
				}
			} else if job.Status != "partial" || job.Succeeded != 1 || !reflect.DeepEqual(client.requested(), []int64{42, 99}) {
				t.Fatalf("partial bulk: %+v", job)
			}
			items := travelRequest[resyncItemList](t, newAPIHandler(l.store, testLogger(t)), "GET", job.ItemsURL+"?status=failed", nil, 200)
			data, _ := json.Marshal(items)
			if len(items.Items) != 1 || bytes.Contains(data, []byte("secret")) {
				t.Fatalf("unsafe/incomplete error result: %s", data)
			}
		})
	}
}

func TestResyncCancellationShutdownAndRestart(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			l, _ := mediaTestLibrary(t)
			entered := make(chan struct{})
			client := &fakeResyncClient{get: func(ctx context.Context, id int64) (json.RawMessage, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			s := resyncTestService(t, l.store, client)
			job, err := s.Enqueue(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			s.Start(context.Background())
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not start")
			}
			if _, err := s.Enqueue(context.Background(), 42); !errors.Is(err, errResyncBusy) {
				t.Fatalf("concurrent job accepted: %v", err)
			}
			// A blocked Garmin request must not hold SQL read/write connections.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := l.store.Dive(ctx, 42); err != nil {
				t.Fatalf("resync blocked public reads: %v", err)
			}
			status := "canceled"
			if shutdown {
				s.Close()
				status = "interrupted"
			} else {
				if _, err := s.Cancel(context.Background(), job.ID); err != nil {
					t.Fatal(err)
				}
			}
			job = waitResync(t, s, job.ID, status)
			if job.Completed != 0 || job.Skipped != 2 || job.CompletedAt == "" {
				t.Fatalf("cancel/shutdown result: %+v", job)
			}
		})
	}
	l, _ := mediaTestLibrary(t)
	calls := 0
	s := resyncTestService(t, l.store, &fakeResyncClient{get: func(_ context.Context, id int64) (json.RawMessage, error) {
		calls++
		return resyncDocument(id, ""), nil
	}})
	job, err := s.Enqueue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.store.db.Exec(`UPDATE resync_jobs SET status='running',phase='fetching' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.store.db.Exec(`UPDATE resync_items SET status='succeeded' WHERE job_id=? AND activity_id=42`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, _ = l.store.resyncJob(context.Background(), job.ID)
	if job.Status != "interrupted" || job.Succeeded != 1 || job.Skipped != 1 || calls != 0 {
		t.Fatalf("restart silently resumed/lost progress: %+v", job)
	}
	if _, err := s.Enqueue(context.Background(), 42); err != nil {
		t.Fatal("interrupted job kept active slot")
	}
}

func TestResyncConcurrentEnqueuesAndEmptyJob(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	s := resyncTestService(t, l.store, &fakeResyncClient{get: func(_ context.Context, id int64) (json.RawMessage, error) { return resyncDocument(id, ""), nil }})
	var accepted, busy atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Enqueue(context.Background(), 42)
			if err == nil {
				accepted.Add(1)
			} else if errors.Is(err, errResyncBusy) {
				busy.Add(1)
			} else {
				t.Errorf("enqueue: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || busy.Load() != 7 {
		t.Fatalf("accepted=%d busy=%d", accepted.Load(), busy.Load())
	}
	store, _ := openTestSQLStore(t)
	empty := resyncTestService(t, store, nil)
	job, err := empty.Enqueue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.runNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, _ = store.resyncJob(context.Background(), job.ID)
	if job.Status != "succeeded" || job.Total != 0 || job.ProgressPercent != 100 {
		t.Fatalf("empty job: %+v", job)
	}
}

func TestResyncPersonalClientUsesPacingAndNeverDownloadsFIT(t *testing.T) {
	const key = "synthetic-resync-key"
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
	var tokens, activities, unexpected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/personalapitoken-service/personal-api/personal-api-key/token" {
			tokens.Add(1)
			if r.Header.Get("Authorization") != "ApiKey "+key {
				t.Error("incorrect key exchange")
			}
			fmt.Fprint(w, token)
			return
		}
		var id int64
		if _, err := fmt.Sscanf(r.URL.Path, "/activity-service/activity/%d", &id); err == nil && (id == 42 || id == 99) {
			activities.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("incorrect bearer")
			}
			w.Write(resyncDocument(id, exampleDiveNote))
			return
		}
		unexpected.Add(1)
		http.Error(w, "unexpected endpoint", 500)
	}))
	defer server.Close()
	rate, clock := rateTestClient(t, server.Client().Transport)
	defer rate.CloseIdleConnections()
	l, _ := mediaTestLibrary(t)
	s := newResyncServiceWithFactory(l.store, func(context.Context) (resyncClient, error) {
		return garmin.NewPersonalClient(key, garmin.WithBaseURL(server.URL), garmin.WithHTTPClient(rate), garmin.WithTokenFile(filepath.Join(t.TempDir(), "tokens.json")))
	}, testLogger(t))
	defer s.Close()
	job, err := s.Enqueue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.runNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, _ = l.store.resyncJob(context.Background(), job.ID)
	if job.Status != "succeeded" || tokens.Load() != 1 || activities.Load() != 2 || unexpected.Load() != 0 || len(clock.waits) != 2 {
		t.Fatalf("requests/status: %+v token=%d activity=%d unexpected=%d waits=%v", job, tokens.Load(), activities.Load(), unexpected.Load(), clock.waits)
	}
}

func TestResyncResultCommitFailureRollsBackActivity(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	ctx := context.Background()
	before, err := l.store.Dive(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	s := resyncTestService(t, l.store, &fakeResyncClient{get: func(_ context.Context, id int64) (json.RawMessage, error) {
		return resyncDocument(id, exampleDiveNote), nil
	}})
	if _, err := l.store.db.Exec(`CREATE TRIGGER reject_resync_success BEFORE UPDATE OF status ON resync_items WHEN NEW.status='succeeded' BEGIN SELECT RAISE(ABORT,'test result commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	job, err := s.Enqueue(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.runNext(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ = l.store.resyncJob(ctx, job.ID)
	after, err := l.store.Dive(ctx, 42)
	if err != nil || job.Status != "failed" || job.ErrorCode != "save_failed" || job.Succeeded != 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("activity/result commit was not atomic: %+v %v", job, err)
	}
}

func TestResyncServiceEnvironment(t *testing.T) {
	clearApplicationEnvironment(t)
	store, _ := openTestSQLStore(t)
	s := newResyncService(store, testLogger(t))
	if s.factory != nil || s.unavailableCode != "garmin_not_configured" {
		t.Fatal("resync enabled without credentials")
	}
	s.Close()
	t.Setenv("DIVELOG_GARMIN_PERSONAL_API_KEY", "synthetic-api-key")
	t.Setenv("DIVELOG_GARMIN_REQUEST_INTERVAL", "invalid")
	s = newResyncService(store, testLogger(t))
	if s.factory != nil || s.unavailableCode != "invalid_garmin_configuration" {
		t.Fatal("invalid pacing accepted")
	}
	s.Close()
	t.Setenv("DIVELOG_GARMIN_REQUEST_INTERVAL", "5s")
	path := filepath.Join(t.TempDir(), "resync-tokens.json")
	t.Setenv("DIVELOG_GARMIN_RESYNC_TOKEN_FILE", path)
	s = newResyncService(store, testLogger(t))
	defer s.Close()
	if s.factory == nil || s.unavailableCode != "" {
		t.Fatal("personal resync unavailable")
	}
	if _, err := s.factory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("constructing resync client wrote credentials or contacted Garmin")
	}
}
