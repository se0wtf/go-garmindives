package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func clearApplicationEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GARMIN_CONFIG", "GARMIN_DB_TYPE", "GARMIN_DB_URI", "GARMIN_OUTPUT_TYPE", "GARMIN_OUTPUT_DSN", "GARMIN_API_ADDR", "GARMIN_PUBLIC_URL", "GARMIN_COOKIE_SECURE", "GARMIN_SESSION_LIFETIME", "GARMIN_SESSION_IDLE_TIMEOUT", "GARMIN_ASSETS_DIR", "GARMIN_MEDIA_MAX_UPLOAD_BYTES"} {
		value, exists := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if exists {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	clearDeepLEnvironment(t)
	disableGeocodingForTest(t)
}
func bootstrapApplication(t *testing.T) (*application, loadedConfig) {
	t.Helper()
	clearApplicationEnvironment(t)
	directory := t.TempDir()
	file := defaultApplicationConfig()
	file.Database.URI = filepath.Join(directory, "dives.db")
	file.Media.Directory = filepath.Join(directory, "assets")
	c, err := resolveApplicationConfig(filepath.Join(directory, "config.yaml"), file)
	if err != nil {
		t.Fatal(err)
	}
	a := newApplication(context.Background(), c, testLogger(t), openSQLStore)
	t.Cleanup(a.Close)
	return a, c
}
func appRequest(t *testing.T, a *application, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, "https://dives.example.test"+path, input)
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func appJSON[T any](t *testing.T, a *application, method, path string, body any, cookie *http.Cookie, status int) T {
	t.Helper()
	w := appRequest(t, a, method, path, body, cookie)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d body=%s", method, path, w.Code, w.Body)
	}
	var value T
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
	}
	return value
}
func setupApplication(t *testing.T, a *application, c loadedConfig) {
	t.Helper()
	appJSON[map[string]any](t, a, "POST", "/api/v1/setup", setupRequest{Database: databasePatch{Type: &c.Effective.Database.Type, URI: &c.Effective.Database.URI}, AdminPassword: testAdminPassword, HomeCountries: []setupHomeCountry{{CountryCode: "France", From: "2020-01-01"}}}, nil, 201)
	waitApplicationReady(t, a)
}
func loginApplication(t *testing.T, a *application, password string) *http.Cookie {
	t.Helper()
	w := appRequest(t, a, "POST", "/api/v1/auth/login", map[string]string{"password": password}, nil)
	if w.Code != 200 {
		t.Fatalf("login failed: status=%d body=%s", w.Code, w.Body)
	}
	for _, cookie := range w.Result().Cookies() {
		if strings.Contains(cookie.Name, "garmindives_session") {
			return cookie
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestInitialSetupGatesAPIsAndPreservesSQLData(t *testing.T) {
	a, c := bootstrapApplication(t)
	status := appJSON[setupStatusResponse](t, a, "GET", "/api/v1/setup/status", nil, nil, 200)
	if status.Configured || status.Ready {
		t.Fatal("fresh server is already configured")
	}
	for _, path := range []string{"/api/v1/dives", "/api/v1/countries", "/assets/42/photo.jpg", "/api/v1/auth/session", "/api/v1/config"} {
		e := appJSON[errorResponse](t, a, "GET", path, nil, nil, 503)
		if e.Error.Code != "not_configured" {
			t.Fatalf("wrong setup gate: %+v", e)
		}
	}
	appJSON[errorResponse](t, a, "POST", "/api/v1/trips", map[string]any{}, nil, 503)
	appJSON[map[string]any](t, a, "GET", "/api/v1/setup/countries", nil, nil, 200)
	appJSON[map[string]any](t, a, "GET", "/healthz", nil, nil, 200)
	store, err := openSQLStore(context.Background(), outputSQLite, c.Effective.Database.URI)
	if err != nil {
		t.Fatal(err)
	}
	record := testSQLDive(t, 42)
	record.FIT, err = decodeFITDownload(context.Background(), testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
	files, err := store.FITFiles(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	setupApplication(t, a, c)
	status = appJSON[setupStatusResponse](t, a, "GET", "/api/v1/setup/status", nil, nil, 200)
	if !status.Configured || !status.Ready {
		t.Fatal("setup did not activate the API")
	}
	detail := appJSON[diveDetail](t, a, "GET", "/api/v1/dives/42", nil, nil, 200)
	if detail.Dive.ID != 42 || string(detail.FIT) == "null" {
		t.Fatal("setup lost existing dive data")
	}
	runtime := waitApplicationReady(t, a)
	storedFiles, err := runtime.store.FITFiles(context.Background(), 42)
	if err != nil || len(storedFiles) != len(files) || string(storedFiles[0].Data) != string(files[0].Data) {
		t.Fatal("setup changed original FIT files")
	}
	appJSON[errorResponse](t, a, "POST", "/api/v1/setup", map[string]string{"admin_password": "another secret passphrase"}, nil, 409)
	for _, path := range []string{"/api/v1/trips", "/api/v1/media/scan", "/api/v1/translations/retry", "/api/v1/countries/retry", "/api/v1/home-countries", "/api/v1/dives/42/media"} {
		e := appJSON[errorResponse](t, a, "POST", path, map[string]any{}, nil, 401)
		if e.Error.Code != "authentication_required" {
			t.Fatal("write API missing auth guard")
		}
	}
	appJSON[errorResponse](t, a, "PATCH", "/api/v1/dives/42", map[string]string{"country_code": "EG"}, nil, 401)
	appJSON[errorResponse](t, a, "PUT", "/api/v1/trips/"+newMediaID()+"/dives/42", nil, nil, 401)
	appJSON[errorResponse](t, a, "DELETE", "/api/v1/dives/42/media/anything", nil, nil, 401)
	appJSON[errorResponse](t, a, "GET", "/api/v1/config", nil, nil, 401)
	data, err := os.ReadFile(c.Path)
	if err != nil || strings.Contains(string(data), testAdminPassword) || strings.Contains(string(data), "argon2") {
		t.Fatal("configuration contains administrator credentials")
	}
	info, _ := os.Stat(c.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("configuration file permissions are not owner-only")
	}
	credentials, err := runtime.store.adminCredentials(context.Background())
	if err != nil || !strings.HasPrefix(credentials.passwordHash, "$argon2id$") || !verifyAdminPassword(credentials.passwordHash, testAdminPassword) {
		t.Fatal("admin password was not hashed with Argon2id")
	}
}

func TestDatabaseOutageAfterStartupFailsReadinessWithoutReopeningSetup(t *testing.T) {
	a, c := bootstrapApplication(t)
	setupApplication(t, a, c)
	runtime := waitApplicationReady(t, a)
	appJSON[map[string]string](t, a, "GET", "/readyz", nil, nil, 200)
	if err := runtime.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	status := appJSON[setupStatusResponse](t, a, "GET", "/api/v1/setup/status", nil, nil, 200)
	if !status.Configured || status.Ready || status.State != "database_unavailable" {
		t.Fatalf("wrong outage status: %+v", status)
	}
	appJSON[errorResponse](t, a, "GET", "/readyz", nil, nil, 503)
	appJSON[map[string]string](t, a, "GET", "/healthz", nil, nil, 200)
	appJSON[errorResponse](t, a, "POST", "/api/v1/setup", nil, nil, 409)
}

func TestSessionCookiesWritesCSRFAndAuthenticatedUploads(t *testing.T) {
	a, c := bootstrapApplication(t)
	setupApplication(t, a, c)
	appJSON[authSessionResponse](t, a, "GET", "/api/v1/auth/session", nil, nil, 200)
	appJSON[errorResponse](t, a, "POST", "/api/v1/auth/login", map[string]string{"password": "incorrect password"}, nil, 401)
	cookie := loginApplication(t, a, testAdminPassword)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Fatalf("insecure session cookie: %+v", cookie)
	}
	if session := appJSON[authSessionResponse](t, a, "GET", "/api/v1/auth/session", nil, cookie, 200); !session.Authenticated || session.ExpiresAt == "" {
		t.Fatal("session state missing")
	}
	trip := appJSON[diveTrip](t, a, "POST", "/api/v1/trips", map[string]any{"year": 2026, "country_code": "Egypt"}, cookie, 201)
	for _, origin := range []string{"https://attacker.example", "null"} {
		r := httptest.NewRequest("PATCH", "https://dives.example.test/api/v1/trips/"+trip.ID, strings.NewReader(`{"rating":1}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("cross-origin mutation accepted: %s status=%d", origin, w.Code)
		}
	}
	r := httptest.NewRequest("PATCH", "https://dives.example.test/api/v1/trips/"+trip.ID, strings.NewReader(`{"rating":1}`))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-site fetch metadata bypassed CSRF checks")
	}
	appJSON[diveTrip](t, a, "PATCH", "/api/v1/trips/"+trip.ID, map[string]float64{"rating": 4.5}, cookie, 200)
	runtime := waitApplicationReady(t, a)
	runtime.media.processor = &fakeMediaProcessor{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "Boat.jpg")
	part.Write([]byte("original boat photo"))
	writer.Close()
	r = httptest.NewRequest("POST", "https://dives.example.test/api/v1/trips/"+trip.ID+"/media", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("authenticated upload failed: %d %s", w.Code, w.Body)
	}
	var media mediaItem
	json.Unmarshal(w.Body.Bytes(), &media)
	w = appRequest(t, a, "GET", media.URL, nil, nil)
	if w.Code != 200 || w.Body.String() != "original boat photo" {
		t.Fatal("original media should remain publicly readable")
	}
	var token string
	var sessionData []byte
	if err := runtime.store.db.QueryRow(`SELECT token_hash,data FROM admin_sessions LIMIT 1`).Scan(&token, &sessionData); err != nil || token == cookie.Value || strings.Contains(string(sessionData), cookie.Value) || strings.Contains(string(sessionData), testAdminPassword) {
		t.Fatal("SQL session store contains raw credentials/tokens")
	}
	renewed := appRequest(t, a, "POST", "/api/v1/auth/login", map[string]string{"password": testAdminPassword}, cookie)
	if renewed.Code != 200 {
		t.Fatal("session renewal failed")
	}
	var newCookie *http.Cookie
	for _, next := range renewed.Result().Cookies() {
		if next.Name == cookie.Name {
			newCookie = next
		}
	}
	if newCookie == nil || newCookie.Value == cookie.Value {
		t.Fatal("login did not rotate session token")
	}
	appJSON[errorResponse](t, a, "PATCH", "/api/v1/trips/"+trip.ID, map[string]int{"rating": 2}, cookie, 401)
	appJSON[map[string]any](t, a, "POST", "/api/v1/auth/logout", nil, newCookie, 204)
	appJSON[errorResponse](t, a, "PATCH", "/api/v1/trips/"+trip.ID, map[string]int{"rating": 2}, newCookie, 401)
}

func TestPasswordChangesRevokeSessionsIncludingInFlightCopies(t *testing.T) {
	a, c := bootstrapApplication(t)
	setupApplication(t, a, c)
	first := loginApplication(t, a, testAdminPassword)
	second := loginApplication(t, a, testAdminPassword)
	runtime := waitApplicationReady(t, a)
	var oldData []byte
	if err := runtime.store.db.QueryRow(`SELECT data FROM admin_sessions WHERE token_hash=?`, sessionDigest(second.Value)).Scan(&oldData); err != nil {
		t.Fatal(err)
	}
	nextPassword := "Manta rays circle the open reef!"
	appJSON[authSessionResponse](t, a, "POST", "/api/v1/auth/password", map[string]string{"current_password": testAdminPassword, "new_password": nextPassword}, first, 200)
	// Simulate a request that loaded a session before revocation and committed
	// its old data afterward. The durable auth version must still reject it.
	if err := (&sqlSessionStore{s: runtime.store}).Commit(second.Value, oldData, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range []*http.Cookie{first, second} {
		appJSON[errorResponse](t, a, "POST", "/api/v1/trips", map[string]any{"year": 2026, "country_code": "EG"}, cookie, 401)
	}
	appJSON[errorResponse](t, a, "POST", "/api/v1/auth/login", map[string]string{"password": testAdminPassword}, nil, 401)
	current := loginApplication(t, a, nextPassword)
	if _, err := runtime.store.db.Exec(`UPDATE admin_sessions SET expires_at=0 WHERE token_hash=?`, sessionDigest(current.Value)); err != nil {
		t.Fatal(err)
	}
	appJSON[errorResponse](t, a, "POST", "/api/v1/trips", map[string]any{}, current, 401)
}

func TestSetupRollbackConcurrencyAndDatabaseAdoption(t *testing.T) {
	a, c := bootstrapApplication(t)
	request := setupRequest{AdminPassword: testAdminPassword, HomeCountries: []setupHomeCountry{{CountryCode: "FR", From: "2020-01-01"}}}
	a.writeConfig = func(string, applicationConfig) error { return errors.New("synthetic failure") }
	appJSON[errorResponse](t, a, "POST", "/api/v1/setup", request, nil, 500)
	store, err := openSQLStore(context.Background(), outputSQLite, c.Effective.Database.URI)
	if err != nil {
		t.Fatal(err)
	}
	var admins, homes int
	store.db.QueryRow(`SELECT COUNT(*) FROM admin_credentials`).Scan(&admins)
	store.db.QueryRow(`SELECT COUNT(*) FROM home_countries`).Scan(&homes)
	store.Close()
	if admins != 0 || homes != 0 {
		t.Fatal("failed configuration write partially initialized SQL")
	}
	a.writeConfig = writeApplicationConfig
	var codes [2]int
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i] = appRequest(t, a, "POST", "/api/v1/setup", request, nil).Code }(i)
	}
	wg.Wait()
	if !((codes[0] == 201 && codes[1] == 409) || (codes[1] == 201 && codes[0] == 409)) {
		t.Fatalf("concurrent setup results: %v", codes)
	}
	waitApplicationReady(t, a)
	cookie := loginApplication(t, a, testAdminPassword)
	a.Close()
	loaded, err := loadApplicationConfig([]string{"-config", c.Path})
	if err != nil {
		t.Fatal(err)
	}
	restarted := newApplication(context.Background(), loaded, testLogger(t), openSQLStore)
	t.Cleanup(restarted.Close)
	waitApplicationReady(t, restarted)
	if session := appJSON[authSessionResponse](t, restarted, "GET", "/api/v1/auth/session", nil, cookie, 200); !session.Authenticated {
		t.Fatal("session did not survive restart")
	}
	restarted.Close()
	// Losing the configuration file must not permit changing the password of
	// an initialized database without proving knowledge of its existing one.
	if err := os.Remove(c.Path); err != nil {
		t.Fatal(err)
	}
	adopt := newApplication(context.Background(), c, testLogger(t), openSQLStore)
	t.Cleanup(adopt.Close)
	wrong := request
	wrong.AdminPassword = "A different valid secret phrase!"
	appJSON[errorResponse](t, adopt, "POST", "/api/v1/setup", wrong, nil, 401)
	appJSON[map[string]any](t, adopt, "POST", "/api/v1/setup", request, nil, 201)
	waitApplicationReady(t, adopt)
}

func TestConfiguredDatabaseFailureDoesNotReopenSetup(t *testing.T) {
	a, c := bootstrapApplication(t)
	setupApplication(t, a, c)
	a.Close()
	loaded, err := loadApplicationConfig([]string{"-config", c.Path})
	if err != nil {
		t.Fatal(err)
	}
	broken := newApplication(context.Background(), loaded, testLogger(t), func(context.Context, string, string) (*sqlStore, error) { return nil, errors.New("synthetic outage") })
	t.Cleanup(broken.Close)
	status := appJSON[setupStatusResponse](t, broken, "GET", "/api/v1/setup/status", nil, nil, 200)
	if !status.Configured || status.Ready {
		t.Fatal("database outage changed configured state")
	}
	appJSON[errorResponse](t, broken, "POST", "/api/v1/setup", map[string]any{}, nil, 409)
	appJSON[errorResponse](t, broken, "GET", "/api/v1/dives", nil, nil, 503)
}

func TestAdminPasswordPolicyAndLoginThrottling(t *testing.T) {
	for _, password := range []string{"short", "123456789012345", "passwordpassword", "aaaaaaaaaaaaaaa", strings.Repeat(" ", 20), strings.Repeat("a", 129)} {
		if validateAdminPassword(password) == nil {
			t.Fatal("weak/oversized password accepted")
		}
	}
	for _, password := range []string{"  coral reefs beneath moonlight  ", "珊瑚の庭をゆっくり泳ぐマンタたちと青い海", "739204865127903"} {
		if err := validateAdminPassword(password); err != nil {
			t.Fatalf("valid passphrase rejected: %v", err)
		}
	}
	hash, err := hashAdminPassword("  coral reefs beneath moonlight  ")
	if err != nil || !verifyAdminPassword(hash, "  coral reefs beneath moonlight  ") || verifyAdminPassword(hash, "coral reefs beneath moonlight") {
		t.Fatal("password hashing trimmed significant whitespace")
	}
	if verifyAdminPassword("$argon2id$v=19$m=4294967295,t=3,p=1$invalid$invalid", testAdminPassword) {
		t.Fatal("unbounded password hash parameters accepted")
	}
	a, c := bootstrapApplication(t)
	setupApplication(t, a, c)
	limited := false
	for i := 0; i < 15; i++ {
		w := appRequest(t, a, "POST", "/api/v1/auth/login", map[string]string{"password": "wrong"}, nil)
		if w.Code == 429 {
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("throttling omitted retry hint")
			}
			limited = true
			break
		}
		if w.Code != 401 {
			t.Fatalf("unexpected login status: %d", w.Code)
		}
	}
	if !limited {
		t.Fatal("login attempts were not rate limited")
	}
}
