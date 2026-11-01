package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

func TestYAMLSettingsEnvironmentPrecedenceAndRelativePaths(t *testing.T) {
	clearApplicationEnvironment(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	file := appconfig.Default()
	file.Database.URI = "relative.db"
	if err := appconfig.Write(path, file); err != nil {
		t.Fatal(err)
	}
	c, err := appconfig.Load([]string{"-config", path})
	if err != nil || c.DatabaseOutput().Endpoint != filepath.Join(dir, "relative.db") || c.AssetsDirectory() != filepath.Join(dir, "assets") {
		t.Fatalf("relative configuration paths: %+v %v", c, err)
	}
	file.Database.URI = "file:relative.db?mode=rwc"
	if err := appconfig.Write(path, file); err != nil {
		t.Fatal(err)
	}
	c, err = appconfig.Load([]string{"-config=" + path})
	if err != nil || !strings.Contains(c.DatabaseOutput().Endpoint, "/relative.db?mode=rwc") {
		t.Fatal("relative file URI did not retain its query or configuration directory")
	}
	t.Setenv("GARMIN_OUTPUT_DSN", "legacy.db")
	t.Setenv("GARMIN_DB_URI", "override.db")
	t.Setenv("GARMIN_COOKIE_SECURE", "false")
	c, err = appconfig.Load([]string{"-config", path})
	if err != nil || c.Effective.Database.URI != "override.db" || c.File.Database.URI != "file:relative.db?mode=rwc" || c.Effective.HTTP.CookieSecure || c.Overrides["database.uri"] != "GARMIN_DB_URI" {
		t.Fatal("ENV precedence or base YAML preservation failed")
	}
	for _, contents := range []string{"version: [", "version: 999", "unknown_setting: true", "version: 1\n---\nversion: 1\n", "initialized: true\n"} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := appconfig.Load([]string{"-config", path}); err == nil {
			t.Fatalf("invalid YAML settings accepted: %q", contents)
		}
	}
	for _, uri := range []string{":memory:", "file::memory:?cache=shared", "file:temporary?mode=memory", "sqlite://relative.db"} {
		if err := appconfig.ValidateDatabase(appconfig.Database{Type: appconfig.SQLite, URI: uri}); err == nil {
			t.Fatalf("nonpersistent/invalid URI accepted: %s", uri)
		}
	}
}

