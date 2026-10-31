package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMediaDescriptionAPIForPhotosAndVideos(t *testing.T) {
	for _, name := range []string{"Turtle.jpg", "Reef.mp4"} {
		t.Run(name, func(t *testing.T) {
			l, processor := mediaTestLibrary(t)
			ctx := context.Background()
			m, err := l.Upload(ctx, 42, name, strings.NewReader("original bytes"), nil)
			if err != nil || m.Description != "" {
				t.Fatalf("new media description = %q, %v", m.Description, err)
			}
			h := newAPIHandler(l.store, testLogger(t))
			path := "/api/v1/dives/42/media/" + m.ID
			before := processor.thumbs.Load()
			description := "Une tortue au-dessus du récif 🐢.\nDepth: 12 m."
			body, err := json.Marshal(map[string]string{"description": description, "caption": "Turtle encounter"})
			if err != nil {
				t.Fatal(err)
			}
			w := mediaRequest(t, h, "PATCH", path, strings.NewReader(string(body)), "application/json")
			updated := decodeMediaResponse(t, w, 200)
			if updated.Description != description || updated.Caption != "Turtle encounter" || updated.ThumbnailURL != m.ThumbnailURL || updated.SHA256 != m.SHA256 || processor.thumbs.Load() != before {
				t.Fatalf("description-only metadata update = %+v", updated)
			}
			w = mediaRequest(t, h, "GET", path, nil, "")
			if detail := decodeMediaResponse(t, w, 200); detail.Description != description {
				t.Fatalf("media detail description = %q", detail.Description)
			}
			w = mediaRequest(t, h, "GET", "/api/v1/dives/42/media", nil, "")
			var list mediaListResponse
			if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil || len(list.Items) != 1 || list.Items[0].Description != description {
				t.Fatalf("media list omitted description: %s, %v", w.Body, err)
			}
			w = mediaRequest(t, h, "GET", "/api/v1/dives/42", nil, "")
			var dive diveDetail
			if err := json.Unmarshal(w.Body.Bytes(), &dive); w.Code != 200 || err != nil || len(dive.Media) != 1 || dive.Media[0].Description != description {
				t.Fatalf("dive detail omitted description: %s, %v", w.Body, err)
			}
			if m.Kind == "photo" {
				patchCover(t, h, "42", `{"cover_media_id":"`+m.ID+`"}`, 200)
				w = mediaRequest(t, h, "GET", "/api/v1/dives?limit=10", nil, "")
				var dives diveListResponse
				if err := json.Unmarshal(w.Body.Bytes(), &dives); w.Code != 200 || err != nil {
					t.Fatalf("dive list = %d %s, %v", w.Code, w.Body, err)
				}
				found := false
				for _, item := range dives.Items {
					if item.ID == 42 {
						found = item.CoverPhoto != nil && item.CoverPhoto.Description == description
					}
				}
				if !found {
					t.Fatalf("cover photo omitted description: %s", w.Body)
				}
			}
			w = mediaRequest(t, h, "PATCH", path, strings.NewReader(`{"caption":"Updated caption"}`), "application/json")
			if kept := decodeMediaResponse(t, w, 200); kept.Description != description {
				t.Fatalf("omitting description replaced it: %q", kept.Description)
			}
			w = mediaRequest(t, h, "PATCH", path, strings.NewReader(`{"description":""}`), "application/json")
			cleared := decodeMediaResponse(t, w, 200)
			if cleared.Description != "" || cleared.Caption != "Updated caption" {
				t.Fatalf("clearing description = %+v", cleared)
			}
		})
	}
}

func TestMediaDescriptionByteLimitAndEscapedJSON(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	m, err := l.Upload(context.Background(), 42, "Photo.jpeg", strings.NewReader("photo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler(l.store, testLogger(t))
	path := "/api/v1/dives/42/media/" + m.ID
	// The decoded text is 8192 UTF-8 bytes. JSON escaping makes the request
	// larger, so the request limit must accommodate valid escaped descriptions.
	body := `{"description":"` + strings.Repeat(`\u00e9`, 4096) + `"}`
	w := mediaRequest(t, h, "PATCH", path, strings.NewReader(body), "application/json")
	updated := decodeMediaResponse(t, w, 200)
	if updated.Description != strings.Repeat("é", 4096) {
		t.Fatal("description did not retain the exact Unicode text at its byte limit")
	}
	for _, body := range []string{
		`{"description":42}`, `{"description":false}`, `{"description":null}`,
		`{"description":"` + strings.Repeat("é", 4097) + `"}`,
	} {
		w := mediaRequest(t, h, "PATCH", path, strings.NewReader(body), "application/json")
		if w.Code != 400 {
			t.Fatalf("invalid description accepted: status=%d, body=%s", w.Code, w.Body)
		}
	}
	kept, err := l.store.mediaByID(context.Background(), 42, m.ID)
	if err != nil || kept.Description != updated.Description {
		t.Fatalf("invalid update changed description: %v", err)
	}
}
