package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const exampleDiveNote = `N:4/5
Commentaire:
Enfin pu faire la passe !
On parle de canyons mais ce sont plus des tranchés que des vrais canyons.
HL:
Pleiiin de requins vers les 50m
Raies aigles
Requin dormeur`

const exampleDiveComment = "Enfin pu faire la passe !\nOn parle de canyons mais ce sont plus des tranchés que des vrais canyons."

var exampleDiveHighlights = []string{"Pleiiin de requins vers les 50m", "Raies aigles", "Requin dormeur"}

func noteTestFloat(value float64) *float64 { return &value }

func assertParsedDiveNotes(t *testing.T, got parsedDiveNotes, rating, maximum *float64, comment string, highlights []string) {
	t.Helper()
	if !reflect.DeepEqual(got.Rating, rating) || !reflect.DeepEqual(got.RatingMax, maximum) || got.Comment != comment || !reflect.DeepEqual(got.Highlights, highlights) {
		t.Fatalf("parsed note = rating %v / %v, comment=%q, highlights=%v; want %v / %v, %q, %v",
			got.Rating, got.RatingMax, got.Comment, got.Highlights, rating, maximum, comment, highlights)
	}
}

func TestParseDiveNotes(t *testing.T) {
	for _, tc := range []struct {
		name, notes     string
		rating, maximum *float64
		comment         string
		highlights      []string
	}{
		{"provided example", exampleDiveNote, noteTestFloat(4), noteTestFloat(5), exampleDiveComment, exampleDiveHighlights},
		{"missing sections", "Clear water", nil, nil, "", []string{}},
		{"zero rating", "N:0/5", noteTestFloat(0), noteTestFloat(5), "", []string{}},
		{"no denominator", "N:4.5", noteTestFloat(4.5), nil, "", []string{}},
		{"French decimals and spacing", "n\u00a0: 4,5 / 5", noteTestFloat(4.5), noteTestFloat(5), "", []string{}},
		{"preserve French text spaces", "N\u202f:4/5\nCommentaire\u00a0: Magnifique\u00a0!\nHL:\n- \n•\nRequins", noteTestFloat(4), noteTestFloat(5), "Magnifique\u00a0!", []string{"Requins"}},
		{"LF and inline sections", "Commentaires: Très beau récif\n\nBeaucoup de poissons.\nHL: Raie aigle\n- Requins\n\n• Tortue\n* Nudibranche", nil, nil, "Très beau récif\n\nBeaucoup de poissons.", []string{"Raie aigle", "Requins", "Tortue", "Nudibranche"}},
		{"CRLF and any order", "\ufeff hl : Tortue\r\nRaie\r\n commentaire : Première ligne\r\nDeuxième ligne\r\n N : 8 / 10\r\n", noteTestFloat(8), noteTestFloat(10), "Première ligne\nDeuxième ligne", []string{"Tortue", "Raie"}},
		{"repeated sections", "N:2/5\nCommentaire: Première partie\nHL: Tortue\nN:4/5\nCommentaire: Suite\nHL: Requin", noteTestFloat(4), noteTestFloat(5), "Première partie\n\nSuite", []string{"Tortue", "Requin"}},
		{"do not parse inline prose", "Commentaire: Le guide a écrit N:4/5 dans son carnet.\nHL: Requin-nourrice\n-50m", nil, nil, "Le guide a écrit N:4/5 dans son carnet.", []string{"Requin-nourrice", "-50m"}},
		{"explicit empty sections", "N:\nCommentaire:\nHL:", nil, nil, "", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertParsedDiveNotes(t, parseDiveNotes(tc.notes, ""), tc.rating, tc.maximum, tc.comment, tc.highlights)
		})
	}
}

func TestParseDiveNotesInvalidRatingsAndSourcePrecedence(t *testing.T) {
	for _, value := range []string{"N:-1/5", "N:6/5", "N:4/0", "N:4/-5", "N:NaN/5", "N:Inf/5", "N:bad", "N:4/5 trailing text", "N:" + strings.Repeat("9", 400) + "/5"} {
		got := parseDiveNotes(value+"\nCommentaire: Kept\nHL: Tortue", "")
		assertParsedDiveNotes(t, got, nil, nil, "Kept", []string{"Tortue"})
	}
	assertParsedDiveNotes(t, parseDiveNotes("", exampleDiveNote), noteTestFloat(4), noteTestFloat(5), exampleDiveComment, exampleDiveHighlights)
	assertParsedDiveNotes(t, parseDiveNotes("Clear water", exampleDiveNote), noteTestFloat(4), noteTestFloat(5), exampleDiveComment, exampleDiveHighlights)
	assertParsedDiveNotes(t, parseDiveNotes("N:0/5\nCommentaire: Dive note wins", exampleDiveNote), noteTestFloat(0), noteTestFloat(5), "Dive note wins", exampleDiveHighlights)
	assertParsedDiveNotes(t, parseDiveNotes("N:invalid\nCommentaire:\nHL:", exampleDiveNote), nil, nil, "", []string{})
}

