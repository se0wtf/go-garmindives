package app

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func createMediaTrip(t *testing.T, h http.Handler, country string) diveTrip {
	t.Helper()
	return travelRequest[diveTrip](t, h, "POST", "/api/v1/trips", map[string]any{"year": 2026, "country_code": country}, 201)
}
func TestTripOwnMediaUploadsScanningThumbnailsAndCover(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	h := newAPIHandler(library.store, testLogger(t))
	trip := createMediaTrip(t, h, "Egypt")
	other := createMediaTrip(t, h, "France")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "Le bateau.jpeg")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("cruise photo"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	w := mediaRequest(t, h, "POST", "/api/v1/trips/"+trip.ID+"/media", &body, writer.FormDataContentType())
	photo := decodeMediaResponse(t, w, 201)
	if photo.TripID != trip.ID || photo.DiveID != 0 || photo.Filename != "Le bateau.jpeg" || photo.URL != "/assets/trips/"+trip.ID+"/Le%20bateau.jpeg" || photo.ThumbnailStatus != "ready" || !strings.HasPrefix(photo.ThumbnailURL, "/api/v1/trips/"+trip.ID+"/media/") {
		t.Fatalf("trip upload: %+v", photo)
	}
	if w := mediaRequest(t, h, "GET", photo.URL, nil, ""); w.Code != 200 || w.Body.String() != "cruise photo" {
		t.Fatalf("trip original: %d %s", w.Code, w.Body)
	}
	if w := mediaRequest(t, h, "GET", photo.ThumbnailURL, nil, ""); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("trip thumbnail: %d", w.Code)
	}
	photo = travelRequest[mediaItem](t, h, "PATCH", "/api/v1/trips/"+trip.ID+"/media/"+photo.ID, map[string]string{"description": "Notre bateau pendant la croisière"}, 200)
	trip = travelRequest[diveTrip](t, h, "PATCH", "/api/v1/trips/"+trip.ID, map[string]string{"cover_media_id": photo.ID}, 200)
	if trip.CoverPhoto == nil || trip.CoverPhoto.ID != photo.ID || trip.CoverPhoto.Description != photo.Description {
		t.Fatal("trip cover did not expose its photo")
	}
	travelRequest[httpx.ErrorResponse](t, h, "PATCH", "/api/v1/trips/"+other.ID, map[string]string{"cover_media_id": photo.ID}, 400)
	travelRequest[httpx.ErrorResponse](t, h, "PATCH", "/api/v1/trips/"+trip.ID+"/media/"+photo.ID, map[string]bool{"favorite": true}, 400)
	writeTestMedia(t, library, "trips/"+trip.ID+"/Tour du bateau.mp4", []byte("360 video"))
	if result, err := library.Scan(context.Background(), false); err != nil || result.Added != 1 {
		t.Fatalf("scan trip directory: %+v %v", result, err)
	}
	items := travelRequest[mediaListResponse](t, h, "GET", "/api/v1/trips/"+trip.ID+"/media", nil, 200)
	if len(items.Items) != 2 {
		t.Fatal("scanned trip media is missing")
	}
	var video mediaItem
	for _, item := range items.Items {
		if item.Kind == "video" {
			video = item
		}
	}
	video = travelRequest[mediaItem](t, h, "PATCH", "/api/v1/trips/"+trip.ID+"/media/"+video.ID, map[string]string{"projection": "360"}, 200)
	if video.EffectiveProjection != "360" {
		t.Fatal("trip 360 video hint missing")
	}
	travelRequest[httpx.ErrorResponse](t, h, "PATCH", "/api/v1/trips/"+trip.ID, map[string]string{"cover_media_id": video.ID}, 400)
	writeTestMedia(t, library, "trips/"+trip.ID+"/Le bateau.jpeg", []byte("updated original"))
	if _, err := library.Scan(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	updated := travelRequest[mediaItem](t, h, "GET", "/api/v1/trips/"+trip.ID+"/media/"+photo.ID, nil, 200)
	if updated.Description != photo.Description || updated.ID != photo.ID {
		t.Fatal("scan replaced trip media metadata/identity")
	}
	travelRequest[map[string]any](t, h, "DELETE", "/api/v1/trips/"+trip.ID+"/media/"+photo.ID, nil, 204)
	detail := travelRequest[tripDetail](t, h, "GET", "/api/v1/trips/"+trip.ID, nil, 200)
	if detail.Trip.CoverMediaID != nil || detail.Trip.CoverPhoto != nil {
		t.Fatal("deleting photo did not clear trip cover")
	}
	travelRequest[map[string]any](t, h, "DELETE", "/api/v1/trips/"+trip.ID, nil, 204)
	if _, err := library.root.Stat("trips/" + trip.ID + "/Tour du bateau.mp4"); err != nil {
		t.Fatal("deleting trip removed its original media file")
	}
	if _, err := library.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := library.store.db.QueryRow(`SELECT COUNT(*) FROM trip_media WHERE trip_id=?`, trip.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("scanner pruned retained deleted-trip media: %d %v", count, err)
	}
}

