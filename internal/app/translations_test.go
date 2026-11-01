package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	charmLog "github.com/charmbracelet/log"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	deeplclient "github.com/se0wtf/go-garmindives/internal/translation"
)

type translatorFunc func(context.Context, []string, string) ([]string, error)

func (f translatorFunc) Translate(ctx context.Context, texts []string, surrounding string) ([]string, error) {
	return f(ctx, texts, surrounding)
}

func englishTestTexts(texts []string) []string {
	translated := make([]string, len(texts))
	for i, text := range texts {
		translated[i] = "EN: " + text
	}
	return translated
}

func clearDeepLEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"DEEPL_API_KEY", "DEEPL_API_URL", "DEEPL_TARGET_LANG", "DEEPL_REQUEST_INTERVAL", "DEEPL_GLOSSARY_ID"} {
		t.Setenv(name, "")
	}
}

func addFrenchTestDive(t *testing.T, store *sqlStore, id int64, note string) {
	t.Helper()
	record := testSQLDive(t, id)
	record.DiveDetails.(map[string]any)["notes"] = note
	if err := store.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
}

func testTranslationService(t *testing.T, store *sqlStore, translator deeplclient.Translator) *translationService {
	t.Helper()
	service := newTranslationService(store, deeplclient.Config{Locale: "EN-GB", Interval: 20 * time.Millisecond}, translator, testLogger(t))
	store.translations = service
	t.Cleanup(service.Close)
	return service
}

func finishTranslations(t *testing.T, service *translationService) {
	t.Helper()
	if err := service.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		worked, _, err := service.workOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("translation queue did not settle")
}

func TestDeepLConfiguration(t *testing.T) {
	clearDeepLEnvironment(t)
	t.Setenv("GARMIN_GEOCODING_ENABLED", "false")
	t.Setenv("DEEPL_API_KEY", "synthetic:fx")
	config, err := deeplclient.FromEnvironment()
	if err != nil || config.Endpoint != "https://api-free.deepl.com/v2/translate" || config.Locale != "EN-GB" || config.Interval != 2*time.Second {
		t.Fatalf("default DeepL configuration: %v", err)
	}
	t.Setenv("DEEPL_API_KEY", "synthetic-pro")
	t.Setenv("DEEPL_TARGET_LANG", "en-us")
	t.Setenv("DEEPL_REQUEST_INTERVAL", "5s")
	config, err = deeplclient.FromEnvironment()
	if err != nil || config.Endpoint != "https://api.deepl.com/v2/translate" || config.Locale != "EN-US" || config.Interval != 5*time.Second {
		t.Fatalf("explicit DeepL configuration: %v", err)
	}
	for _, tc := range []struct{ name, value string }{
		{"DEEPL_REQUEST_INTERVAL", "0s"}, {"DEEPL_REQUEST_INTERVAL", "oops"}, {"DEEPL_TARGET_LANG", "DE"},
		{"DEEPL_API_URL", "http://example.com"}, {"DEEPL_API_URL", "https://user:secret@example.com"},
		{"DEEPL_API_URL", "https://example.com/?key=secret"}, {"DEEPL_API_URL", "https://example.com/path"},
	} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := deeplclient.FromEnvironment(); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("invalid configuration must return a sanitized error")
			}
		})
	}
}