func TestAdminConfigurationRequiresRestartAndRedactsConnectionSecrets(t *testing.T) {
	a, c := bootstrapApplication(t)
	setupApplication(t, a, c)
	cookie := loginApplication(t, a, testAdminPassword)
	before := appJSON[configurationResponse](t, a, "GET", "/api/v1/config", nil, cookie, 200)
	if before.RestartRequired {
		t.Fatal("fresh configuration unexpectedly requires restart")
	}
	const uri = "postgres://admin:synthetic-secret@example.invalid:5432/dives?application_name=private-option"
	w := appRequest(t, a, "PATCH", "/api/v1/config", map[string]any{"database": map[string]string{"type": "postgres", "uri": uri}}, cookie)
	if w.Code != 200 {
		t.Fatalf("config patch: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "synthetic-secret") || strings.Contains(w.Body.String(), "private-option") {
		t.Fatal("configuration API leaked connection credentials/options")
	}
	after := appJSON[configurationResponse](t, a, "GET", "/api/v1/config", nil, cookie, 200)
	if !after.RestartRequired || after.Database.Type != "sqlite" || after.PendingDatabase.Type != "postgres" {
		t.Fatal("database change switched running connection or did not flag restart")
	}
	// The active SQL database/session remains usable until restart.
	appJSON[diveTrip](t, a, "POST", "/api/v1/trips", map[string]any{"year": 2026, "country_code": "EG"}, cookie, 201)
	data, err := os.ReadFile(c.Path)
	if err != nil || !strings.Contains(string(data), "postgres://") {
		t.Fatal("new database URI was not persisted to YAML")
	}
	appJSON[httpx.ErrorResponse](t, a, "PATCH", "/api/v1/config", map[string]any{"initialized": false}, cookie, 400)
}

func TestSetupUsesEnvironmentWithoutPersistingItsSecrets(t *testing.T) {
	clearApplicationEnvironment(t)
	dir := t.TempDir()
	file := appconfig.Default()
	file.Media.Directory = filepath.Join(dir, "assets")
	const uri = "postgres://admin:synthetic-env-secret@example.invalid:5432/dives"
	t.Setenv("GARMIN_DB_TYPE", "postgres")
	t.Setenv("GARMIN_DB_URI", uri)
	c, err := appconfig.Resolve(filepath.Join(dir, "config.yaml"), file)
	if err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context, kind, dsn string) (*sqlStore, error) {
		if kind != "postgres" || dsn != uri {
			t.Error("ENV database configuration was not applied")
		}
		return openSQLStore(ctx, appconfig.SQLite, filepath.Join(dir, "actual-test.db"))
	}
	a := newApplication(context.Background(), c, testLogger(t), open)
	defer a.Close()
	appJSON[map[string]any](t, a, "POST", "/api/v1/setup", setupRequest{AdminPassword: testAdminPassword, HomeCountries: []setupHomeCountry{{CountryCode: "NC", From: "2026-01-01"}}}, nil, 201)
	waitApplicationReady(t, a)
	data, err := os.ReadFile(c.Path)
	if err != nil || strings.Contains(string(data), "synthetic-env-secret") {
		t.Fatal("ENV credentials were copied to YAML")
	}
	cookie := loginApplication(t, a, testAdminPassword)
	response := appRequest(t, a, "GET", "/api/v1/config", nil, cookie)
	if strings.Contains(response.Body.String(), "synthetic-env-secret") {
		t.Fatal("ENV credentials leaked through configuration API")
	}
	appJSON[httpx.ErrorResponse](t, a, "PATCH", "/api/v1/config", map[string]any{"database": map[string]string{"uri": "another.db"}}, cookie, 400)
}

func TestSQLOnlyOutputsAndUnconfiguredSyncFailBeforeAuthentication(t *testing.T) {
	for _, args := range [][]string{{"-output-type", "yaml"}, {"-output-type", "json"}, {"-output", "dives.yaml"}} {
		calls := 0
		err := runWithDependencies(context.Background(), args, io.Discard, testLogger(t), appDependencies{configDirectory: func() (string, error) { return t.TempDir(), nil }, openDestination: func(context.Context, appconfig.Output) (diveDestination, error) {
			calls++
			return nil, errors.New("must not open output")
		}})
		if err == nil || calls != 0 {
			t.Fatalf("legacy archive output accepted: %v", args)
		}
	}
	clearApplicationEnvironment(t)
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := Run(context.Background(), []string{"sync", "-config", filepath.Join(t.TempDir(), "config.yaml")}, input, io.Discard, testLogger(t)); !errors.Is(err, appconfig.ErrNotConfigured) {
		t.Fatalf("unconfigured sync did not stop before authentication: %v", err)
	}
}

func TestCLIAdminPasswordRecoveryUsesSQLAndRevokesSessions(t *testing.T) {
	a, c := bootstrapApplication(t)
	setupApplication(t, a, c)
	cookie := loginApplication(t, a, testAdminPassword)
	const next = "  Sea turtles near the reef  "
	var output bytes.Buffer
	err := resetAdminPasswordWithPrompt(context.Background(), []string{"-config", c.Path}, func(context.Context, string) (string, error) { return next, nil }, &output, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	appJSON[httpx.ErrorResponse](t, a, "POST", "/api/v1/trips", map[string]any{}, cookie, 401)
	loginApplication(t, a, next)
	if strings.Contains(output.String(), next) {
		t.Fatal("password recovery printed the password")
	}
}
