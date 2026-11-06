package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

func TestResyncAPIsRequireAdminAndReportAsyncProgress(t *testing.T) {
	a, c := bootstrapApplication(t)
	appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/admin/resync/status", nil, nil, 503)
	setupApplication(t, a, c)
	cookie := loginApplication(t, a, testAdminPassword)
	runtime := waitApplicationReady(t, a)
	if err := runtime.store.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 42)}); err != nil {
		t.Fatal(err)
	}
	status := appJSON[resyncStatusResponse](t, a, "GET", "/api/v1/admin/resync/status", nil, cookie, 200)
	if status.Available || status.UnavailableCode != "garmin_not_configured" || status.Busy || status.ActiveJob != nil {
		t.Fatalf("unconfigured resync status: %+v", status)
	}
	appJSON[httpx.ErrorResponse](t, a, "POST", "/api/v1/admin/dives/42/resync", nil, cookie, 503)
	runtime.resync.Close()
	entered := make(chan struct{})
	client := &fakeResyncClient{get: func(ctx context.Context, id int64) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	service := resyncTestService(t, runtime.store, client)
	runtime.resync = service
	service.Start(a.ctx)
	jobPath := "/api/v1/admin/resync/jobs/" + identity.NewID()
	for _, request := range []struct{ method, path string }{
		{"POST", "/api/v1/admin/dives/42/resync"}, {"POST", "/api/v1/admin/dives/resync"},
		{"GET", "/api/v1/admin/resync/status"}, {"HEAD", "/api/v1/admin/resync/status"}, {"GET", "/api/v1/admin/resync/jobs"},
		{"GET", jobPath}, {"GET", jobPath + "/items"}, {"POST", jobPath + "/cancel"},
	} {
		appJSON[httpx.ErrorResponse](t, a, request.method, request.path, nil, nil, 401)
	}
	r := httptest.NewRequest("POST", "https://dives.example.test/api/v1/admin/dives/resync", nil)
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin resync accepted")
	}
	requestCtx, disconnect := context.WithCancel(context.Background())
	r = httptest.NewRequest("POST", "https://dives.example.test/api/v1/admin/dives/42/resync", nil).WithContext(requestCtx)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	disconnect()
	if w.Code != 202 {
		t.Fatalf("enqueue: %d %s", w.Code, w.Body)
	}
	var job resyncJob
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("Location") != job.URL || job.Scope != "activity" || job.ActivityID != 42 {
		t.Fatalf("job response: %+v", job)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("asynchronous worker did not start")
	}
	status = appJSON[resyncStatusResponse](t, a, "GET", "/api/v1/admin/resync/status", nil, cookie, 200)
	if !status.Available || !status.Busy || status.ActiveJob == nil || status.ActiveJob.CurrentActivityID != 42 || status.PollIntervalSeconds != 2 {
		t.Fatalf("progress: %+v", status)
	}
	appJSON[resyncJob](t, a, "GET", job.URL, nil, cookie, 200)
	appJSON[resyncItemList](t, a, "GET", job.ItemsURL+"?status=running", nil, cookie, 200)
	appJSON[resyncJobList](t, a, "GET", "/api/v1/admin/resync/jobs", nil, cookie, 200)
	w = appRequest(t, a, "POST", "/api/v1/admin/dives/resync", nil, cookie)
	if w.Code != 409 || w.Header().Get("Location") != job.URL {
		t.Fatal("busy response missing active job")
	}
	appJSON[resyncJob](t, a, "POST", job.URL+"/cancel", nil, cookie, 202)
	waitResync(t, service, job.ID, "canceled")
	appJSON[resyncJob](t, a, "POST", job.URL+"/cancel", nil, cookie, 200)
	appJSON[httpx.ErrorResponse](t, a, "POST", "/api/v1/admin/dives/404/resync", nil, cookie, 404)
	appJSON[httpx.ErrorResponse](t, a, "POST", "/api/v1/admin/dives/-1/resync", nil, cookie, 400)
	appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/admin/resync/jobs/invalid", nil, cookie, 400)
	appJSON[httpx.ErrorResponse](t, a, "GET", jobPath, nil, cookie, 404)
}

