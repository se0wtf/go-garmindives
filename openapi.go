package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

//go:embed docs/openapi.yaml
var openAPIYAML []byte

//go:embed docs/swagger/*
var swaggerFiles embed.FS

// YAML is the editable source; JSON is derived from it once for client generators
// and Swagger UI. Both are embedded so installed binaries need no working files.
var openAPIJSON = sync.OnceValues(func() ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(openAPIYAML, &document); err != nil {
		return nil, err
	}
	return json.MarshalIndent(document, "", "  ")
})

func registerDocumentationRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/openapi.yaml", documentationFile("openapi.yaml", "application/yaml; charset=utf-8", openAPIYAML))
	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		data, err := openAPIJSON()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal_error", "could not load OpenAPI document")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		http.ServeContent(w, r, "openapi.json", time.Time{}, bytes.NewReader(data))
	})
	mux.HandleFunc("GET /swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "swagger/", http.StatusTemporaryRedirect)
	})
	for _, path := range []string{"/docs", "/docs/{$}"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/swagger/", http.StatusTemporaryRedirect)
		})
	}
	// Explicit routes keep unrelated files and directory listings out of the docs.
	for name, contentType := range map[string]string{
		"index.html":                       "text/html; charset=utf-8",
		"swagger-initializer.js":           "text/javascript; charset=utf-8",
		"swagger-ui-bundle.js":             "text/javascript; charset=utf-8",
		"swagger-ui.css":                   "text/css; charset=utf-8",
		"LICENSE":                          "text/plain; charset=utf-8",
		"swagger-ui-bundle.js.LICENSE.txt": "text/plain; charset=utf-8",
	} {
		data, err := swaggerFiles.ReadFile("docs/swagger/" + name)
		if err != nil {
			panic(err) // Missing embedded UI assets are a build/development error.
		}
		handler := documentationFile(name, contentType, data)
		mux.Handle("GET /swagger/"+name, handler)
		if name == "index.html" {
			mux.Handle("GET /swagger/{$}", handler)
		}
	}
}

func documentationFile(name, contentType string, data []byte) http.Handler {
	hash := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(hash[:]) + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}