func TestDiveNotesStoredAndExposedForLegacyAndPersonalAPI(t *testing.T) {
	store, _ := openTestSQLStore(t)
	ctx := context.Background()
	legacy := testSQLDive(t, 42)
	legacy.DiveDetails.(map[string]any)["notes"] = exampleDiveNote
	var personal map[string]any
	if err := json.Unmarshal(personalDiveFixture(t), &personal); err != nil {
		t.Fatal(err)
	}
	personal["activityId"], personal["description"] = float64(43), exampleDiveNote
	personalRecord := DiveRecord{ID: 43, Activity: map[string]any{}, DiveDetails: personal}
	zero := testSQLDive(t, 44)
	zero.DiveDetails.(map[string]any)["notes"] = "N:0/5"
	if err := store.SaveDives(ctx, []DiveRecord{legacy, personalRecord, zero, testSQLDive(t, 45)}); err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler(store, testLogger(t))
	for _, id := range []string{"42", "43"} {
		w := mediaRequest(t, h, "GET", "/api/v1/dives/"+id, nil, "")
		var detail diveDetail
		if err := json.Unmarshal(w.Body.Bytes(), &detail); w.Code != 200 || err != nil {
			t.Fatalf("dive detail = %d %s, %v", w.Code, w.Body, err)
		}
		assertParsedDiveNotes(t, parsedDiveNotes{Rating: detail.Dive.Rating, RatingMax: detail.Dive.RatingMax, Comment: detail.Dive.Comment, Highlights: detail.Dive.Highlights}, noteTestFloat(4), noteTestFloat(5), exampleDiveComment, exampleDiveHighlights)
		if id == "42" && (detail.Dive.Notes != exampleDiveNote || detail.Dive.Description != "Boat dive") {
			t.Fatal("parsing changed the original legacy note/description")
		}
		if id == "43" && detail.Dive.Description != exampleDiveNote {
			t.Fatal("parsing changed the original Personal API description")
		}
	}
	w := mediaRequest(t, h, "GET", "/api/v1/dives", nil, "")
	var list diveListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil || len(list.Items) != 4 {
		t.Fatalf("dive list = %d %s, %v", w.Code, w.Body, err)
	}
	for _, item := range list.Items {
		switch item.ID {
		case 42, 43:
			assertParsedDiveNotes(t, parsedDiveNotes{Rating: item.Rating, RatingMax: item.RatingMax, Comment: item.Comment, Highlights: item.Highlights}, noteTestFloat(4), noteTestFloat(5), exampleDiveComment, exampleDiveHighlights)
		case 44:
			if item.Rating == nil || *item.Rating != 0 {
				t.Fatal("zero rating was lost")
			}
		case 45:
			if item.Rating != nil || item.RatingMax != nil || item.Comment != "" || item.Highlights == nil || len(item.Highlights) != 0 {
				t.Fatalf("unstructured note must have null/empty derived fields: %+v", item)
			}
		}
	}
	var score, maximum float64
	var comment, highlights string
	if err := store.db.QueryRow(`SELECT rating, rating_max, note_comment, highlights_json FROM dives WHERE id = 42`).Scan(&score, &maximum, &comment, &highlights); err != nil || score != 4 || maximum != 5 || comment != exampleDiveComment {
		t.Fatalf("parsed fields were not persisted: %v", err)
	}
	// FIT-only enrichment must retain the note and the corresponding parsed fields.
	legacy.DiveDetails.(map[string]any)["notes"] = "N:1/5\nCommentaire: Should not overwrite stored text"
	var err error
	legacy.FIT, err = decodeFITDownload(ctx, testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDives(ctx, []DiveRecord{legacy}); err != nil {
		t.Fatal(err)
	}
	kept, err := store.Dive(ctx, 42)
	if err != nil || kept.Dive.Notes != exampleDiveNote || kept.Dive.Rating == nil || *kept.Dive.Rating != 4 {
		t.Fatalf("FIT backfill changed parsed notes: %+v, %v", kept.Dive, err)
	}
}

func TestDiveNotesMigrationBackfillsCachedRecordsInBatches(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v7.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := &sqlStore{db: db, dialect: outputSQLite}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:7] {
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
	count := diveNoteMigrationBatchSize + 3
	for i := 1; i <= count; i++ {
		note, description := exampleDiveNote, "Original description"
		if i%2 == 0 {
			note, description = "", exampleDiveNote
		}
		if _, err := db.Exec(`INSERT INTO dives
			(id, name, activity_type, dive_type, start_time, timezone, location, notes, description,
			activity_json, activity_details_json, dive_details_json, fit_json)
			VALUES (?, 'Cached dive', 'diving', '', '', '', '', ?, ?, '{}', '{}', '{}', 'null')`, i, note, description); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := openSQLStore(ctx, outputSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []int64{1, 2, diveNoteMigrationBatchSize, int64(count)} {
		detail, err := store.Dive(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		assertParsedDiveNotes(t, parsedDiveNotes{Rating: detail.Dive.Rating, RatingMax: detail.Dive.RatingMax, Comment: detail.Dive.Comment, Highlights: detail.Dive.Highlights}, noteTestFloat(4), noteTestFloat(5), exampleDiveComment, exampleDiveHighlights)
		if (id%2 == 1 && detail.Dive.Notes != exampleDiveNote) || (id%2 == 0 && detail.Dive.Description != exampleDiveNote) {
			t.Fatal("migration modified the raw source note")
		}
	}
	var parsedCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dives WHERE rating = 4 AND rating_max = 5`).Scan(&parsedCount); err != nil || parsedCount != count {
		t.Fatalf("migration did not backfill every batch: %d, %v", parsedCount, err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("note migration is not idempotent: %v", err)
	}
}
