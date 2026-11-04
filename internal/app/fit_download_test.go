package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	fitdata "github.com/se0wtf/go-garmindives/internal/fit"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func TestFITDownloadsRequireAdminAndNeverAppearInJSON(t *testing.T) {
	a, config := bootstrapApplication(t)
	const download = "/api/v1/admin/dives/42/fit"
	appJSON[httpx.ErrorResponse](t, a, "GET", download, nil, nil, 503)
	setupApplication(t, a, config)
	store := waitApplicationReady(t, a).store
	body := testFIT(t)
	document, err := fitdata.DecodeDownload(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	document.Files[0].Name = "nested/original.FIT"
	record := testSQLDive(t, 42)
	record.FIT = document
	if err := store.SaveDives(context.Background(), []DiveRecord{record, testSQLDive(t, 43)}); err != nil {
		t.Fatal(err)
	}
	// A large private FIT cache must never inflate a public or administrator JSON response.
	var cached map[string]any
	if err := json.Unmarshal(storedFITJSON(t, store, 42), &cached); err != nil {
		t.Fatal(err)
	}
	cached["private_padding"] = strings.Repeat("private-fit-marker-", 32768)
	privateJSON, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE dives SET fit_json=? WHERE id=42`, string(privateJSON)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE dive_samples SET raw_json=? WHERE dive_id=42`, string(privateJSON)); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{download, "/api/v1/admin/dives/999/fit"} {
			if response := appRequest(t, a, method, path, nil, nil); response.Code != 401 {
				t.Fatalf("anonymous %s %s returned %d", method, path, response.Code)
			}
		}
	}
	cookie := loginApplication(t, a, testAdminPassword)
	for _, session := range []*http.Cookie{nil, cookie} {
		for _, path := range []string{"/api/v1/dives", "/api/v1/dives/42", "/api/v1/dives/42?include=fit", "/api/v1/dives/42/samples"} {
			response := appRequest(t, a, "GET", path, nil, session)
			if response.Code != 200 || response.Body.Len() > 64<<10 || bytes.Contains(response.Body.Bytes(), []byte("original_base64")) || bytes.Contains(response.Body.Bytes(), []byte("private-fit-marker")) {
				t.Fatalf("%s leaked FIT data or grew unexpectedly: status=%d bytes=%d", path, response.Code, response.Body.Len())
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if _, exists := payload["fit"]; exists {
				t.Fatal("FIT must be omitted entirely, including for authenticated JSON requests")
			}
			if strings.Contains(path, "/samples") {
				var items []map[string]json.RawMessage
				if err := json.Unmarshal(payload["items"], &items); err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					if _, exists := item["raw"]; exists {
						t.Fatal("sample raw data must be omitted for anonymous and administrator responses")
					}
				}
			}
		}
	}
	for _, session := range []*http.Cookie{nil, cookie} {
		if response := appRequest(t, a, "GET", "/api/v1/dives/42/fit", nil, session); response.Code != 404 {
			t.Fatalf("public FIT URL returned %d", response.Code)
		}
	}
	response := appRequest(t, a, "GET", download, nil, cookie)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), body) || response.Header().Get("Content-Type") != "application/octet-stream" || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("original FIT download: status=%d headers=%v", response.Code, response.Header())
	}
	disposition, parameters, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || parameters["filename"] != "original.FIT" {
		t.Fatalf("unsafe or missing download filename: %q", response.Header().Get("Content-Disposition"))
	}
	head := appRequest(t, a, "HEAD", download, nil, cookie)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("FIT HEAD returned status=%d bytes=%d headers=%v", head.Code, head.Body.Len(), head.Header())
	}
	request := httptest.NewRequest("GET", "https://dives.example.test"+download, nil)
	request.AddCookie(cookie)
	request.Header.Set("Range", "bytes=10-19")
	partial := httptest.NewRecorder()
	a.Handler().ServeHTTP(partial, request)
	if partial.Code != 206 || !bytes.Equal(partial.Body.Bytes(), body[10:20]) {
		t.Fatalf("FIT range request returned status=%d bytes=%d", partial.Code, partial.Body.Len())
	}
	for path, code := range map[string]string{
		"/api/v1/admin/dives/43/fit":  "fit_unavailable",
		"/api/v1/admin/dives/999/fit": "not_found",
	} {
		errorResponse := appJSON[httpx.ErrorResponse](t, a, "GET", path, nil, cookie, 404)
		if errorResponse.Error.Code != code {
			t.Fatalf("%s returned %s, want %s", path, errorResponse.Error.Code, code)
		}
	}
	appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/admin/dives/0/fit", nil, cookie, 400)
	appJSON[httpx.ErrorResponse](t, a, "GET", "/api/v1/admin/dives/invalid/fit", nil, cookie, 400)
	if _, err := store.db.Exec(`UPDATE dive_fit_files SET sha256=? WHERE dive_id=42`, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	appJSON[httpx.ErrorResponse](t, a, "GET", download, nil, cookie, 500)
	appJSON[diveDetail](t, a, "GET", "/api/v1/dives/42", nil, nil, 200)
	appJSON[map[string]any](t, a, "POST", "/api/v1/auth/logout", nil, cookie, 204)
	appJSON[httpx.ErrorResponse](t, a, "GET", download, nil, cookie, 401)
}

func TestAdminFITDownloadBundlesMultipleOriginals(t *testing.T) {
	a, config := bootstrapApplication(t)
	setupApplication(t, a, config)
	store := waitApplicationReady(t, a).store
	exerciseFITArchiveDownload(t, store, a.Handler(), loginApplication(t, a, testAdminPassword))
}

func exerciseFITArchiveDownload(t *testing.T, store *sqlStore, handler http.Handler, cookie *http.Cookie) {
	t.Helper()
	body := testFIT(t)
	chained := append(append([]byte{}, body...), body...)
	document, err := fitdata.DecodeDownload(context.Background(), testFITZIP(t, map[string][]byte{"one.fit": body, "two.fit": chained}))
	if err != nil {
		t.Fatal(err)
	}
	document.Files[0].Name = "../../duplicate.fit"
	document.Files[1].Name = `nested\duplicate.fit`
	record := testSQLDive(t, 900)
	record.FIT = document
	if err := store.SaveDives(context.Background(), []DiveRecord{record}); err != nil {
		t.Fatal(err)
	}
	files, err := store.FITFiles(context.Background(), 900)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		request := httptest.NewRequest(method, "https://dives.example.test/api/v1/admin/dives/900/fit", nil)
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 || response.Header().Get("Content-Type") != "application/zip" || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("FIT archive %s: status=%d headers=%v", method, response.Code, response.Header())
		}
		if method == "HEAD" {
			if response.Body.Len() != 0 {
				t.Fatal("HEAD included archive bytes")
			}
			continue
		}
		archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
		if err != nil || len(archive.File) != len(files) {
			t.Fatalf("invalid FIT archive: %v", err)
		}
		for index, entry := range archive.File {
			if entry.Name != fmt.Sprintf("%02d-duplicate.fit", index) {
				t.Fatalf("archive failed to flatten paths or distinguish duplicate names: %q", entry.Name)
			}
			reader, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(data, files[index].Data) {
				t.Fatalf("archive changed original FIT bytes: %v %v", readErr, closeErr)
			}
		}
	}
}