func TestDeepLRequestAndResponse(t *testing.T) {
	const key = "synthetic-secret:fx"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v2/translate" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "DeepL-Auth-Key "+key {
			t.Error("incorrect translation request/authentication")
		}
		var body struct {
			Text     []string `json:"text"`
			Source   string   `json:"source_lang"`
			Target   string   `json:"target_lang"`
			Context  string   `json:"context"`
			Preserve bool     `json:"preserve_formatting"`
			Glossary string   `json:"glossary_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Source != "FR" || body.Target != "EN-GB" || body.Context != "Plongée sous-marine" || !body.Preserve || body.Glossary != "dive-glossary" {
			t.Error("incorrect translation parameters")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"translations": []map[string]string{{"text": "Eagle rays"}, {"text": "Sharks"}}})
	}))
	defer server.Close()
	client := deeplclient.NewClient(deeplclient.Config{APIKey: key, Locale: "EN-GB", Endpoint: server.URL + "/v2/translate", Glossary: "dive-glossary"})
	got, err := client.Translate(context.Background(), []string{"Raies aigles", "Requins"}, "Plongée sous-marine")
	if err != nil || !reflect.DeepEqual(got, []string{"Eagle rays", "Sharks"}) || calls.Load() != 1 {
		t.Fatalf("translation response=%v, err=%v", got, err)
	}
}

func TestDeepLErrorsAreBoundedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		status             int
		body, code         string
		retryable, blocked bool
	}{
		{403, `{"message":"synthetic-secret"}`, "translation_authentication_failed", false, true},
		{456, `{"message":"synthetic-secret"}`, "translation_quota_exceeded", false, true},
		{429, `{}`, "translation_rate_limited", true, false},
		{503, `{}`, "translation_provider_unavailable", true, false},
		{400, `{}`, "translation_configuration_error", false, true},
		{413, `{}`, "translation_text_too_large", false, false},
		{200, `{"translations":[]}`, "invalid_translation_response", true, false},
		{200, `{"translations":[{"text":null}]}`, "invalid_translation_response", true, false},
		{200, `{"translations":[{"text":""}]}`, "invalid_translation_response", true, false},
		{200, `not json`, "invalid_translation_response", true, false},
	} {
		t.Run(fmt.Sprintf("%d/%s/%s", tc.status, tc.code, tc.body), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "75")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := deeplclient.NewClient(deeplclient.Config{APIKey: "synthetic-secret", Locale: "EN-GB", Endpoint: server.URL})
			_, err := client.Translate(context.Background(), []string{"Bonjour"}, "")
			var failure *deeplclient.Failure
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Retryable != tc.retryable || failure.Blocked != tc.blocked || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("unexpected/sensitive translation error: %v", err)
			}
			if (tc.status == 429 || tc.status == 503) && failure.RetryAfter != 75*time.Second {
				t.Fatalf("Retry-After ignored: %v", failure.RetryAfter)
			}
		})
	}
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	client := deeplclient.NewClient(deeplclient.Config{APIKey: "synthetic-secret", Endpoint: redirect.URL})
	if _, err := client.Translate(context.Background(), []string{"Bonjour"}, ""); err == nil || leaked.Load() {
		t.Fatal("translation credentials followed a redirect")
	}
	now := time.Now().UTC().Truncate(time.Second)
	if httpx.RetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now) != 90*time.Second || httpx.RetryAfter("invalid", now) != 0 {
		t.Fatal("Retry-After date parsing failed")
	}
}

func TestTranslationAPIUsesPersistentCacheAndInvalidatesChangedText(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	addFrenchTestDive(t, l.store, 100, exampleDiveNote)
	addFrenchTestDive(t, l.store, 101, exampleDiveNote)
	photo, err := l.Upload(context.Background(), 100, "Tortue.jpg", strings.NewReader("original"), nil)
	if err != nil {
		t.Fatal(err)
	}
	caption, description := "Tortue", "Une tortue au-dessus du récif."
	photo, err = l.Patch(context.Background(), 100, photo.ID, mediaPatch{Caption: &caption, Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	service := testTranslationService(t, l.store, translatorFunc(func(_ context.Context, texts []string, surrounding string) ([]string, error) {
		calls.Add(1)
		if !strings.Contains(surrounding, "plongée") {
			t.Error("missing dive context")
		}
		return englishTestTexts(texts), nil
	}))
	h := newAPIHandler(l.store, testLogger(t))
	w := mediaRequest(t, h, "GET", "/api/v1/dives/100", nil, "")
	var detail diveDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 || detail.Dive.Translations.English.Status != "pending" || detail.Dive.Comment != exampleDiveComment || calls.Load() != 0 {
		t.Fatalf("pending API response=%s, err=%v", w.Body, err)
	}
	finishTranslations(t, service)
	if calls.Load() != 2 {
		t.Fatalf("identical dives should share one cached request, got %d calls", calls.Load())
	}
	patchCover(t, h, "100", `{"cover_media_id":"`+photo.ID+`"}`, 200)
	for i := 0; i < 3; i++ {
		w = mediaRequest(t, h, "GET", "/api/v1/dives/100", nil, "")
		if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
			t.Fatalf("dive response=%s, %v", w.Body, err)
		}
		translated := detail.Dive.Translations.English
		if translated.Status != "ready" || translated.Comment != "EN: "+exampleDiveComment || len(translated.Highlights) != 3 || translated.Highlights[1] != "EN: Raies aigles" || detail.Dive.Notes != exampleDiveNote || *detail.Dive.Rating != 4 {
			t.Fatalf("incorrect bilingual dive: %+v", detail.Dive)
		}
		if detail.Dive.CoverPhoto == nil || detail.Dive.CoverPhoto.Translations.English.Description != "EN: "+description || detail.Dive.CoverPhoto.Translations.English.Caption != "" {
			t.Fatal("cover did not reuse the media translation")
		}
		if len(detail.Media) != 1 || detail.Media[0].Translations.English.Caption != "EN: "+caption {
			t.Fatal("dive media is missing its English translation")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("viewing cached translations contacted the provider")
	}
	newDescription := "Une raie aigle près du tombant."
	path := "/api/v1/dives/100/media/" + photo.ID
	body, _ := json.Marshal(map[string]string{"description": newDescription})
	w = mediaRequest(t, h, "PATCH", path, bytes.NewReader(body), "application/json")
	changed := decodeMediaResponse(t, w, 200)
	if changed.Translations.English.Status != "pending" || changed.Translations.English.Description != "" || changed.Description != newDescription {
		t.Fatal("changed French text exposed a stale English translation")
	}
	finishTranslations(t, service)
	w = mediaRequest(t, h, "GET", path, nil, "")
	if got := decodeMediaResponse(t, w, 200); got.Translations.English.Description != "EN: "+newDescription || calls.Load() != 3 {
		t.Fatal("changed text was not translated once")
	}
	body, _ = json.Marshal(map[string]string{"description": description})
	mediaRequest(t, h, "PATCH", path, bytes.NewReader(body), "application/json")
	finishTranslations(t, service)
	if calls.Load() != 3 {
		t.Fatal("restoring text did not reuse the previous cached revision")
	}
	// Reopen without a provider/key: ready translations remain available.
	databasePath := l.store.path
	_ = l.store.Close()
	reopened, err := openSQLStore(context.Background(), appconfig.SQLite, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	w = mediaRequest(t, newAPIHandler(reopened, testLogger(t)), "GET", "/api/v1/dives/100", nil, "")
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 || detail.Dive.Translations.English.Status != "ready" || detail.Media[0].Translations.English.Description != "EN: "+description {
		t.Fatalf("restart lost cached English: %s, %v", w.Body, err)
	}
}

func TestTranslationBatchesResumeAfterFailure(t *testing.T) {
	store, _ := openTestSQLStore(t)
	highlights := make([]string, 55)
	for i := range highlights {
		highlights[i] = fmt.Sprintf("Observation %d", i)
	}
	highlights = append(highlights, highlights[0]) // Duplicate in a later batch.
	addFrenchTestDive(t, store, 100, "Commentaire: Beau récif\nHL:\n"+strings.Join(highlights, "\n"))
	var batches [][]string
	service := testTranslationService(t, store, translatorFunc(func(_ context.Context, texts []string, _ string) ([]string, error) {
		batches = append(batches, append([]string{}, texts...))
		if len(batches) == 2 {
			return nil, &deeplclient.Failure{Code: "translation_rate_limited", Retryable: true, RetryAfter: time.Minute}
		}
		return englishTestTexts(texts), nil
	}))
	now := time.Now()
	service.now = func() time.Time { return now }
	if err := service.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if worked, _, err := service.workOnce(context.Background()); err != nil || !worked || len(batches) != 1 || len(batches[0]) != 50 {
		t.Fatalf("first batch: %v", err)
	}
	if worked, delay, err := service.workOnce(context.Background()); err != nil || !worked || delay < time.Minute {
		t.Fatalf("retry cooldown: worked=%v, delay=%v, err=%v", worked, delay, err)
	}
	if worked, _, err := service.workOnce(context.Background()); err != nil || worked {
		t.Fatal("retried before Retry-After elapsed")
	}
	now = now.Add(time.Minute)
	if worked, _, err := service.workOnce(context.Background()); err != nil || !worked {
		t.Fatalf("resume failed: %v", err)
	}
	if len(batches) != 3 || len(batches[2]) != 6 || !reflect.DeepEqual(batches[1], batches[2]) {
		t.Fatalf("successful text was translated twice: batch sizes=%v", []int{len(batches[0]), len(batches[1]), len(batches[2])})
	}
	var result englishTranslation
	source := translationSource{Kind: "dive", Comment: "Beau récif", Highlights: highlights}
	if err := service.populate(context.Background(), []translationTarget{{source: source, destination: &result}}); err != nil || result.Status != "ready" || len(result.Highlights) != len(highlights) || result.Highlights[55] != "EN: "+highlights[0] {
		t.Fatalf("ordered resumed translation: %+v, %v", result, err)
	}
}

func TestTranslationQuotaPauseRetryAndFrenchFallback(t *testing.T) {
	store, _ := openTestSQLStore(t)
	addFrenchTestDive(t, store, 100, "Commentaire: Première plongée")
	var calls int
	fail := false
	service := testTranslationService(t, store, translatorFunc(func(_ context.Context, texts []string, _ string) ([]string, error) {
		calls++
		if fail {
			return nil, &deeplclient.Failure{Code: "translation_quota_exceeded", Blocked: true}
		}
		return englishTestTexts(texts), nil
	}))
	finishTranslations(t, service)
	fail = true
	addFrenchTestDive(t, store, 101, "Commentaire: Deuxième plongée")
	finishTranslations(t, service)
	if service.blockedReason() != "translation_quota_exceeded" || calls != 2 {
		t.Fatal("quota exhaustion did not pause provider requests")
	}
	if worked, _, err := service.workOnce(context.Background()); err != nil || worked || calls != 2 {
		t.Fatal("blocked worker made another provider call")
	}
	h := newAPIHandler(store, testLogger(t))
	w := mediaRequest(t, h, "GET", "/api/v1/dives/101", nil, "")
	var detail diveDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 || detail.Dive.Comment != "Deuxième plongée" || detail.Dive.Translations.English.Error != "translation_quota_exceeded" {
		t.Fatalf("provider error broke French API: %s, %v", w.Body, err)
	}
	w = mediaRequest(t, h, "GET", "/api/v1/dives/100", nil, "")
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.Dive.Translations.English.Status != "ready" {
		t.Fatal("quota pause hid an existing cached translation")
	}
	w = mediaRequest(t, h, "GET", "/api/v1/translations/status", nil, "")
	var status translationStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.BlockedReason == "" || status.Failed != 1 || status.Ready != 1 {
		t.Fatalf("translation status=%s, %v", w.Body, err)
	}
	fail = false
	w = mediaRequest(t, h, "POST", "/api/v1/translations/retry", nil, "")
	if w.Code != 202 {
		t.Fatalf("retry=%d %s", w.Code, w.Body)
	}
	finishTranslations(t, service)
	if calls != 3 || service.blockedReason() != "" {
		t.Fatal("explicit retry did not resume pending work")
	}
	store.translations = nil
	w = mediaRequest(t, newAPIHandler(store, testLogger(t)), "POST", "/api/v1/translations/retry", nil, "")
	if w.Code != 503 {
		t.Fatalf("retry without a provider=%d", w.Code)
	}
}

func TestTranslationLeasesAndObsoleteSources(t *testing.T) {
	l, _ := mediaTestLibrary(t)
	photo, err := l.Upload(context.Background(), 42, "Test.jpg", strings.NewReader("image"), nil)
	if err != nil {
		t.Fatal(err)
	}
	oldText, newText := "Ancien texte", "Nouveau texte"
	_, err = l.Patch(context.Background(), 42, photo.ID, mediaPatch{Description: &oldText})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	service := testTranslationService(t, l.store, translatorFunc(func(_ context.Context, texts []string, _ string) ([]string, error) {
		calls++
		if len(texts) != 1 || texts[0] != newText {
			t.Error("provider received obsolete text")
		}
		return englishTestTexts(texts), nil
	}))
	if err := service.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = l.Patch(context.Background(), 42, photo.ID, mediaPatch{Description: &newText})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := service.claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := newTranslationService(l.store, service.config, service.client, nil)
	if _, err := other.claim(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate in-flight job claimed: %v", err)
	}
	if _, err := l.store.db.Exec(`UPDATE translation_cache SET lease_until = 0 WHERE source_hash = ?`, job.key); err != nil {
		t.Fatal(err)
	}
	finishTranslations(t, other)
	if calls != 1 {
		t.Fatalf("obsolete work was translated: calls=%d", calls)
	}
	// A stale worker cannot overwrite a ready result after losing its lease.
	if err := service.saveJob(context.Background(), job, nil, "error", "stale", 1, 0); err != nil {
		t.Fatal(err)
	}
	var result englishTranslation
	if err := other.populate(context.Background(), []translationTarget{{source: translationSource{Kind: "media", Description: newText}, destination: &result}}); err != nil || result.Status != "ready" {
		t.Fatalf("stale worker overwrote cache: %+v, %v", result, err)
	}
}

func TestTranslationWorkerPacingAndChangeNotification(t *testing.T) {
	store, _ := openTestSQLStore(t)
	for i := 1; i <= 3; i++ {
		addFrenchTestDive(t, store, int64(i), fmt.Sprintf("Commentaire: Plongée %d", i))
	}
	times := make(chan time.Time, 4)
	service := testTranslationService(t, store, translatorFunc(func(_ context.Context, texts []string, _ string) ([]string, error) {
		times <- time.Now()
		return englishTestTexts(texts), nil
	}))
	service.Start(context.Background())
	var previous time.Time
	for i := 0; i < 3; i++ {
		select {
		case current := <-times:
			if !previous.IsZero() && current.Sub(previous) < 18*time.Millisecond {
				t.Fatalf("translation pacing bypassed: %v", current.Sub(previous))
			}
			previous = current
			for j := 0; j < 20; j++ {
				service.signal()
			}
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not translate initial records")
		}
	}
	addFrenchTestDive(t, store, 4, "Commentaire: Nouvelle plongée")
	select {
	case <-times:
	case <-time.After(3 * time.Second):
		t.Fatal("new sync data did not wake the translation worker")
	}
	service.Close()
}

func TestTranslationWorkerCancellationDoesNotBlockAPIReads(t *testing.T) {
	store, _ := openTestSQLStore(t)
	addFrenchTestDive(t, store, 100, "Commentaire: Beau récif")
	started := make(chan struct{})
	service := testTranslationService(t, store, translatorFunc(func(ctx context.Context, _ []string, _ string) ([]string, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	service.Start(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("translation did not start")
	}
	h := newAPIHandler(store, testLogger(t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := httptest.NewRequest("GET", "/api/v1/dives/100", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"pending"`) {
		t.Fatalf("API read waited for translation: %d %s", w.Code, w.Body)
	}
	service.Close()
	var leased, attempts int
	if err := store.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(attempts),0) FROM translation_cache WHERE lease_until <> 0`).Scan(&leased, &attempts); err != nil || leased != 0 || attempts != 0 {
		t.Fatalf("shutdown leaked work lease: %d/%d, %v", leased, attempts, err)
	}
}

func TestRunServeStartsTranslationBackfillAndResumesFailedWork(t *testing.T) {
	clearDeepLEnvironment(t)
	t.Setenv("GARMIN_GEOCODING_ENABLED", "false")
	store, _ := openTestSQLStore(t)
	addFrenchTestDive(t, store, 100, "Commentaire: Beau récif")
	seed := testTranslationService(t, store, nil)
	if err := seed.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE translation_cache SET status = 'error', attempts = 5, last_error = 'translation_authentication_failed'`); err != nil {
		t.Fatal(err)
	}
	const key = "synthetic-startup-secret:fx"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "DeepL-Auth-Key "+key {
			t.Error("serve did not configure DeepL credentials")
		}
		_, _ = io.WriteString(w, `{"translations":[{"text":"Beautiful reef"}]}`)
	}))
	defer provider.Close()
	t.Setenv("DEEPL_API_KEY", key)
	t.Setenv("DEEPL_API_URL", provider.URL)
	t.Setenv("DEEPL_REQUEST_INTERVAL", "1ms")
	var logs bytes.Buffer
	logger, err := NewAppLogger(&logs, "debug")
	if err != nil {
		t.Fatal(err)
	}
	err = runServeWithDependencies(context.Background(), configuredServeArgs(t, store, []string{"-output-type", "sqlite", "-assets-dir", t.TempDir()}), io.Discard, logger,
		func(context.Context, string, string) (*sqlStore, error) { return store, nil },
		func(ctx context.Context, _ string, app *application, _ *charmLog.Logger) error {
			running := waitApplicationReady(t, app).store
			h := newAPIHandler(running, logger)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				w := mediaRequest(t, h, "GET", "/api/v1/dives/100", nil, "")
				var detail diveDetail
				if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
					return fmt.Errorf("startup API response: %d %s", w.Code, w.Body)
				}
				if detail.Dive.Translations.English.Status == "ready" {
					if detail.Dive.Translations.English.Comment != "Beautiful reef" || detail.Dive.Comment != "Beau récif" {
						return errors.New("incorrect bilingual startup response")
					}
					var source, result, lastError string
					if err := running.db.QueryRowContext(ctx, `SELECT source_json, result_json, last_error FROM translation_cache`).Scan(&source, &result, &lastError); err != nil {
						return err
					}
					if strings.Contains(source+result+lastError, key) {
						return errors.New("translation cache contains provider credentials")
					}
					return nil
				}
				time.Sleep(5 * time.Millisecond)
			}
			return errors.New("startup did not resume translation backfill")
		})
	if err != nil || calls.Load() != 1 || strings.Contains(logs.String(), key) {
		t.Fatalf("translation startup: calls=%d, err=%v, credential leak=%v", calls.Load(), err, strings.Contains(logs.String(), key))
	}
}

