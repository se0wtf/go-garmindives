package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func patchCover(t *testing.T, handler http.Handler, id, body string, status int) diveSummary {
	t.Helper()
	w := mediaRequest(t, handler, "PATCH", "/api/v1/dives/"+id, strings.NewReader(body), "application/json")
	if w.Code != status {
		t.Fatalf("PATCH cover status=%d, want=%d: %s", w.Code, status, w.Body)
	}
	var summary diveSummary
	if status == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
			t.Fatal(err)
		}
	}
	return summary
}

func TestDiveCoverAPIListDetailValidationAndClear(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	ctx := context.Background()
	photo, err := l.Upload(ctx, 42, "Turtle été #1.jpeg", strings.NewReader("original photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	video, err := l.Upload(ctx, 42, "Reef.mp4", strings.NewReader("video"), nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := l.Upload(ctx, 99, "Other dive.jpg", strings.NewReader("other photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler(l.store, testLogger(t))
	list := mediaRequest(t, h, "GET", "/api/v1/dives", nil, "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"cover_photo":null`) || !strings.Contains(list.Body.String(), `"cover_media_id":null`) {
		t.Fatalf("unset covers = %d %s", list.Code, list.Body)
	}
	selected := patchCover(t, h, "42", `{"cover_media_id":"`+photo.ID+`"}`, 200)
	if selected.CoverMediaID == nil || *selected.CoverMediaID != photo.ID || selected.CoverPhoto == nil || selected.CoverPhoto.ID != photo.ID || selected.CoverPhoto.URL != photo.URL || selected.CoverPhoto.ThumbnailURL != photo.ThumbnailURL || selected.CoverPhoto.Filename != photo.Filename {
		t.Fatalf("selected cover = %+v", selected)
	}
	list = mediaRequest(t, h, "GET", "/api/v1/dives?limit=1", nil, "")
	var first diveListResponse
	if err := json.Unmarshal(list.Body.Bytes(), &first); err != nil || len(first.Items) != 1 || first.Items[0].ID != 99 || first.Items[0].CoverPhoto != nil || first.NextCursor == "" {
		t.Fatalf("first page = %s, %v", list.Body, err)
	}
	list = mediaRequest(t, h, "GET", "/api/v1/dives?limit=1&cursor="+first.NextCursor, nil, "")
	var second diveListResponse
	if err := json.Unmarshal(list.Body.Bytes(), &second); err != nil || len(second.Items) != 1 || second.Items[0].ID != 42 || second.Items[0].CoverPhoto == nil || second.Items[0].CoverPhoto.URL != photo.URL || second.NextCursor != "" {
		t.Fatalf("cover missing from paginated list = %s, %v", list.Body, err)
	}
	w := mediaRequest(t, h, "GET", "/api/v1/dives/42", nil, "")
	var detail diveDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.Dive.CoverPhoto == nil || detail.Dive.CoverPhoto.ID != photo.ID {
		t.Fatalf("cover missing from detail = %s, %v", w.Body, err)
	}
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"cover_media_id":42}`, `{"cover_media_id":""}`,
		`{"cover_media_id":"missing"}`, `{"cover_media_id":"` + video.ID + `"}`,
		`{"cover_media_id":"` + other.ID + `"}`, `{"cover_media_id":null,"name":"changed"}`,
		`{"cover_media_id":null} {}`, `{"cover_media_id":`,
	} {
		patchCover(t, h, "42", body, 400)
	}
	retained, err := l.store.Dive(ctx, 42)
	if err != nil || retained.Dive.CoverMediaID == nil || *retained.Dive.CoverMediaID != photo.ID {
		t.Fatalf("invalid selection changed the cover: %+v, %v", retained.Dive, err)
	}
	patchCover(t, h, "999", `{"cover_media_id":null}`, 404)
	patchCover(t, h, "invalid", `{"cover_media_id":null}`, 400)
	cleared := patchCover(t, h, "42", `{"cover_media_id":null}`, 200)
	if cleared.CoverMediaID != nil || cleared.CoverPhoto != nil {
		t.Fatalf("clear retained a cover: %+v", cleared)
	}
	if _, err := l.root.Stat("42/" + photo.Filename); err != nil {
		t.Fatalf("clearing a cover deleted its photo: %v", err)
	}
}

func TestDiveCoverFollowsThumbnailsAndClearsOnDeletion(t *testing.T) {
	l, processor := mediaTestLibrary(t)
	ctx := context.Background()
	processor.fail = true
	photo, err := l.Upload(ctx, 42, "Turtle.jpg", strings.NewReader("original"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler(l.store, testLogger(t))
	selected := patchCover(t, h, "42", `{"cover_media_id":"`+photo.ID+`"}`, 200)
	if selected.CoverPhoto == nil || selected.CoverPhoto.URL == "" || selected.CoverPhoto.ThumbnailURL != "" {
		t.Fatalf("cover without thumbnail needs an original fallback: %+v", selected)
	}
	processor.fail = false
	if _, err := l.Scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	dive, err := l.store.Dive(ctx, 42)
	if err != nil || dive.Dive.CoverPhoto == nil || dive.Dive.CoverPhoto.ThumbnailURL == "" {
		t.Fatalf("rebuilt thumbnail missing from cover: %+v, %v", dive.Dive, err)
	}
	previous := dive.Dive.CoverPhoto.ThumbnailURL
	writeTestMedia(t, l, "42/Turtle.jpg", []byte("replaced photo with different size"))
	if _, err := l.Scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	dive, err = l.store.Dive(ctx, 42)
	if err != nil || dive.Dive.CoverPhoto == nil || dive.Dive.CoverPhoto.ID != photo.ID || dive.Dive.CoverPhoto.ThumbnailURL == previous {
		t.Fatalf("cover lost selection or kept stale thumbnail URL: %+v, %v", dive.Dive, err)
	}
	if err := l.Delete(ctx, 42, photo.ID); err != nil {
		t.Fatal(err)
	}
	dive, err = l.store.Dive(ctx, 42)
	if err != nil || dive.Dive.CoverMediaID != nil || dive.Dive.CoverPhoto != nil {
		t.Fatalf("deleting cover media retained the selection: %+v, %v", dive.Dive, err)
	}
	photo, err = l.Upload(ctx, 42, "On disk.jpg", strings.NewReader("another photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	patchCover(t, h, "42", `{"cover_media_id":"`+photo.ID+`"}`, 200)
	if err := l.root.Remove("42/On disk.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	dive, err = l.store.Dive(ctx, 42)
	if err != nil || dive.Dive.CoverMediaID != nil || dive.Dive.CoverPhoto != nil {
		t.Fatalf("scanning a deleted cover retained the selection: %+v, %v", dive.Dive, err)
	}
}

func TestDiveCoverPersistsAcrossSyncAndRestart(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	ctx := context.Background()
	photo, err := l.Upload(ctx, 42, "Cover.jpg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.store.setDiveCover(ctx, 42, &photo.ID); err != nil {
		t.Fatal(err)
	}
	// Exercise FIT backfill's update path, not just an ignored duplicate insert.
	record := testSQLDive(t, 42)
	record.FIT, err = decodeFITDownload(ctx, testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.store.SaveDives(ctx, []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
	path := l.store.path
	_ = l.store.Close()
	store, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dive, err := store.Dive(ctx, 42)
	if err != nil || dive.Dive.CoverMediaID == nil || *dive.Dive.CoverMediaID != photo.ID || dive.Dive.CoverPhoto == nil || dive.Dive.CoverPhoto.ThumbnailURL != photo.ThumbnailURL {
		t.Fatalf("sync/restart lost cover: %+v, %v", dive.Dive, err)
	}
	if _, err := store.db.Exec(`DELETE FROM dives WHERE id = 42`); err != nil {
		t.Fatalf("dive/media cover foreign keys prevent deleting the dive: %v", err)
	}
	if _, err := store.mediaByID(ctx, 42, photo.ID); !errors.Is(err, errMediaNotFound) {
		t.Fatalf("dive removal retained media: %v", err)
	}
}

func TestDiveCoverMigratesExistingVersionFiveDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v5.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := &sqlStore{db: db, dialect: outputSQLite}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:5] {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range migration.statements {
			if _, err := tx.Exec(statement); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if migration.run != nil {
			if err := migration.run(ctx, old, tx); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, migration.version); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.SaveDives(ctx, []DiveRecord{testSQLDive(t, 42)}); err != nil {
		t.Fatal(err)
	}
	media := mediaItem{ID: "existing-photo", DiveID: 42, Filename: "Kept.jpg", Kind: "photo", ContentType: "image/jpeg", SizeBytes: 5, SHA256: "retained", StereoLayout: "auto", Projection: "flat", ThumbnailStatus: "error", Probe: json.RawMessage(`{}`)}
	if err := old.saveMedia(ctx, media); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dive, err := store.Dive(ctx, 42)
	if err != nil || dive.Dive.CoverPhoto != nil || dive.Dive.CoverMediaID != nil || len(dive.Media) != 1 || dive.Media[0].Filename != media.Filename {
		t.Fatalf("migration changed existing data: %+v, %v", dive, err)
	}
	if _, err := store.setDiveCover(ctx, 42, &media.ID); err != nil {
		t.Fatalf("migrated photo cannot be selected: %v", err)
	}
	if err := store.removeMedia(ctx, media); err != nil {
		t.Fatal(err)
	}
	var cover sql.NullString
	if err := store.db.QueryRow(`SELECT cover_media_id FROM dives WHERE id = 42`).Scan(&cover); err != nil || cover.Valid {
		t.Fatalf("migrated foreign key did not clear cover: %v, %v", cover, err)
	}
}