func TestTripMediaConfinesPathsAndFavoritePersistsThroughScan(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	s := library.store
	h := newAPIHandler(s, testLogger(t))
	trip := createMediaTrip(t, h, "EG")
	for _, name := range []string{"../escape.jpg", "a/b.jpg", ".hidden.jpg"} {
		if _, err := library.uploadOwned(context.Background(), mediaOwner{TripID: trip.ID}, name, strings.NewReader("photo"), nil); err == nil {
			t.Fatalf("unsafe trip filename accepted: %s", name)
		}
	}
	photo, err := library.Upload(context.Background(), 42, "Favorite.jpg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	favorite := true
	if _, err := library.Patch(context.Background(), 42, photo.ID, mediaPatch{Favorite: &favorite}); err != nil {
		t.Fatal(err)
	}
	writeTestMedia(t, library, "42/Favorite.jpg", []byte("replacement"))
	if _, err := library.Scan(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	m, err := s.mediaByID(context.Background(), 42, photo.ID)
	if err != nil || !m.Favorite {
		t.Fatal("filesystem scan lost favorite flag")
	}
	if err := os.Symlink("42", library.directory+"/trips"); err != nil {
		t.Fatal(err)
	}
	if _, err := library.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM trip_media`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("symlinked trip directory was scanned: %v", err)
	}
}

func TestTripTranslationsReuseMediaCacheAndGeneratedNames(t *testing.T) {
	library, _ := mediaTestLibrary(t)
	s := library.store
	h := newAPIHandler(s, testLogger(t))
	trip := createMediaTrip(t, h, "EG")
	if trip.Name != "Égypte 2026" || trip.NameEN != "Egypt 2026" || trip.Translations.English.Name != "Egypt 2026" || trip.Translations.English.Status != "ready" {
		t.Fatalf("generated bilingual names need no DeepL call: %+v", trip)
	}
	trip = travelRequest[diveTrip](t, h, "PATCH", "/api/v1/trips/"+trip.ID, map[string]string{"name": "Les requins", "subtitle": "Une semaine en mer", "description": "Notre voyage", "operator_comment": "Équipage sympathique"}, 200)
	desc := "Le bateau de croisière"
	photo, err := library.uploadOwned(context.Background(), mediaOwner{TripID: trip.ID}, "Bateau.jpg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.patchOwned(context.Background(), photo.owner(), photo.ID, mediaPatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	var calls int
	service := testTranslationService(t, s, translatorFunc(func(_ context.Context, texts []string, _ string) ([]string, error) {
		calls++
		values := englishTestTexts(texts)
		for i, text := range texts {
			if text == "Les requins" {
				values[i] = "Shark expedition"
			}
		}
		return values, nil
	}))
	finishTranslations(t, service)
	detail := travelRequest[tripDetail](t, h, "GET", "/api/v1/trips/"+trip.ID, nil, 200)
	if detail.Trip.NameEN != "Shark expedition" || detail.Trip.Translations.English.Subtitle != "EN: Une semaine en mer" || detail.Trip.Translations.English.OperatorComment != "EN: Équipage sympathique" || len(detail.Media) != 1 || detail.Media[0].Translations.English.Description != "EN: "+desc {
		raw, _ := json.Marshal(detail)
		t.Fatalf("trip translations: %s", raw)
	}
	if calls != 2 {
		t.Fatalf("unexpected translation requests: %d", calls)
	}
	travelRequest[tripDetail](t, h, "GET", "/api/v1/trips/"+trip.ID, nil, 200)
	if calls != 2 {
		t.Fatal("view translated trip contacted provider")
	}
	list := travelRequest[tripListResponse](t, h, "GET", "/api/v1/trips?name=Shark%20expedition", nil, 200)
	if len(list.Items) != 1 || list.Items[0].ID != trip.ID {
		t.Fatal("English translated custom name is not searchable")
	}
	travelRequest[diveTrip](t, h, "PATCH", "/api/v1/trips/"+trip.ID, map[string]string{"name": "Autre voyage"}, 200)
	travelRequest[diveTrip](t, h, "PATCH", "/api/v1/trips/"+trip.ID, map[string]string{"name": "Les requins"}, 200)
	list = travelRequest[tripListResponse](t, h, "GET", "/api/v1/trips?name=Shark%20expedition", nil, 200)
	if len(list.Items) != 1 || calls != 2 {
		t.Fatal("restoring a cached custom name lost English search or called the provider")
	}

}