func TestTranslationRetryExhaustionAndManualRecovery(t *testing.T) {
	store, _ := openTestSQLStore(t)
	addFrenchTestDive(t, store, 100, "Commentaire: Beau récif")
	var calls int
	fail := true
	service := testTranslationService(t, store, translatorFunc(func(_ context.Context, texts []string, _ string) ([]string, error) {
		calls++
		if fail {
			return nil, &deeplclient.Failure{Code: "translation_provider_unavailable", Retryable: true}
		}
		return englishTestTexts(texts), nil
	}))
	now := time.Now()
	service.now = func() time.Time { return now }
	if err := service.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		worked, delay, err := service.workOnce(context.Background())
		if err != nil || !worked || delay != 30*time.Second*time.Duration(1<<(attempt-1)) {
			t.Fatalf("attempt %d: worked=%v delay=%v err=%v", attempt, worked, delay, err)
		}
		if worked, _, err := service.workOnce(context.Background()); err != nil || worked {
			t.Fatalf("attempt %d bypassed backoff: %v", attempt, err)
		}
		now = now.Add(delay)
	}
	if worked, _, err := service.workOnce(context.Background()); err != nil || worked || calls != 5 {
		t.Fatal("exhausted job continued contacting the provider")
	}
	var result englishTranslation
	target := translationTarget{source: translationSource{Kind: "dive", Comment: "Beau récif"}, destination: &result}
	if err := service.populate(context.Background(), []translationTarget{target}); err != nil || result.Status != "error" || result.Error != "translation_provider_unavailable" {
		t.Fatalf("exhausted translation: %+v, %v", result, err)
	}
	fail = false
	if err := service.Retry(context.Background()); err != nil {
		t.Fatal(err)
	}
	finishTranslations(t, service)
	if err := service.populate(context.Background(), []translationTarget{target}); err != nil || result.Status != "ready" || calls != 6 {
		t.Fatalf("manual recovery: %+v, calls=%d, err=%v", result, calls, err)
	}
}