func TestResyncHistoryAndItemPagination(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	s := resyncTestService(t, l.store, &fakeResyncClient{get: func(_ context.Context, id int64) (json.RawMessage, error) {
		return resyncDocument(id, exampleDiveNote), nil
	}})
	for i := 0; i < 2; i++ {
		if _, err := s.Enqueue(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.runNext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h := newAPIHandler(l.store, testLogger(t))
	first := travelRequest[resyncJobList](t, h, "GET", "/api/v1/admin/resync/jobs?limit=1", nil, 200)
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatal("missing history pagination")
	}
	second := travelRequest[resyncJobList](t, h, "GET", "/api/v1/admin/resync/jobs?limit=1&cursor="+first.NextCursor, nil, 200)
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID || second.NextCursor != "" {
		t.Fatal("history cursor repeated job")
	}
	items := travelRequest[resyncItemList](t, h, "GET", first.Items[0].ItemsURL+"?limit=1", nil, 200)
	next := travelRequest[resyncItemList](t, h, "GET", first.Items[0].ItemsURL+"?limit=1&cursor="+items.NextCursor, nil, 200)
	if len(items.Items) != 1 || items.Items[0].ActivityID != 42 || len(next.Items) != 1 || next.Items[0].ActivityID != 99 {
		t.Fatal("activity cursor repeated/skipped items")
	}
	travelRequest[httpx.ErrorResponse](t, h, "GET", first.Items[0].ItemsURL+"?status=unknown", nil, 400)
	travelRequest[httpx.ErrorResponse](t, h, "GET", "/api/v1/admin/resync/jobs?cursor=invalid", nil, 400)
}

func TestResyncFetchLegacyAndInvalidResponses(t *testing.T) {
	client := &fakeResyncClient{get: func(context.Context, int64) (json.RawMessage, error) {
		return json.RawMessage(`{"connectId":42,"notes":"N:4/5"}`), nil
	}, activity: func(context.Context, int64) (json.RawMessage, error) {
		return json.RawMessage(`{"activityId":42,"activityType":{"typeKey":"single_gas_diving"},"activityName":"Legacy name"}`), nil
	}}
	if record, err := fetchResyncMetadata(context.Background(), client, 42); err != nil || record.FIT != nil || record.ActivityDetails != nil {
		t.Fatalf("legacy data-only record: %+v %v", record, err)
	}
	for _, raw := range []string{`null`, `{`, `{"activityId":43}`, `{"activityId":42,"activityTypeDTO":{"typeKey":"running"}}`} {
		client.get = func(context.Context, int64) (json.RawMessage, error) { return json.RawMessage(raw), nil }
		client.activity = nil
		if _, err := fetchResyncMetadata(context.Background(), client, 42); err == nil {
			t.Fatalf("invalid response accepted: %s", raw)
		}
	}
	if strings.Contains(classifyResyncError(errors.New("secret provider message")).message, "secret") {
		t.Fatal("provider message leaked")
	}
}

func exerciseResyncPostgres(t *testing.T, store *sqlStore) {
	t.Helper()
	ctx := context.Background()
	const id int64 = 9099
	if err := store.SaveDives(ctx, []DiveRecord{testSQLDive(t, id)}); err != nil {
		t.Fatal(err)
	}
	previous := store.resync
	s := resyncTestService(t, store, &fakeResyncClient{get: func(_ context.Context, id int64) (json.RawMessage, error) {
		return resyncDocument(id, exampleDiveNote), nil
	}})
	defer func() { store.resync = previous }()
	job, err := s.Enqueue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.runNext(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = store.resyncJob(ctx, job.ID)
	if err != nil || job.Status != "succeeded" || job.Succeeded != 1 {
		t.Fatalf("PostgreSQL resync: %+v %v", job, err)
	}
	dive, err := store.Dive(ctx, id)
	if err != nil || dive.Dive.Rating == nil || *dive.Dive.Rating != 4 {
		t.Fatal("PostgreSQL metadata not refreshed")
	}
	h := newAPIHandler(store, testLogger(t))
	travelRequest[resyncJobList](t, h, "GET", "/api/v1/admin/resync/jobs", nil, 200)
	travelRequest[resyncItemList](t, h, "GET", job.ItemsURL, nil, 200)
	queued, err := s.Enqueue(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.runNext(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ = store.resyncJob(ctx, queued.ID)
	if job.Status != "canceled" {
		t.Fatal("PostgreSQL cancellation failed")
	}
}
