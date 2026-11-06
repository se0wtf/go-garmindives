package app

import (
	"context"
	"net/http/httptest"
	"testing"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func TestPendingAPIsRequireAdminAndSupportReview(t *testing.T) {
	photoTestTools(t)
	a, c := bootstrapApplication(t)
	appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/admin/media/pending", nil, nil, 503)
	setupApplication(t, a, c)
	cookie := loginApplication(t, a, testAdminPassword)
	runtime := waitApplicationReady(t, a)
	p := runtime.pending
	if err := runtime.store.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 42)}); err != nil {
		t.Fatal(err)
	}
	putPending(t, p, "No metadata.png", photoPNG(t, 800, 600, false))
	if _, err := p.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.db.Exec(`UPDATE pending_media SET stable_since=stable_since-3000`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	item := onlyPending(t, p)
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/admin/media/pending", nil},
		{"GET", "/api/v1/admin/media/pending/status", nil},
		{"GET", "/api/v1/admin/media/pending/" + item.ID, nil},
		{"GET", item.URL, nil}, {"HEAD", item.URL, nil}, {"GET", item.ThumbnailURL, nil},
		{"POST", "/api/v1/admin/media/pending/scan", nil},
		{"POST", "/api/v1/admin/media/pending/" + item.ID + "/assign", map[string]int64{"dive_id": 42}},
		{"DELETE", "/api/v1/admin/media/pending/" + item.ID, nil},
	} {
		appJSON[httpx.ErrorResponse](t, a, request.method, request.path, request.body, nil, 401)
	}
	list := appJSON[pendingListResponse](t, a, "GET", "/api/v1/admin/media/pending?reason=missing_capture_time&limit=1", nil, cookie, 200)
	if len(list.Items) != 1 || list.Items[0].ID != item.ID {
		t.Fatal("admin review list missing photo")
	}
	status := appJSON[pendingStatusResponse](t, a, "GET", "/api/v1/admin/media/pending/status", nil, cookie, 200)
	if status.Total != 1 || status.MatchWindowSeconds != 600 || status.ScanIntervalSeconds != 60 {
		t.Fatalf("pending status: %+v", status)
	}
	for _, path := range []string{item.URL, item.ThumbnailURL} {
		w := appRequest(t, a, "GET", path, nil, cookie)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("private pending file: %d %s", w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("GET", "https://dives.example.test"+item.URL, nil)
	r.AddCookie(cookie)
	r.Header.Set("Range", "bytes=0-7")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 206 || w.Body.Len() != 8 {
		t.Fatal("private original byte ranges failed")
	}
	if w := appRequest(t, a, "HEAD", item.URL, nil, cookie); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("pending HEAD failed")
	}
	if w := appRequest(t, a, "GET", "/assets/pending/No%20metadata.png", nil, nil); w.Code == 200 {
		t.Fatal("pending file leaked through public assets")
	}
	appJSON[httpx.ErrorResponse](t, a, "POST", "/api/v1/admin/media/pending/"+item.ID+"/assign", map[string]int64{"dive_id": -1}, cookie, 400)
	appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/admin/media/pending?reason=unknown", nil, cookie, 400)
	appJSON[httpx.ErrorResponse](t, a, "POST", "/api/v1/admin/media/pending/scan?force=invalid", nil, cookie, 400)
	m := appJSON[mediaItem](t, a, "POST", "/api/v1/admin/media/pending/"+item.ID+"/assign", map[string]int64{"dive_id": 42}, cookie, 200)
	if m.DiveID != 42 || m.ThumbnailStatus != "ready" {
		t.Fatal("manual pending assignment failed")
	}
	appJSON[httpx.ErrorResponse](t, a, "GET", item.URL, nil, cookie, 404)
	appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/admin/media/pending/"+item.ID, nil, cookie, 404)
	appJSON[map[string]string](t, a, "POST", "/api/v1/admin/media/pending/scan", nil, cookie, 202)
}

func TestPendingPaginationAndDeleteAPI(t *testing.T) {
	p, _, reader, clock := pendingTestService(t)
	reader.capture = nil
	putPending(t, p, "B.jpg", []byte("B"))
	putPending(t, p, "A.jpg", []byte("A"))
	settlePending(t, p, clock)
	h := newAPIHandler(p.store, testLogger(t))
	page := travelRequest[pendingListResponse](t, h, "GET", "/api/v1/admin/media/pending?limit=1", nil, 200)
	if len(page.Items) != 1 || page.Items[0].Filename != "A.jpg" || page.NextCursor == "" {
		t.Fatal("pending cursor first page")
	}
	next := travelRequest[pendingListResponse](t, h, "GET", "/api/v1/admin/media/pending?limit=1&cursor="+page.NextCursor, nil, 200)
	if len(next.Items) != 1 || next.Items[0].Filename != "B.jpg" || next.NextCursor != "" {
		t.Fatal("pending cursor continuation")
	}
	travelRequest[map[string]any](t, h, "DELETE", "/api/v1/admin/media/pending/"+page.Items[0].ID, nil, 204)
	travelRequest[httpx.ErrorResponse](t, h, "GET", "/api/v1/admin/media/pending/"+page.Items[0].ID, nil, 404)
}