func TestTranslationDisabledEmptyAndOversizedContent(t *testing.T) {
	store, _ := openTestSQLStore(t)
	addFrenchTestDive(t, store, 100, "Commentaire: Beau récif")
	h := newAPIHandler(store, testLogger(t))
	w := mediaRequest(t, h, "GET", "/api/v1/dives/100", nil, "")
	var detail diveDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 || detail.Dive.Translations.English.Status != "disabled" || detail.Dive.Comment != "Beau récif" {
		t.Fatalf("disabled translation broke French response: %s, %v", w.Body, err)
	}
	w = mediaRequest(t, h, "GET", "/api/v1/translations/status", nil, "")
	var status translationStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 200 || status.Enabled {
		t.Fatalf("disabled status: %s, %v", w.Body, err)
	}
	var result englishTranslation
	if err := store.translationService().populate(context.Background(), []translationTarget{{source: translationSource{Kind: "dive"}, destination: &result}}); err != nil || result.Status != "ready" {
		t.Fatalf("empty source should need no translation: %+v, %v", result, err)
	}
	tooLarge := strings.Repeat("é", deeplclient.MaxTranslationRequestBytes)
	if _, err := store.db.Exec(`UPDATE dives SET note_comment = ? WHERE id = 100`, tooLarge); err != nil {
		t.Fatal(err)
	}
	service := testTranslationService(t, store, translatorFunc(func(context.Context, []string, string) ([]string, error) {
		t.Error("oversized source was sent to the provider")
		return nil, errors.New("unexpected provider call")
	}))
	finishTranslations(t, service)
	if err := service.populate(context.Background(), []translationTarget{{source: translationSource{Kind: "dive", Comment: tooLarge}, destination: &result}}); err != nil || result.Status != "error" || result.Error != "translation_text_too_large" {
		t.Fatalf("oversized source: status=%s code=%s err=%v", result.Status, result.Error, err)
	}
}
